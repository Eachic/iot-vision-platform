package main

import (
	"context"
	"errors"
	"image"
	"log"
	"path/filepath"
	"time"

	"iot-vision-platform/internal/platform"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

type worker struct {
	db       *gorm.DB
	redis    *redis.Client
	cfg      platform.Config
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
	ctx := context.Background()
	useRedis := true
	if err := platform.PingRedis(ctx, rdb); err != nil {
		log.Printf("redis unavailable, worker will poll MySQL queued tasks: %v", err)
		useRedis = false
	}

	w := &worker{db: db, redis: rdb, cfg: cfg, name: "worker-" + platform.NewID("node")}
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
	streams, err := w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    "image_workers",
		Consumer: w.name,
		Streams:  []string{"image_uploaded_stream", ">"},
		Count:    1,
		Block:    5 * time.Second,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			if err := w.process(ctx, msg); err != nil {
				log.Printf("process failed message=%s err=%v", msg.ID, err)
			}
			_ = w.redis.XAck(ctx, "image_uploaded_stream", "image_workers", msg.ID).Err()
		}
	}
	return nil
}

func (w *worker) process(ctx context.Context, msg redis.XMessage) error {
	imageID := value(msg, "image_id")
	originalPath := filepath.FromSlash(value(msg, "original_path"))
	originalName := value(msg, "original_name")
	if originalName == "" {
		originalName = filepath.Base(originalPath)
	}
	if imageID == "" || originalPath == "" {
		return nil
	}
	if err := w.db.Model(&platform.ImageRecord{}).Where("image_id = ?", imageID).Updates(map[string]interface{}{
		"status":        platform.StatusProcessing,
		"error_message": "",
	}).Error; err != nil {
		return err
	}

	img, format, err := platform.DecodeImage(originalPath)
	if err != nil {
		w.markFailed(imageID, err)
		return err
	}
	bounds := img.Bounds()
	thumbPath := filepath.Join(w.cfg.StorageRoot, "thumbnail", imageID+".jpg")
	if err := platform.CreateThumbnail(img, thumbPath, 360); err != nil {
		w.markFailed(imageID, err)
		return err
	}
	hash, err := platform.HashFile(originalPath)
	if err != nil {
		w.markFailed(imageID, err)
		return err
	}
	tags := w.analyzeTags(ctx, imageID, originalPath, originalName, img)
	err = w.db.Transaction(func(tx *gorm.DB) error {
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
			"thumbnail_path": filepath.ToSlash(thumbPath),
			"hash":           hash,
			"width":          bounds.Dx(),
			"height":         bounds.Dy(),
			"format":         format,
			"status":         platform.StatusCompleted,
			"error_message":  "",
		}).Error
	})
	return err
}

func (w *worker) analyzeTags(ctx context.Context, imageID string, originalPath string, originalName string, img image.Image) []platform.GeneratedTag {
	if w.analyzer == nil {
		return platform.GenerateTags(img, originalName)
	}
	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AIRPCTimeout)
	defer cancel()
	tags, err := w.analyzer.Analyze(aiCtx, platform.AnalyzeImageRequest{
		RequestID:   platform.NewID("ai_req"),
		ImageID:     imageID,
		ImageURI:    filepath.ToSlash(originalPath),
		Filename:    originalName,
		ContentType: "image/" + platform.DetectFormatFromName(originalName),
		Tasks:       []platform.AnalysisTask{{Type: "classification"}},
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

func (w *worker) markFailed(imageID string, err error) {
	_ = w.db.Model(&platform.ImageRecord{}).Where("image_id = ?", imageID).Updates(map[string]interface{}{
		"status":        platform.StatusFailed,
		"error_message": err.Error(),
	}).Error
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
