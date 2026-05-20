package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"iot-vision-platform/internal/platform"

	"github.com/go-redis/redis/v8"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const (
	dbRetryAttempts = 8
	dbRetryDelay    = 150 * time.Millisecond
	pendingMinIdle  = 5 * time.Minute
)

var errRetryMessageLater = errors.New("retry stream message later")

type worker struct {
	db       *gorm.DB
	redis    *redis.Client
	cfg      platform.Config
	storage  platform.Storage
	name     string
	analyzer platform.VisionAnalyzer
}

func main() {
	cfg := platform.LoadConfig()
	if err := platform.EnsureStorage(cfg.StorageRoot); err != nil {
		panic(err)
	}
	db, err := platform.OpenDB(cfg)
	if err != nil {
		panic(err)
	}
	rdb := platform.OpenRedis(cfg)
	storage, err := platform.NewStorageFromConfig(cfg)
	if err != nil {
		panic(err)
	}
	log.Printf("storage provider %s bucket=%s", storage.Provider(), storage.Bucket())
	ctx := context.Background()
	useRedis := true
	if err := platform.PingRedis(ctx, rdb); err != nil {
		log.Printf("redis unavailable, worker will poll MySQL queued tasks: %v", err)
		useRedis = false
	}

	w := &worker{db: db, redis: rdb, cfg: cfg, storage: storage, name: "worker-" + platform.NewID("node")}
	if cfg.AIRPCEnabled {
		analyzer, err := platform.NewGRPCVisionAnalyzer(cfg.AIRPCAddr)
		if err != nil {
			log.Printf("ai rpc unavailable, worker will use local tag fallback: %v", err)
		} else {
			w.analyzer = analyzer
			defer analyzer.Close()
			log.Printf("ai rpc enabled: %s", cfg.AIRPCAddr)
		}
	}
	if useRedis {
		if err := rdb.XGroupCreateMkStream(ctx, "image_uploaded_stream", "image_workers", "0").Err(); err != nil {
			if !isBusyGroup(err) {
				panic(err)
			}
		}
	}
	log.Printf("worker started: %s", w.name)
	for {
		var err error
		if useRedis {
			err = w.consume(ctx)
		} else {
			err = w.pollDatabase()
		}
		if err != nil {
			log.Printf("consume error: %v", err)
			time.Sleep(2 * time.Second)
		}
	}
}

func (w *worker) consume(ctx context.Context) error {
	if handled, err := w.consumePending(ctx); err != nil || handled {
		return err
	}
	streams, err := w.readGroup(ctx, ">")
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	w.handleStreams(ctx, streams)
	return nil
}

func (w *worker) consumePending(ctx context.Context) (bool, error) {
	pending, err := w.redis.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: "image_uploaded_stream",
		Group:  "image_workers",
		Start:  "-",
		End:    "+",
		Count:  1,
	}).Result()
	if errors.Is(err, redis.Nil) || len(pending) == 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, item := range pending {
		if item.Idle < pendingMinIdle {
			continue
		}
		messages, err := w.redis.XClaim(ctx, &redis.XClaimArgs{
			Stream:   "image_uploaded_stream",
			Group:    "image_workers",
			Consumer: w.name,
			MinIdle:  pendingMinIdle,
			Messages: []string{item.ID},
		}).Result()
		if err != nil {
			return false, err
		}
		w.handleMessages(ctx, messages)
		return true, nil
	}
	return false, nil
}

func (w *worker) readGroup(ctx context.Context, start string) ([]redis.XStream, error) {
	return w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    "image_workers",
		Consumer: w.name,
		Streams:  []string{"image_uploaded_stream", start},
		Count:    1,
		Block:    5 * time.Second,
	}).Result()
}

func (w *worker) handleStreams(ctx context.Context, streams []redis.XStream) {
	for _, stream := range streams {
		w.handleMessages(ctx, stream.Messages)
	}
}

func (w *worker) handleMessages(ctx context.Context, messages []redis.XMessage) {
	for _, msg := range messages {
		if err := w.process(ctx, msg); err != nil {
			log.Printf("process failed message=%s err=%v", msg.ID, err)
			if errors.Is(err, errRetryMessageLater) {
				continue
			}
		}
		_ = w.redis.XAck(ctx, "image_uploaded_stream", "image_workers", msg.ID).Err()
	}
}

