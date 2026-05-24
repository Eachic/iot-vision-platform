package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"iot-vision-platform/internal/platform"

	mysqlDriver "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	dbRetryAttempts = 8
	dbRetryDelay    = 150 * time.Millisecond
)

var errRetryMessageLater = errors.New("retry queue message later")

type worker struct {
	db       *gorm.DB
	cfg      platform.Config
	storage  platform.Storage
	name     string
	detector *platform.GRPCVisionAnalyzer
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
	storage, err := platform.NewStorageFromConfig(cfg)
	if err != nil {
		panic(err)
	}
	log.Printf("storage provider %s bucket=%s", storage.Provider(), storage.Bucket())
	ctx := context.Background()

	w := &worker{db: db, cfg: cfg, storage: storage, name: "worker-" + platform.NewID("node")}
	if cfg.DetectionRPCEnabled {
		detector, err := platform.NewGRPCVisionAnalyzer(cfg.DetectionRPCAddr)
		if err != nil {
			log.Printf("detection rpc unavailable, worker will use plain thumbnails: %v", err)
		} else {
			w.detector = detector
			defer detector.Close()
			log.Printf("detection rpc enabled: %s", cfg.DetectionRPCAddr)
		}
	}
	log.Printf("worker started: %s", w.name)
	for {
		if err := w.drainQueuedBacklog(ctx, 20); err != nil {
			log.Printf("database queued backlog drain failed: %v", err)
		}
		if err := w.consumeRabbitMQ(ctx); err != nil {
			log.Printf("rabbitmq consume unavailable, polling MySQL queued tasks temporarily: %v", err)
			if _, pollErr := w.pollDatabaseOnce(ctx); pollErr != nil {
				log.Printf("database polling fallback failed: %v", pollErr)
			}
			time.Sleep(2 * time.Second)
		}
	}
}

func (w *worker) consumeRabbitMQ(ctx context.Context) error {
	queue, err := platform.NewRabbitMQTaskQueue(w.cfg)
	if err != nil {
		return err
	}
	defer queue.Close()
	deliveries, err := queue.ConsumeImageTasks(w.name, w.cfg.RabbitMQPrefetch)
	if err != nil {
		return err
	}
	closed := queue.NotifyClose(make(chan *amqp.Error, 1))
	log.Printf("rabbitmq consumer started queue=%s prefetch=%d", w.cfg.RabbitMQQueue, max(1, w.cfg.RabbitMQPrefetch))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case closeErr := <-closed:
			if closeErr == nil {
				return errors.New("rabbitmq connection closed")
			}
			return closeErr
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("rabbitmq delivery channel closed")
			}
			w.handleDelivery(ctx, delivery)
		}
	}
}

func (w *worker) handleDelivery(ctx context.Context, delivery amqp.Delivery) {
	task, err := platform.DecodeImageTask(delivery.Body)
	if err != nil {
		log.Printf("discard invalid rabbitmq task delivery_tag=%d err=%v", delivery.DeliveryTag, err)
		_ = delivery.Ack(false)
		return
	}
	if err := w.process(ctx, task); err != nil {
		log.Printf("process failed image=%s delivery_tag=%d err=%v", task.ImageID, delivery.DeliveryTag, err)
		if errors.Is(err, errRetryMessageLater) {
			retries := platform.RabbitMQRetryCount(delivery.Headers, w.cfg.RabbitMQQueue)
			if retries >= int64(w.cfg.RabbitMQMaxRetries) {
				_ = w.markFailed(task.ImageID, fmt.Errorf("retry limit exceeded after %d attempts: %v", retries, err))
				_ = delivery.Ack(false)
				return
			}
			_ = delivery.Nack(false, false)
			return
		}
	}
	_ = delivery.Ack(false)
}

