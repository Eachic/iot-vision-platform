package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	return "/files/" + kind + "/" + filepath.Base(path)
}