func (w *worker) process(ctx context.Context, msg redis.XMessage) error {
	imageID := value(msg, "image_id")
	originalPath := value(msg, "original_path")
	originalName := value(msg, "original_name")
	if originalName == "" {
		originalName = filepath.Base(originalPath)
	}
	if imageID == "" || originalPath == "" {
		return nil
	}
	if err := retryDB(func() error {
		return w.db.Model(&platform.ImageRecord{}).Where("image_id = ?", imageID).Updates(map[string]interface{}{
			"status":        platform.StatusProcessing,
			"error_message": "",
		}).Error
	}); err != nil {
		return retryMessageLater(err)
	}

	var record platform.ImageRecord
	if err := retryDB(func() error {
		return w.db.Where("image_id = ?", imageID).First(&record).Error
	}); err != nil {
		return w.failImage(imageID, err)
	}
	originalKey := w.storageKey(record.OriginalPath, record.OriginalObjectKey)
	reader, _, err := w.storage.Get(ctx, originalKey)
	if err != nil {
		return w.failImage(imageID, err)
	}
	originalBytes, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil {
		return w.failImage(imageID, err)
	}
	if closeErr != nil {
		return w.failImage(imageID, closeErr)
	}
	hash, size, err := platform.HashReaderHex(bytes.NewReader(originalBytes))
	if err != nil {
		return w.failImage(imageID, err)
	}
	img, format, err := platform.DecodeImageReader(bytes.NewReader(originalBytes))
	if err != nil {
		return w.failImage(imageID, err)
	}
	bounds := img.Bounds()
	thumbBytes, err := platform.CreateThumbnailJPEG(img, 360)
	if err != nil {
		return w.failImage(imageID, err)
	}
	thumbKey := filepath.ToSlash(filepath.Join("thumbnail", imageID+".jpg"))
	if _, err := w.storage.Put(ctx, thumbKey, bytes.NewReader(thumbBytes), platform.PutObjectOptions{
		ContentType:   "image/jpeg",
		ContentLength: int64(len(thumbBytes)),
		Metadata: map[string]string{
			"image-id": imageID,
		},
	}); err != nil {
		return w.failImage(imageID, err)
	}
	tags := w.analyzeTags(ctx, imageID, originalKey, originalName, format, img)
	if err := retryDB(func() error {
		return w.completeImage(imageID, thumbKey, hash, bounds.Dx(), bounds.Dy(), size, format, tags)
	}); err != nil {
		return w.failImage(imageID, err)
	}
	return nil
}

func (w *worker) completeImage(imageID string, thumbKey string, hash string, width int, height int, size int64, format string, tags []platform.GeneratedTag) error {
	return w.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("image_id = ?", imageID).Delete(&platform.ImageTag{}).Error; err != nil {
			return err
		}
		for _, tag := range tags {
			if err := tx.Create(&platform.ImageTag{
				ImageID:    imageID,
				Tag:        tag.Tag,
				Confidence: tag.Confidence,
			}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&platform.ImageRecord{}).Where("image_id = ?", imageID).Updates(map[string]interface{}{
			"thumbnail_path": thumbKey,
			"hash":           hash,
			"width":          width,
			"height":         height,
			"size":           size,
			"format":         format,
			"status":         platform.StatusCompleted,
			"error_message":  "",
		}).Error
	})
}

func (w *worker) analyzeTags(ctx context.Context, imageID string, originalKey string, originalName string, format string, img image.Image) []platform.GeneratedTag {
	if w.analyzer == nil {
		return platform.GenerateTags(img, originalName)
	}
	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AIRPCTimeout)
	defer cancel()
	imageURI := w.aiImageURI(aiCtx, originalKey)
	tags, err := w.analyzer.Analyze(aiCtx, platform.AnalyzeImageRequest{
		RequestID:   platform.NewID("ai_req"),
		ImageID:     imageID,
		ImageURI:    imageURI,
		Filename:    originalName,
		ContentType: "image/" + format,
		Tasks:       []platform.AnalysisTask{{Type: "classification"}},
		Params: map[string]string{
			"storage_provider": string(w.storage.Provider()),
			"storage_key":      originalKey,
		},
	})
	if err != nil {
		log.Printf("ai analyze failed image=%s err=%v; fallback to local tags", imageID, err)
		return platform.GenerateTags(img, originalName)
	}
	return tags
}