func (w *worker) process(ctx context.Context, task platform.ImageTask) error {
	imageID := strings.TrimSpace(task.ImageID)
	originalPath := strings.TrimSpace(task.OriginalPath)
	originalName := strings.TrimSpace(task.OriginalName)
	if originalName == "" {
		originalName = filepath.Base(originalPath)
	}
	if imageID == "" || originalPath == "" {
		return nil
	}

	var record platform.ImageRecord
	if err := retryDB(func() error {
		return w.db.Where("image_id = ?", imageID).First(&record).Error
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if isRetryableDBError(err) {
			return retryMessageLater(err)
		}
		return w.failImage(imageID, err)
	}
	if record.Status == platform.StatusCompleted {
		log.Printf("skip completed image=%s", imageID)
		return nil
	}
	if err := retryDB(func() error {
		return w.db.Model(&platform.ImageRecord{}).
			Where("image_id = ? AND status <> ?", imageID, platform.StatusCompleted).
			Updates(map[string]interface{}{
				"status":        platform.StatusProcessing,
				"error_message": "",
			}).Error
	}); err != nil {
		return retryMessageLater(err)
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
	tags := platform.GenerateTags(img, originalName)
	detections := w.detectObjects(ctx, imageID, originalKey, originalName, format)
	thumbBytes, err := platform.CreateDetectionThumbnailJPEG(img, 360, detections)
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
	if err := retryDB(func() error {
		return w.completeImage(imageID, thumbKey, hash, bounds.Dx(), bounds.Dy(), size, format, tags)
	}); err != nil {
		return w.failImage(imageID, err)
	}
	return nil
}

func (w *worker) detectObjects(ctx context.Context, imageID string, originalKey string, originalName string, format string) []platform.Detection {
	if w.detector == nil {
		return nil
	}
	detectCtx, cancel := context.WithTimeout(ctx, w.cfg.DetectionRPCTimeout)
	defer cancel()
	imageURI := w.detectionImageURI(detectCtx, originalKey)
	detections, err := w.detector.Detect(detectCtx, platform.AnalyzeImageRequest{
		RequestID:   platform.NewID("det_req"),
		ImageID:     imageID,
		ImageURI:    imageURI,
		Filename:    originalName,
		ContentType: "image/" + format,
		Tasks:       []platform.AnalysisTask{{Type: "detection"}},
		Params: map[string]string{
			"storage_provider": string(w.storage.Provider()),
			"storage_key":      originalKey,
		},
	})
	if err != nil {
		log.Printf("detection failed image=%s err=%v; using plain thumbnail", imageID, err)
		return nil
	}
	if len(detections) == 0 {
		log.Printf("detection returned no boxes image=%s; using plain thumbnail", imageID)
		return nil
	}
	return detections
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

func (w *worker) drainQueuedBacklog(ctx context.Context, limit int) error {
	if limit < 1 {
		limit = 1
	}
	for i := 0; i < limit; i++ {
		handled, err := w.pollDatabaseOnce(ctx)
		if err != nil || !handled {
			return err
		}
	}
	return nil
}

func (w *worker) pollDatabaseOnce(ctx context.Context) (bool, error) {
	var images []platform.ImageRecord
	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", platform.StatusQueued).
			Order("created_at asc").
			Limit(1).
			Find(&images).Error; err != nil {
			return err
		}
		if len(images) == 0 {
			return nil
		}
		return tx.Model(&platform.ImageRecord{}).
			Where("image_id = ? AND status = ?", images[0].ImageID, platform.StatusQueued).
			Updates(map[string]interface{}{
				"status":        platform.StatusProcessing,
				"error_message": "",
			}).Error
	})
	if err != nil {
		return false, err
	}
	if len(images) == 0 {
		return false, nil
	}
	image := images[0]
	return true, w.process(ctx, platform.ImageTask{
		ImageID:      image.ImageID,
		OriginalPath: image.OriginalPath,
		DeviceID:     image.DeviceID,
		EdgeNodeID:   image.EdgeNodeID,
		OriginalName: filepath.Base(image.OriginalPath),
		ThumbnailDir: filepath.ToSlash(filepath.Join(w.cfg.StorageRoot, "thumbnail")),
	})
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

func (w *worker) detectionImageURI(ctx context.Context, originalKey string) string {
	if w.storage.Provider() == platform.StorageProviderLocal {
		return strings.TrimRight(w.cfg.PublicGatewayURL, "/") + platform.PublicFileURL("original", originalKey)
	}
	return w.aiImageURI(ctx, originalKey)
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
