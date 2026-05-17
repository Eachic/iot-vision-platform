package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"iot-vision-platform/internal/platform"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

const maxUploadSize = 10 << 20

type app struct {
	cfg   platform.Config
	db    *gorm.DB
	redis *redis.Client
}

type imageResponse struct {
	platform.ImageRecord
	OriginalURL  string `json:"original_url"`
	ThumbnailURL string `json:"thumbnail_url"`
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
	if err := platform.PingRedis(context.Background(), rdb); err != nil {
		log.Printf("redis unavailable, uploads will rely on database polling fallback: %v", err)
		rdb = nil
	}

	a := &app{cfg: cfg, db: db, redis: rdb}
	router := gin.Default()
	router.Use(corsMiddleware())
	router.MaxMultipartMemory = maxUploadSize
	router.Static("/files/original", filepath.Join(cfg.StorageRoot, "original"))
	router.Static("/files/thumbnail", filepath.Join(cfg.StorageRoot, "thumbnail"))

	api := router.Group("/api")
	api.GET("/health", a.health)
	api.POST("/images/upload", a.uploadImage)
	api.GET("/images", a.listImages)
	api.GET("/images/:image_id", a.getImage)
	api.GET("/devices", a.listDevices)
	api.GET("/tasks/status", a.taskStatus)
	api.GET("/stats", a.stats)

	if err := router.Run(cfg.CloudAPIAddr); err != nil {
		panic(err)
	}
}

func (a *app) health(c *gin.Context) {
	sqlDB, err := a.db.DB()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	if err := sqlDB.Ping(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Headers", "Content-Type, X-Device-Token")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func (a *app) uploadImage(c *gin.Context) {
	if c.GetHeader("X-Device-Token") != a.cfg.DeviceToken {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
		return
	}
	deviceID := strings.TrimSpace(c.PostForm("device_id"))
	edgeNodeID := strings.TrimSpace(c.PostForm("edge_node_id"))
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}
	if edgeNodeID == "" {
		edgeNodeID = "edge-default"
	}
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	if file.Size > maxUploadSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is too large"})
		return
	}
	if !isAllowedImage(file) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only jpg, jpeg and png images are allowed"})
		return
	}

	imageID := platform.NewID("img")
	ext := strings.ToLower(filepath.Ext(file.Filename))
	originalPath := filepath.Join(a.cfg.StorageRoot, "original", imageID+ext)
	hashValue, size, err := saveUploadedFile(file, originalPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	capturedAt := parseTime(c.PostForm("captured_at"))
	now := time.Now()
	record := platform.ImageRecord{
		ImageID:       imageID,
		DeviceID:      deviceID,
		EdgeNodeID:    edgeNodeID,
		OriginalPath:  filepath.ToSlash(originalPath),
		ThumbnailPath: "",
		Hash:          hashValue,
		Size:          size,
		Format:        platform.DetectFormatFromName(file.Filename),
		Status:        platform.StatusQueued,
		CapturedAt:    capturedAt,
	}

	err = a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		device := platform.Device{
			DeviceID: deviceID,
			Name:     deviceID,
			Location: "demo-area",
			Status:   "online",
			LastSeen: &now,
		}
		return tx.Where("device_id = ?", deviceID).Assign(platform.Device{
			Status:   "online",
			LastSeen: &now,
		}).FirstOrCreate(&device).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := a.enqueueImage(c.Request.Context(), record); err != nil {
		_ = a.db.Model(&platform.ImageRecord{}).Where("image_id = ?", imageID).Updates(map[string]interface{}{
			"status":        platform.StatusFailed,
			"error_message": err.Error(),
		}).Error
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"image_id": imageID, "status": platform.StatusQueued})
}

func (a *app) listImages(c *gin.Context) {
	var images []platform.ImageRecord
	query := a.db.Preload("Tags")
	if v := c.Query("device_id"); v != "" {
		query = query.Where("device_id = ?", v)
	}
	if v := c.Query("status"); v != "" {
		query = query.Where("status = ?", v)
	}
	if v := c.Query("start_time"); v != "" {
		if t := parseTime(v); t != nil {
			query = query.Where("created_at >= ?", *t)
		}
	}
	if v := c.Query("end_time"); v != "" {
		if t := parseTime(v); t != nil {
			query = query.Where("created_at <= ?", *t)
		}
	}
	if v := c.Query("tag"); v != "" {
		query = query.Joins("JOIN image_tags ON image_tags.image_id = images.image_id").Where("image_tags.tag = ?", v)
	}

	page := max(1, parseInt(c.Query("page"), 1))
	pageSize := min(200, max(1, parseInt(c.Query("page_size"), 20)))
	sortBy := imageSortColumn(c.Query("sort_by"))
	sortOrder := imageSortOrder(c.Query("sort_order"))
	var total int64
	if err := query.Model(&platform.ImageRecord{}).Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := query.Order(sortBy + " " + sortOrder).Offset((page - 1) * pageSize).Limit(pageSize).Find(&images).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": mapImages(images), "total": total, "page": page, "page_size": pageSize, "sort_by": sortBy, "sort_order": sortOrder})
}