func (w *worker) pollDatabase() error {
	var image platform.ImageRecord
	err := w.db.Where("status = ?", platform.StatusQueued).Order("created_at asc").First(&image).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		time.Sleep(2 * time.Second)
		return nil
	}
	if err != nil {
		return err
	}
	msg := redis.XMessage{Values: map[string]interface{}{
		"image_id":      image.ImageID,
		"original_path": image.OriginalPath,
		"device_id":     image.DeviceID,
		"edge_node_id":  image.EdgeNodeID,
		"original_name": filepath.Base(image.OriginalPath),
		"thumbnail_dir": filepath.ToSlash(filepath.Join(w.cfg.StorageRoot, "thumbnail")),
	}}
	return w.process(context.Background(), msg)
}

func (w *worker) failImage(imageID string, cause error) error {
	if err := w.markFailed(imageID, cause); err != nil {
		return retryMessageLater(err)
	}
	return cause
}

func (w *worker) markFailed(imageID string, cause error) error {
	return retryDB(func() error {
		return w.db.Model(&platform.ImageRecord{}).Where("image_id = ?", imageID).Updates(map[string]interface{}{
			"status":        platform.StatusFailed,
			"error_message": cause.Error(),
		}).Error
	})
}

func (w *worker) storageKey(pathOrKey string, objectKey string) string {
	key := strings.TrimSpace(objectKey)
	if key == "" {
		key = strings.TrimSpace(filepath.ToSlash(pathOrKey))
	}
	if w.storage.Provider() != platform.StorageProviderLocal {
		return key
	}
	root := strings.TrimRight(filepath.ToSlash(w.cfg.StorageRoot), "/") + "/"
	if strings.HasPrefix(key, root) {
		return strings.TrimPrefix(key, root)
	}
	return strings.TrimLeft(key, "/")
}

func (w *worker) aiImageURI(ctx context.Context, originalKey string) string {
	if w.storage.Provider() == platform.StorageProviderLocal {
		return filepath.ToSlash(filepath.Join(w.cfg.StorageRoot, filepath.FromSlash(originalKey)))
	}
	if publicStorage, ok := w.storage.(interface{ PublicURL(string) string }); ok {
		if publicURL := publicStorage.PublicURL(originalKey); publicURL != "" {
			return publicURL
		}
	}
	url, err := w.storage.PresignGet(ctx, originalKey, w.cfg.AIRPCTimeout+time.Minute)
	if err != nil {
		log.Printf("failed to presign original image %s for ai-service: %v", originalKey, err)
		return originalKey
	}
	return url
}

func value(msg redis.XMessage, key string) string {
	if raw, ok := msg.Values[key]; ok && raw != nil {
		return raw.(string)
	}
	return ""
}

func isBusyGroup(err error) bool {
	return err != nil && (err.Error() == "BUSYGROUP Consumer Group name already exists" || len(err.Error()) >= 9 && err.Error()[:9] == "BUSYGROUP")
}

func retryMessageLater(err error) error {
	return fmt.Errorf("%w: %v", errRetryMessageLater, err)
}

func retryDB(fn func() error) error {
	var err error
	for attempt := 1; attempt <= dbRetryAttempts; attempt++ {
		err = fn()
		if err == nil || !isRetryableDBError(err) {
			return err
		}
		time.Sleep(time.Duration(attempt) * dbRetryDelay)
	}
	return err
}

func isRetryableDBError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1213 || mysqlErr.Number == 1205
	}
	message := err.Error()
	return strings.Contains(message, "Error 1213") ||
		strings.Contains(message, "Deadlock found") ||
		strings.Contains(message, "Error 1205") ||
		strings.Contains(message, "Lock wait timeout")
}
