package main

import (
	"bytes"
	"encoding/base64"
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

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gin-gonic/gin"
)

type edgeApp struct {
	cfg       platform.Config
	seen      map[string]bool
	seenMutex sync.Mutex
	mqtt      mqtt.Client
}

type cachedUpload struct {
	CacheID    string    `json:"cache_id"`
	FilePath   string    `json:"file_path"`
	FileName   string    `json:"file_name"`
	DeviceID   string    `json:"device_id"`
	EdgeNodeID string    `json:"edge_node_id"`
	CapturedAt time.Time `json:"captured_at"`
}

type incomingUpload struct {
	Data        []byte
	FileName    string
	ContentType string
	DeviceID    string
	EdgeNodeID  string
	CapturedAt  time.Time
}

type uploadOutcome struct {
	Status  string
	Hash    string
	CacheID string
}

type mqttUploadMessage struct {
	DeviceID    string `json:"device_id"`
	EdgeNodeID  string `json:"edge_node_id"`
	CapturedAt  string `json:"captured_at"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Token       string `json:"token"`
	ImageBase64 string `json:"image_base64"`
}

func main() {
	cfg := platform.LoadConfig()
	if err := platform.EnsureStorage(cfg.StorageRoot); err != nil {
		panic(err)
	}
	app := &edgeApp{cfg: cfg, seen: map[string]bool{}}
	app.loadSeen()
	go app.retryLoop()
	if cfg.MQTTEnabled {
		go app.startMQTTConsumer()
	}

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
	data, err := io.ReadAll(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	capturedAt := parseCapturedAt(c.PostForm("captured_at"))
	outcome, err := a.handleIncomingUpload(incomingUpload{
		Data:        data,
		FileName:    fileHeader.Filename,
		ContentType: fileHeader.Header.Get("Content-Type"),
		DeviceID:    deviceID,
		EdgeNodeID:  edgeNodeID,
		CapturedAt:  capturedAt,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if outcome.Status == "cached" {
		c.JSON(http.StatusAccepted, gin.H{"status": outcome.Status, "hash": outcome.Hash, "cache_id": outcome.CacheID})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": outcome.Status, "hash": outcome.Hash})
}

func (a *edgeApp) handleIncomingUpload(upload incomingUpload) (uploadOutcome, error) {
	hash, data, err := platform.HashReader(bytes.NewReader(upload.Data))
	if err != nil {
		return uploadOutcome{}, err
	}
	if a.cfg.EdgeDedup && a.wasSeen(hash) {
		return uploadOutcome{Status: "duplicate_filtered", Hash: hash}, nil
	}
	if upload.EdgeNodeID == "" {
		upload.EdgeNodeID = "edge-001"
	}
	if upload.CapturedAt.IsZero() {
		upload.CapturedAt = time.Now()
	}
	if err := a.forward(data, upload.FileName, upload.DeviceID, upload.EdgeNodeID, upload.CapturedAt); err != nil {
		cache, cacheErr := a.cacheUpload(data, upload.FileName, upload.DeviceID, upload.EdgeNodeID, upload.CapturedAt)
		if cacheErr != nil {
			return uploadOutcome{}, &cacheError{forwardErr: err, cacheErr: cacheErr}
		}
		if a.cfg.EdgeDedup {
			a.remember(hash)
		}
		return uploadOutcome{Status: "cached", Hash: hash, CacheID: cache.CacheID}, nil
	}
	if a.cfg.EdgeDedup {
		a.remember(hash)
	}
	return uploadOutcome{Status: "forwarded", Hash: hash}, nil
}

func parseCapturedAt(value string) time.Time {
	if value == "" {
		return time.Now()
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Now()
	}
	return parsed
}

func (a *edgeApp) startMQTTConsumer() {
	opts := mqtt.NewClientOptions().
		AddBroker(a.cfg.MQTTBroker).
		SetClientID(a.cfg.MQTTClientID).
		SetAutoReconnect(true).
		SetConnectRetry(true)
	opts.OnConnect = func(client mqtt.Client) {
		token := client.Subscribe(a.cfg.MQTTTopic, 1, a.handleMQTTMessage)
		token.Wait()
		if err := token.Error(); err != nil {
			log.Printf("mqtt subscribe failed topic=%s err=%v", a.cfg.MQTTTopic, err)
			return
		}
		log.Printf("mqtt subscribed broker=%s topic=%s client_id=%s", a.cfg.MQTTBroker, a.cfg.MQTTTopic, a.cfg.MQTTClientID)
	}
	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		log.Printf("mqtt connection lost: %v", err)
	}
	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("mqtt connect failed broker=%s err=%v", a.cfg.MQTTBroker, err)
		return
	}
	a.mqtt = client
}

func (a *edgeApp) handleMQTTMessage(_ mqtt.Client, msg mqtt.Message) {
	var payload mqttUploadMessage
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		log.Printf("mqtt invalid json topic=%s err=%v", msg.Topic(), err)
		return
	}
	if payload.Token != a.cfg.DeviceToken {
		log.Printf("mqtt invalid token topic=%s device=%s", msg.Topic(), payload.DeviceID)
		return
	}
	if strings.TrimSpace(payload.DeviceID) == "" {
		log.Printf("mqtt missing device_id topic=%s", msg.Topic())
		return
	}
	if strings.TrimSpace(payload.ImageBase64) == "" {
		log.Printf("mqtt missing image_base64 topic=%s device=%s", msg.Topic(), payload.DeviceID)
		return
	}
	data, err := base64.StdEncoding.DecodeString(payload.ImageBase64)
	if err != nil {
		log.Printf("mqtt invalid image_base64 topic=%s device=%s err=%v", msg.Topic(), payload.DeviceID, err)
		return
	}
	fileName := strings.TrimSpace(payload.Filename)
	if fileName == "" {
		fileName = "mqtt_upload.jpg"
	}
	outcome, err := a.handleIncomingUpload(incomingUpload{
		Data:        data,
		FileName:    fileName,
		ContentType: payload.ContentType,
		DeviceID:    strings.TrimSpace(payload.DeviceID),
		EdgeNodeID:  strings.TrimSpace(payload.EdgeNodeID),
		CapturedAt:  parseCapturedAt(payload.CapturedAt),
	})
	if err != nil {
		log.Printf("mqtt upload failed topic=%s device=%s err=%v", msg.Topic(), payload.DeviceID, err)
		return
	}
	log.Printf("mqtt upload handled topic=%s device=%s status=%s hash=%s cache_id=%s", msg.Topic(), payload.DeviceID, outcome.Status, outcome.Hash, outcome.CacheID)
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

type cacheError struct {
	forwardErr error
	cacheErr   error
}

func (e *cacheError) Error() string {
	return e.forwardErr.Error() + "; cache_error=" + e.cacheErr.Error()
}
