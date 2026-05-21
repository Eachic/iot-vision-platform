package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"iot-vision-platform/internal/platform"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const maxUploadSize = 10 << 20

const (
	cacheKeyStats       = "cache:cloud-api:stats"
	cacheKeyTasksStatus = "cache:cloud-api:tasks_status"
	cacheKeyDevices     = "cache:cloud-api:devices"
	cacheKeyImagesFirst = "cache:cloud-api:images:first_page"

	statsCacheTTL       = 5 * time.Second
	tasksStatusCacheTTL = 2 * time.Second
	devicesCacheTTL     = 10 * time.Second
	imagesFirstCacheTTL = 2 * time.Second

	staleProcessingTimeout = 10 * time.Minute
)

type app struct {
	cfg     platform.Config
	db      *gorm.DB
	redis   *redis.Client
	storage platform.Storage
}

type imageResponse struct {
	platform.ImageRecord
	OriginalURL  string `json:"original_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID       uint64 `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type tokenClaims struct {
	UserID   uint64 `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Exp      int64  `json:"exp"`
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

	storage, err := platform.NewStorageFromConfig(cfg)
	if err != nil {
		panic(err)
	}
	log.Printf("storage provider %s bucket=%s", storage.Provider(), storage.Bucket())
	a := &app{cfg: cfg, db: db, redis: rdb, storage: storage}
	if err := a.ensureDefaultAdmin(); err != nil {
		panic(err)
	}
	router := gin.Default()
	router.Use(corsMiddleware())
	router.MaxMultipartMemory = maxUploadSize
	router.Static("/files/original", filepath.Join(cfg.StorageRoot, "original"))
	router.Static("/files/thumbnail", filepath.Join(cfg.StorageRoot, "thumbnail"))

	api := router.Group("/api")
	api.GET("/health", a.health)
	api.POST("/auth/login", a.login)
	api.POST("/images/upload", a.uploadImage)
	protected := api.Group("")
	protected.Use(a.authMiddleware())
	protected.GET("/auth/me", a.me)
	protected.GET("/images", a.listImages)
	protected.GET("/images/:image_id", a.getImage)
	protected.GET("/devices", a.listDevices)
	protected.GET("/tasks/status", a.taskStatus)
	protected.GET("/stats", a.stats)

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

func (a *app) ensureDefaultAdmin() error {
	username := strings.TrimSpace(a.cfg.DefaultAdmin)
	password := strings.TrimSpace(a.cfg.DefaultPass)
	if username == "" || password == "" {
		return nil
	}
	var count int64
	if err := a.db.Model(&platform.User{}).Where("username = ?", username).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return a.db.Create(&platform.User{
		Username:     username,
		PasswordHash: string(hash),
		Role:         "admin",
	}).Error
}

func (a *app) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid login request"})
		return
	}
	username := strings.TrimSpace(req.Username)
	password := strings.TrimSpace(req.Password)
	if username == "" || password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
		return
	}
	var user platform.User
	if err := a.db.Where("username = ?", username).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
		return
	}
	token, err := a.signToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user": mapUser(user)})
}

func (a *app) me(c *gin.Context) {
	user, ok := currentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": mapUser(user)})
}

func (a *app) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := a.verifyToken(strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		var user platform.User
		if err := a.db.Where("id = ? AND username = ?", claims.UserID, claims.Username).First(&user).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid user"})
			return
		}
		c.Set("current_user", user)
		c.Next()
	}
}

func (a *app) signToken(user platform.User) (string, error) {
	expiresAt := time.Now().Add(time.Duration(max(1, a.cfg.JWTExpireHours)) * time.Hour).Unix()
	claims := tokenClaims{UserID: user.ID, Username: user.Username, Role: user.Role, Exp: expiresAt}
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	headerRaw, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsRaw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerRaw) + "." + base64.RawURLEncoding.EncodeToString(claimsRaw)
	return unsigned + "." + a.tokenSignature(unsigned), nil
}

func (a *app) verifyToken(token string) (tokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenClaims{}, errors.New("invalid token format")
	}
	unsigned := parts[0] + "." + parts[1]
	expected := a.tokenSignature(unsigned)
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return tokenClaims{}, errors.New("invalid token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}, err
	}
	var claims tokenClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return tokenClaims{}, err
	}
	if claims.Exp <= time.Now().Unix() {
		return tokenClaims{}, errors.New("token expired")
	}
	return claims, nil
}

func (a *app) tokenSignature(unsigned string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.JWTSecret))
	_, _ = mac.Write([]byte(unsigned))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func currentUser(c *gin.Context) (platform.User, bool) {
	raw, ok := c.Get("current_user")
	if !ok {
		return platform.User{}, false
	}
	user, ok := raw.(platform.User)
	return user, ok
}

func mapUser(user platform.User) userResponse {
	return userResponse{ID: user.ID, Username: user.Username, Role: user.Role}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Headers", "Content-Type, X-Device-Token, Authorization")
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
	objectPrefix := a.cfg.StorageObjectPrefix
	if a.storage.Provider() == platform.StorageProviderLocal {
		objectPrefix = "original"
	}
	objectKey := originalObjectKey(objectPrefix, imageID, ext, time.Now())
	stored, hashValue, size, err := saveUploadedObject(c.Request.Context(), a.storage, file, objectKey, contentTypeForExt(ext), map[string]string{
		"image-id":     imageID,
		"device-id":    deviceID,
		"edge-node-id": edgeNodeID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	capturedAt := parseTime(c.PostForm("captured_at"))
	now := time.Now()
	record := platform.ImageRecord{
		ImageID:                 imageID,
		DeviceID:                deviceID,
		EdgeNodeID:              edgeNodeID,
		OriginalPath:            stored.Key,
		ThumbnailPath:           "",
		OriginalStorageProvider: string(stored.Provider),
		OriginalBucket:          stored.Bucket,
		OriginalObjectKey:       stored.Key,
		OriginalObjectURL:       stored.URL,
		Hash:                    hashValue,
		Size:                    size,
		Format:                  platform.DetectFormatFromName(file.Filename),
		Status:                  platform.StatusQueued,
		CapturedAt:              capturedAt,
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

	a.invalidateDashboardCache(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"image_id": imageID, "status": platform.StatusQueued})
}

func (a *app) listImages(c *gin.Context) {
	hasFilter := false
	query := a.db.Preload("Tags")
	if v := strings.TrimSpace(c.Query("device_id")); v != "" {
		hasFilter = true
		query = query.Where("device_id = ?", v)
	}
	if v := strings.TrimSpace(c.Query("status")); v != "" {
		hasFilter = true
		query = query.Where("status = ?", v)
	}
	if v := strings.TrimSpace(c.Query("start_time")); v != "" {
		hasFilter = true
		if t := parseTime(v); t != nil {
			query = query.Where("created_at >= ?", *t)
		}
	}
	if v := strings.TrimSpace(c.Query("end_time")); v != "" {
		hasFilter = true
		if t := parseTime(v); t != nil {
			query = query.Where("created_at <= ?", *t)
		}
	}
	if v := strings.TrimSpace(c.Query("tag")); v != "" {
		hasFilter = true
		query = query.Joins("JOIN image_tags ON image_tags.image_id = images.image_id").Where("image_tags.tag = ?", v)
	}

	page := max(1, parseInt(c.Query("page"), 1))
	pageSize := min(200, max(1, parseInt(c.Query("page_size"), 20)))
	sortBy := imageSortColumn(c.Query("sort_by"))
	sortOrder := imageSortOrder(c.Query("sort_order"))
	if isDefaultImagesQuery(hasFilter, page, pageSize, sortBy, sortOrder) {
		a.cachedJSON(c, cacheKeyImagesFirst, imagesFirstCacheTTL, func(ctx context.Context) (gin.H, error) {
			return a.queryImages(ctx, query, page, pageSize, sortBy, sortOrder)
		})
		return
	}
	payload, err := a.queryImages(c.Request.Context(), query, page, pageSize, sortBy, sortOrder)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (a *app) queryImages(ctx context.Context, query *gorm.DB, page int, pageSize int, sortBy string, sortOrder string) (gin.H, error) {
	var images []platform.ImageRecord
	var total int64
	if err := query.Model(&platform.ImageRecord{}).Count(&total).Error; err != nil {
		return nil, err
	}
	if err := query.Order(sortBy + " " + sortOrder).Offset((page - 1) * pageSize).Limit(pageSize).Find(&images).Error; err != nil {
		return nil, err
	}
	return gin.H{"items": a.mapImages(ctx, images), "total": total, "page": page, "page_size": pageSize, "sort_by": sortBy, "sort_order": sortOrder}, nil
}

func isDefaultImagesQuery(hasFilter bool, page int, pageSize int, sortBy string, sortOrder string) bool {
	return !hasFilter && page == 1 && pageSize == 60 && sortBy == "created_at" && sortOrder == "desc"
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
	c.JSON(http.StatusOK, a.mapImage(c.Request.Context(), image))
}

func (a *app) listDevices(c *gin.Context) {
	a.cachedJSON(c, cacheKeyDevices, devicesCacheTTL, func(ctx context.Context) (gin.H, error) {
		type imageCount struct {
			DeviceID string
			Count    int64
		}
		type row struct {
			platform.Device
			ImageCount int64 `json:"image_count"`
		}
		var devices []platform.Device
		if err := a.db.Order("last_seen desc").Find(&devices).Error; err != nil {
			return nil, err
		}
		var counts []imageCount
		if err := a.db.Model(&platform.ImageRecord{}).Select("device_id, count(*) as count").Group("device_id").Scan(&counts).Error; err != nil {
			return nil, err
		}
		countByDevice := make(map[string]int64, len(counts))
		for _, count := range counts {
			countByDevice[count.DeviceID] = count.Count
		}
		rows := make([]row, 0, len(devices))
		for _, device := range devices {
			rows = append(rows, row{Device: device, ImageCount: countByDevice[device.DeviceID]})
		}
		return gin.H{"items": rows}, nil
	})
}

func (a *app) taskStatus(c *gin.Context) {
	a.cachedJSON(c, cacheKeyTasksStatus, tasksStatusCacheTTL, func(ctx context.Context) (gin.H, error) {
		if err := a.reconcileStaleProcessingTasks(ctx); err != nil {
			log.Printf("reconcile stale processing tasks failed: %v", err)
		}
		counts := map[string]int64{}
		for _, status := range []string{platform.StatusQueued, platform.StatusProcessing, platform.StatusCompleted, platform.StatusFailed} {
			var count int64
			if err := a.db.Model(&platform.ImageRecord{}).Where("status = ?", status).Count(&count).Error; err != nil {
				return nil, err
			}
			counts[status] = count
		}
		var recent []platform.ImageRecord
		if err := a.db.Preload("Tags").Order("updated_at desc").Limit(12).Find(&recent).Error; err != nil {
			return nil, err
		}
		return gin.H{"counts": counts, "recent": a.mapImages(ctx, recent)}, nil
	})
}

func (a *app) reconcileStaleProcessingTasks(ctx context.Context) error {
	deadline := time.Now().Add(-staleProcessingTimeout)
	result := a.db.WithContext(ctx).Model(&platform.ImageRecord{}).
		Where("status = ? AND updated_at < ?", platform.StatusProcessing, deadline).
		Updates(map[string]interface{}{
			"status":        platform.StatusFailed,
			"error_message": "processing timeout, worker may have stopped before finishing",
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		log.Printf("marked stale processing tasks as failed count=%d", result.RowsAffected)
	}
	return nil
}

func (a *app) stats(c *gin.Context) {
	a.cachedJSON(c, cacheKeyStats, statsCacheTTL, func(ctx context.Context) (gin.H, error) {
		var imageCount int64
		var deviceCount int64
		var todayCount int64
		today := time.Now().Format("2006-01-02")
		if err := a.db.Model(&platform.ImageRecord{}).Count(&imageCount).Error; err != nil {
			return nil, err
		}
		if err := a.db.Model(&platform.Device{}).Count(&deviceCount).Error; err != nil {
			return nil, err
		}
		if err := a.db.Model(&platform.ImageRecord{}).Where("created_at >= ?", today).Count(&todayCount).Error; err != nil {
			return nil, err
		}

		type tagCount struct {
			Tag   string `json:"tag"`
			Count int64  `json:"count"`
		}
		var tags []tagCount
		if err := a.db.Model(&platform.ImageTag{}).Select("tag, count(*) as count").Group("tag").Order("count desc").Limit(10).Scan(&tags).Error; err != nil {
			return nil, err
		}
		return gin.H{"images": imageCount, "devices": deviceCount, "today": todayCount, "tags": tags}, nil
	})
}

func (a *app) cachedJSON(c *gin.Context, key string, ttl time.Duration, producer func(context.Context) (gin.H, error)) {
	ctx := c.Request.Context()
	if a.redis != nil {
		raw, err := a.redis.Get(ctx, key).Bytes()
		if err == nil {
			c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
			return
		}
		if !errors.Is(err, redis.Nil) {
			log.Printf("redis cache get failed key=%s err=%v", key, err)
		}
	}

	payload, err := producer(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if a.redis != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			log.Printf("json marshal cache payload failed key=%s err=%v", key, err)
		} else if err := a.redis.Set(ctx, key, raw, ttl).Err(); err != nil {
			log.Printf("redis cache set failed key=%s err=%v", key, err)
		}
	}
	c.JSON(http.StatusOK, payload)
}

func (a *app) invalidateDashboardCache(ctx context.Context) {
	if a.redis == nil {
		return
	}
	if err := a.redis.Del(ctx, cacheKeyStats, cacheKeyTasksStatus, cacheKeyDevices, cacheKeyImagesFirst).Err(); err != nil {
		log.Printf("redis cache invalidate failed: %v", err)
	}
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

func saveUploadedObject(ctx context.Context, storage platform.Storage, fileHeader *multipart.FileHeader, key string, contentType string, metadata map[string]string) (platform.StoredObject, string, int64, error) {
	src, err := fileHeader.Open()
	if err != nil {
		return platform.StoredObject{}, "", 0, err
	}
	defer src.Close()

	hash := sha256.New()
	body := io.TeeReader(src, hash)
	stored, err := storage.Put(ctx, key, body, platform.PutObjectOptions{
		ContentType:   contentType,
		ContentLength: fileHeader.Size,
		Metadata:      metadata,
	})
	if err != nil {
		return platform.StoredObject{}, "", 0, err
	}
	size := stored.Size
	if size <= 0 {
		size = fileHeader.Size
	}
	return stored, hex.EncodeToString(hash.Sum(nil)), size, nil
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

func originalObjectKey(prefix string, imageID string, ext string, t time.Time) string {
	cleanPrefix := strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/ ")
	if cleanPrefix == "" {
		cleanPrefix = "original"
	}
	return filepath.ToSlash(filepath.Join(cleanPrefix, t.Format("2006"), t.Format("01"), t.Format("02"), imageID+ext))
}

func contentTypeForExt(ext string) string {
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

func (a *app) mapImages(ctx context.Context, images []platform.ImageRecord) []imageResponse {
	result := make([]imageResponse, 0, len(images))
	for _, image := range images {
		result = append(result, a.mapImage(ctx, image))
	}
	return result
}

func (a *app) mapImage(ctx context.Context, image platform.ImageRecord) imageResponse {
	return imageResponse{
		ImageRecord:  image,
		OriginalURL:  a.objectURL(ctx, "original", image.OriginalPath, image.OriginalObjectKey, image.OriginalObjectURL),
		ThumbnailURL: a.objectURL(ctx, "thumbnail", image.ThumbnailPath, image.ThumbnailPath, ""),
	}
}

func (a *app) objectURL(ctx context.Context, kind string, pathOrKey string, objectKey string, storedURL string) string {
	if storedURL != "" {
		return normalizeStoredURL(storedURL)
	}
	key := strings.TrimSpace(objectKey)
	if key == "" {
		key = strings.TrimSpace(pathOrKey)
	}
	if key == "" {
		return ""
	}
	if a.storage == nil || a.storage.Provider() == platform.StorageProviderLocal {
		return platform.PublicFileURL(kind, key)
	}
	if publicStorage, ok := a.storage.(interface{ PublicURL(string) string }); ok {
		if publicURL := publicStorage.PublicURL(key); publicURL != "" {
			return publicURL
		}
	}
	url, err := a.storage.PresignGet(ctx, key, time.Hour)
	if err != nil {
		log.Printf("failed to presign %s object %s: %v", kind, key, err)
		return ""
	}
	return url
}

func normalizeStoredURL(rawURL string) string {
	value := strings.TrimSpace(rawURL)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" {
		return value
	}
	if strings.Contains(value, ".") && !strings.HasPrefix(value, "/") {
		return "https://" + strings.TrimLeft(value, "/")
	}
	return value
}
