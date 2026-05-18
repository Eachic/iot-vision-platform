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
	"os"
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

type app struct {
	cfg                   platform.Config
	db                    *gorm.DB
	redis                 *redis.Client
	originalMirror        platform.Storage
	originalMirrorInitErr error
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

	originalMirror, originalMirrorInitErr := initOriginalMirror(cfg)
	a := &app{cfg: cfg, db: db, redis: rdb, originalMirror: originalMirror, originalMirrorInitErr: originalMirrorInitErr}
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

func initOriginalMirror(cfg platform.Config) (platform.Storage, error) {
	switch strings.ToUpper(strings.TrimSpace(cfg.StorageProvider)) {
	case "", "LOCAL":
		log.Printf("storage provider LOCAL: originals will only be saved locally")
		return nil, nil
	case "HUAWEI":
		storage, err := platform.NewHuaweiStorageFromEnv()
		if err != nil {
			log.Printf("storage provider HUAWEI configured but OBS mirror is unavailable: %v", err)
			return nil, err
		}
		log.Printf("storage provider HUAWEI: originals will be mirrored to bucket %s", storage.Bucket())
		return storage, nil
	default:
		panic("unsupported STORAGE_PROVIDER: " + cfg.StorageProvider)
	}
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
	a.mirrorOriginal(c.Request.Context(), &record, originalPath, ext, size)

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

func (a *app) mirrorOriginal(ctx context.Context, record *platform.ImageRecord, originalPath string, ext string, size int64) {
	if strings.ToUpper(strings.TrimSpace(a.cfg.StorageProvider)) != "HUAWEI" {
		return
	}
	record.OriginalStorageProvider = string(platform.StorageProviderHuawei)
	if a.originalMirrorInitErr != nil {
		record.OriginalStorageError = a.originalMirrorInitErr.Error()
		return
	}
	if a.originalMirror == nil {
		record.OriginalStorageError = "huawei storage mirror is not initialized"
		return
	}
	file, err := os.Open(originalPath)
	if err != nil {
		record.OriginalStorageError = err.Error()
		return
	}
	defer file.Close()

	objectKey := originalObjectKey(a.cfg.StorageObjectPrefix, record.ImageID, ext, time.Now())
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	stored, err := a.originalMirror.Put(ctx, objectKey, file, platform.PutObjectOptions{
		ContentType:   contentType,
		ContentLength: size,
		Metadata: map[string]string{
			"image-id":     record.ImageID,
			"device-id":    record.DeviceID,
			"edge-node-id": record.EdgeNodeID,
		},
	})
	if err != nil {
		record.OriginalStorageError = err.Error()
		log.Printf("failed to mirror original image %s to HUAWEI OBS: %v", record.ImageID, err)
		return
	}
	record.OriginalStorageProvider = string(stored.Provider)
	record.OriginalBucket = stored.Bucket
	record.OriginalObjectKey = stored.Key
	record.OriginalObjectURL = stored.URL
	record.OriginalStorageError = ""
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

func originalObjectKey(prefix string, imageID string, ext string, t time.Time) string {
	cleanPrefix := strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/ ")
	if cleanPrefix == "" {
		cleanPrefix = "original"
	}
	return filepath.ToSlash(filepath.Join(cleanPrefix, t.Format("2006"), t.Format("01"), t.Format("02"), imageID+ext))
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
