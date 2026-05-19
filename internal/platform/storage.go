package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func EnsureStorage(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "original"),
		filepath.Join(root, "thumbnail"),
		filepath.Join(root, "edge-cache"),
		filepath.Join(root, "edge-cache", "pending"),
		filepath.Join(root, "edge-cache", "meta"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}

func NewStorageFromConfig(cfg Config) (Storage, error) {
	switch strings.ToUpper(strings.TrimSpace(cfg.StorageProvider)) {
	case "", "LOCAL":
		return NewLocalStorage(cfg.StorageRoot, "")
	case "HUAWEI":
		return NewHuaweiStorageFromEnv()
	default:
		return nil, errors.New("unsupported STORAGE_PROVIDER: " + cfg.StorageProvider)
	}
}

func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "_" + strings.ReplaceAll(filepath.Base(os.Args[0]), ".", "")
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func HashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func HashReaderHex(reader io.Reader) (string, int64, error) {
	hash := sha256.New()
	written, err := io.Copy(hash, reader)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func HashReader(reader io.Reader) (string, []byte, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}

func PublicFileURL(kind string, path string) string {
	if path == "" {
		return ""
	}
	normalizedKind := strings.Trim(strings.ReplaceAll(kind, "\\", "/"), "/")
	normalizedPath := strings.TrimSpace(filepath.ToSlash(path))
	normalizedPath = strings.TrimLeft(normalizedPath, "/")
	prefix := normalizedKind + "/"
	if strings.HasPrefix(normalizedPath, prefix) {
		return "/files/" + normalizedKind + "/" + strings.TrimPrefix(normalizedPath, prefix)
	}
	marker := "/" + prefix
	if idx := strings.LastIndex(normalizedPath, marker); idx >= 0 {
		return "/files/" + normalizedKind + "/" + normalizedPath[idx+len(marker):]
	}
	return "/files/" + normalizedKind + "/" + filepath.Base(normalizedPath)
}
