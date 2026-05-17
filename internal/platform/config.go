package platform

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	MySQLDSN       string
	RedisAddr      string
	RedisPassword  string
	StorageRoot    string
	DeviceToken    string
	EdgeDedup      bool
	CloudAPIAddr   string
	EdgeNodeAddr   string
	CloudUploadURL string
}

func LoadConfig() Config {
	loadDotEnv(".env")
	return Config{
		MySQLDSN:       env("MYSQL_DSN", "iot_user:iot_password@tcp(127.0.0.1:3306)/iot_vision?charset=utf8mb4&parseTime=True&loc=Local"),
		RedisAddr:      env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:  env("REDIS_PASSWORD", ""),
		StorageRoot:    cleanPath(env("STORAGE_ROOT", "./storage")),
		DeviceToken:    env("DEVICE_TOKEN", "course-demo-token"),
		EdgeDedup:      envBool("EDGE_DEDUP_ENABLED", true),
		CloudAPIAddr:   env("CLOUD_API_ADDR", ":8080"),
		EdgeNodeAddr:   env("EDGE_NODE_ADDR", ":8081"),
		CloudUploadURL: env("CLOUD_UPLOAD_URL", "http://127.0.0.1:8080/api/images/upload"),
	}
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
