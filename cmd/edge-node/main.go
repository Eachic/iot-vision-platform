package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"iot-vision-platform/internal/platform"

	"github.com/gin-gonic/gin"
)

type edgeApp struct {
	cfg       platform.Config
	seen      map[string]bool
	seenMutex sync.Mutex
}

type cachedUpload struct {
	CacheID    string    `json:"cache_id"`
	FilePath   string    `json:"file_path"`
	FileName   string    `json:"file_name"`
	DeviceID   string    `json:"device_id"`
	EdgeNodeID string    `json:"edge_node_id"`
	CapturedAt time.Time `json:"captured_at"`
}

func main() {
	cfg := platform.LoadConfig()
	if err := platform.EnsureStorage(cfg.StorageRoot); err != nil {
		panic(err)
	}
	app := &edgeApp{cfg: cfg, seen: map[string]bool{}}
	app.loadSeen()
	go app.retryLoop()

	router := gin.Default()
	router.MaxMultipartMemory = 10 << 20
	router.POST("/api/edge/upload", app.upload)
	router.GET("/api/edge/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "online", "cache_count": app.cacheCount(), "dedup_enabled": app.cfg.EdgeDedup})
	})
	if err := router.Run(cfg.EdgeNodeAddr); err != nil {
		panic(err)
	}
}

func (a *edgeApp) upload(c *gin.Context) {
	if c.GetHeader("X-Device-Token") != a.cfg.DeviceToken {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
		return
	}
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	deviceID := strings.TrimSpace(c.PostForm("device_id"))
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}
	edgeNodeID := strings.TrimSpace(c.PostForm("edge_node_id"))
	if edgeNodeID == "" {
		edgeNodeID = "edge-001"
	}
	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer src.Close()
	hash, data, err := platform.HashReader(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if a.cfg.EdgeDedup && a.wasSeen(hash) {
		c.JSON(http.StatusOK, gin.H{"status": "duplicate_filtered", "hash": hash})
		return
	}
	capturedAt := time.Now()
	if value := c.PostForm("captured_at"); value != "" {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			capturedAt = parsed
		}
	}
	if err := a.forward(data, fileHeader.Filename, deviceID, edgeNodeID, capturedAt); err != nil {
		cache, cacheErr := a.cacheUpload(data, fileHeader.Filename, deviceID, edgeNodeID, capturedAt)
		if cacheErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "cache_error": cacheErr.Error()})
			return
		}
		if a.cfg.EdgeDedup {
			a.remember(hash)
		}
		c.JSON(http.StatusAccepted, gin.H{"status": "cached", "cache_id": cache.CacheID})
		return
	}
	if a.cfg.EdgeDedup {
		a.remember(hash)
	}
	c.JSON(http.StatusOK, gin.H{"status": "forwarded", "hash": hash})
}

func (a *edgeApp) forward(data []byte, name string, deviceID string, edgeNodeID string, capturedAt time.Time) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		return err
	}
	_ = writer.WriteField("device_id", deviceID)
	_ = writer.WriteField("edge_node_id", edgeNodeID)
	_ = writer.WriteField("captured_at", capturedAt.Format(time.RFC3339))
	if err := writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, a.cfg.CloudUploadURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Device-Token", a.cfg.DeviceToken)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return &httpError{status: resp.StatusCode, body: string(msg)}
	}
	return nil
}

func (a *edgeApp) cacheUpload(data []byte, name string, deviceID string, edgeNodeID string, capturedAt time.Time) (cachedUpload, error) {
	cacheID := platform.NewID("cache")
	ext := filepath.Ext(name)
	filePath := filepath.Join(a.cfg.StorageRoot, "edge-cache", "pending", cacheID+ext)
	metaPath := filepath.Join(a.cfg.StorageRoot, "edge-cache", "meta", cacheID+".json")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return cachedUpload{}, err
	}
	cache := cachedUpload{CacheID: cacheID, FilePath: filepath.ToSlash(filePath), FileName: name, DeviceID: deviceID, EdgeNodeID: edgeNodeID, CapturedAt: capturedAt}
	raw, _ := json.MarshalIndent(cache, "", "  ")
	if err := os.WriteFile(metaPath, raw, 0644); err != nil {
		return cachedUpload{}, err
	}
	return cache, nil
}

func (a *edgeApp) retryLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.retryCached()
	}
}

func (a *edgeApp) retryCached() {
	metaDir := filepath.Join(a.cfg.StorageRoot, "edge-cache", "meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		metaPath := filepath.Join(metaDir, entry.Name())
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var cache cachedUpload
		if err := json.Unmarshal(raw, &cache); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.FromSlash(cache.FilePath))
		if err != nil {
			continue
		}
		if err := a.forward(data, cache.FileName, cache.DeviceID, cache.EdgeNodeID, cache.CapturedAt); err != nil {
			log.Printf("retry cached upload failed: %s %v", cache.CacheID, err)
			continue
		}
		_ = os.Remove(filepath.FromSlash(cache.FilePath))
		_ = os.Remove(metaPath)
		log.Printf("cached upload retried: %s", cache.CacheID)
	}
}

func (a *edgeApp) loadSeen() {
	path := filepath.Join(a.cfg.StorageRoot, "edge-cache", "seen.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			a.seen[line] = true
		}
	}
}

func (a *edgeApp) wasSeen(hash string) bool {
	a.seenMutex.Lock()
	defer a.seenMutex.Unlock()
	return a.seen[hash]
}

func (a *edgeApp) remember(hash string) {
	a.seenMutex.Lock()
	defer a.seenMutex.Unlock()
	if a.seen[hash] {
		return
	}
	a.seen[hash] = true
	path := filepath.Join(a.cfg.StorageRoot, "edge-cache", "seen.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString(hash + "\n")
}

func (a *edgeApp) cacheCount() int {
	entries, err := os.ReadDir(filepath.Join(a.cfg.StorageRoot, "edge-cache", "meta"))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return e.body
}