func (a *app) getImage(c *gin.Context) {
	var image platform.ImageRecord
	if err := a.db.Preload("Tags").Where("image_id = ?", c.Param("image_id")).First(&image).Error; err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, mapImage(image))
}

func (a *app) listDevices(c *gin.Context) {
	type row struct {
		platform.Device
		ImageCount int64 `json:"image_count"`
	}
	var devices []platform.Device
	if err := a.db.Order("last_seen desc").Find(&devices).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows := make([]row, 0, len(devices))
	for _, device := range devices {
		var count int64
		_ = a.db.Model(&platform.ImageRecord{}).Where("device_id = ?", device.DeviceID).Count(&count).Error
		rows = append(rows, row{Device: device, ImageCount: count})
	}
	c.JSON(http.StatusOK, gin.H{"items": rows})
}

func (a *app) taskStatus(c *gin.Context) {
	counts := map[string]int64{}
	for _, status := range []string{platform.StatusQueued, platform.StatusProcessing, platform.StatusCompleted, platform.StatusFailed} {
		var count int64
		_ = a.db.Model(&platform.ImageRecord{}).Where("status = ?", status).Count(&count).Error
		counts[status] = count
	}
	var recent []platform.ImageRecord
	if err := a.db.Preload("Tags").Order("updated_at desc").Limit(12).Find(&recent).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"counts": counts, "recent": mapImages(recent)})
}

func (a *app) stats(c *gin.Context) {
	var imageCount int64
	var deviceCount int64
	var todayCount int64
	today := time.Now().Format("2006-01-02")
	_ = a.db.Model(&platform.ImageRecord{}).Count(&imageCount).Error
	_ = a.db.Model(&platform.Device{}).Count(&deviceCount).Error
	_ = a.db.Model(&platform.ImageRecord{}).Where("created_at >= ?", today).Count(&todayCount).Error

	type tagCount struct {
		Tag   string `json:"tag"`
		Count int64  `json:"count"`
	}
	var tags []tagCount
	_ = a.db.Model(&platform.ImageTag{}).Select("tag, count(*) as count").Group("tag").Order("count desc").Limit(10).Scan(&tags).Error
	c.JSON(http.StatusOK, gin.H{"images": imageCount, "devices": deviceCount, "today": todayCount, "tags": tags})
}

func (a *app) enqueueImage(ctx context.Context, record platform.ImageRecord) error {
	if a.redis == nil {
		return nil
	}
	return a.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: "image_uploaded_stream",
		Values: map[string]interface{}{
			"image_id":      record.ImageID,
			"original_path": record.OriginalPath,
			"device_id":     record.DeviceID,
			"edge_node_id":  record.EdgeNodeID,
			"original_name": filepath.Base(record.OriginalPath),
			"thumbnail_dir": filepath.ToSlash(filepath.Join(a.cfg.StorageRoot, "thumbnail")),
		},
	}).Err()
}

func saveUploadedFile(fileHeader *multipart.FileHeader, path string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", 0, err
	}
	src, err := fileHeader.Open()
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	dst, err := os.Create(path)
	if err != nil {
		return "", 0, err
	}
	defer dst.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hash), src)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func isAllowedImage(file *multipart.FileHeader) bool {
	ext := strings.ToLower(filepath.Ext(file.Filename))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png"
}

func parseTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return &t
		}
	}
	return nil
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func imageSortColumn(value string) string {
	switch strings.TrimSpace(value) {
	case "captured_at":
		return "captured_at"
	case "updated_at":
		return "updated_at"
	case "size":
		return "size"
	case "device_id":
		return "device_id"
	default:
		return "created_at"
	}
}

func imageSortOrder(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "asc") {
		return "asc"
	}
	return "desc"
}

func mapImages(images []platform.ImageRecord) []imageResponse {
	result := make([]imageResponse, 0, len(images))
	for _, image := range images {
		result = append(result, mapImage(image))
	}
	return result
}

func mapImage(image platform.ImageRecord) imageResponse {
	return imageResponse{
		ImageRecord:  image,
		OriginalURL:  platform.PublicFileURL("original", image.OriginalPath),
		ThumbnailURL: platform.PublicFileURL("thumbnail", image.ThumbnailPath),
	}
}
