package platform

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MySQLDSN            string
	RedisAddr           string
	RedisPassword       string
	StorageRoot         string
	StorageProvider     string
	StorageObjectPrefix string
	DeviceToken         string
	EdgeDedup           bool
	AIRPCEnabled        bool
	AIRPCAddr           string
	AIRPCTimeout        time.Duration
	DetectionRPCEnabled bool
	DetectionRPCAddr    string
	DetectionRPCTimeout time.Duration
	PublicGatewayURL    string
	JWTSecret           string
	JWTExpireHours      int
	DefaultAdmin        string
	DefaultPass         string
	CloudAPIAddr        string
	EdgeNodeAddr        string
	CloudUploadURL      string
	MQTTEnabled         bool
	MQTTBroker          string
	MQTTTopic           string
	MQTTClientID        string
}

func LoadConfig() Config {
	loadDotEnv(".env")
	return Config{
		MySQLDSN:            env("MYSQL_DSN", "iot_user:iot_password@tcp(127.0.0.1:3306)/iot_vision?charset=utf8mb4&parseTime=True&loc=Local"),
		RedisAddr:           env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:       env("REDIS_PASSWORD", ""),
		StorageRoot:         cleanPath(env("STORAGE_ROOT", "./storage")),
		StorageProvider:     strings.ToUpper(env("STORAGE_PROVIDER", "LOCAL")),
		StorageObjectPrefix: env("STORAGE_OBJECT_PREFIX", "original"),
		DeviceToken:         env("DEVICE_TOKEN", "course-demo-token"),
		EdgeDedup:           envBool("EDGE_DEDUP_ENABLED", true),
		AIRPCEnabled:        envBool("AI_RPC_ENABLED", true),
		AIRPCAddr:           env("AI_RPC_ADDR", "127.0.0.1:9000"),
		AIRPCTimeout:        time.Duration(envInt("AI_RPC_TIMEOUT_SECONDS", 5)) * time.Second,
		DetectionRPCEnabled: envBool("DETECTION_RPC_ENABLED", false),
		DetectionRPCAddr:    env("DETECTION_RPC_ADDR", "127.0.0.1:9100"),
		DetectionRPCTimeout: time.Duration(envInt("DETECTION_RPC_TIMEOUT_SECONDS", 10)) * time.Second,
		PublicGatewayURL:    strings.TrimRight(envFirst("http://127.0.0.1:5173", "PUBLIC_GATEWAY_URL", "DETECTION_IMAGE_BASE_URL"), "/"),
		JWTSecret:           env("JWT_SECRET", "course-demo-jwt-secret"),
		JWTExpireHours:      envInt("JWT_EXPIRE_HOURS", 24),
		DefaultAdmin:        env("DEFAULT_ADMIN_USERNAME", "admin"),
		DefaultPass:         env("DEFAULT_ADMIN_PASSWORD", "admin123456"),
		CloudAPIAddr:        env("CLOUD_API_ADDR", ":8080"),
		EdgeNodeAddr:        env("EDGE_NODE_ADDR", ":8081"),
		CloudUploadURL:      env("CLOUD_UPLOAD_URL", "http://127.0.0.1:8080/api/images/upload"),
		MQTTEnabled:         envBool("MQTT_ENABLED", false),
		MQTTBroker:          env("MQTT_BROKER", "tcp://127.0.0.1:1883"),
		MQTTTopic:           env("MQTT_TOPIC", "iot/images/+"),
		MQTTClientID:        env("MQTT_CLIENT_ID", "edge-node-001"),
	}
}

func envFirst(fallback string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return fallback
}

func env(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

func cleanPath(path string) string {
	if path == "" {
		return "."
	}
	cleaned := filepath.Clean(path)
	return filepath.ToSlash(cleaned)
}
