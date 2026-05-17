package platform

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type LocalStorage struct {
	root      string
	bucket    string
	publicURL string
}

func NewLocalStorage(root string, publicURL string) (*LocalStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("local storage root is required")
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	return &LocalStorage{
		root:      filepath.Clean(root),
		bucket:    "local",
		publicURL: strings.TrimRight(publicURL, "/"),
	}, nil
}

func (s *LocalStorage) Provider() StorageProvider {
	return StorageProviderLocal
}

func (s *LocalStorage) Bucket() string {
	return s.bucket
}

func (s *LocalStorage) Put(ctx context.Context, key string, body io.Reader, opts PutObjectOptions) (StoredObject, error) {
	if err := ctx.Err(); err != nil {
		return StoredObject{}, err
	}
	cleanKey, fullPath, err := s.resolveKey(key)
	if err != nil {
		return StoredObject{}, err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return StoredObject{}, err
	}
	file, err := os.Create(fullPath)
	if err != nil {
		return StoredObject{}, err
	}
	written, copyErr := io.Copy(file, body)
	closeErr := file.Close()
	if copyErr != nil {
		return StoredObject{}, copyErr
	}
	if closeErr != nil {
		return StoredObject{}, closeErr
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(cleanKey))
	}
	return StoredObject{
		Provider:    s.Provider(),
		Bucket:      s.Bucket(),
		Key:         cleanKey,
		ContentType: contentType,
		Size:        written,
		URL:         s.publicObjectURL(cleanKey),
	}, nil
}

func (s *LocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, StoredObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, StoredObject{}, err
	}
	cleanKey, fullPath, err := s.resolveKey(key)
	if err != nil {
		return nil, StoredObject{}, err
	}
	file, err := os.Open(fullPath)
	if err != nil {
		return nil, StoredObject{}, err
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, StoredObject{}, err
	}
	contentType := mime.TypeByExtension(filepath.Ext(cleanKey))
	return file, StoredObject{
		Provider:    s.Provider(),
		Bucket:      s.Bucket(),
		Key:         cleanKey,
		ContentType: contentType,
		Size:        stat.Size(),
		URL:         s.publicObjectURL(cleanKey),
	}, nil
}

func (s *LocalStorage) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, fullPath, err := s.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *LocalStorage) PresignGet(ctx context.Context, key string, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cleanKey, _, err := s.resolveKey(key)
	if err != nil {
		return "", err
	}
	if s.publicURL == "" {
		return "", errors.New("local storage public URL is not configured")
	}
	return s.publicObjectURL(cleanKey), nil
}

func (s *LocalStorage) resolveKey(key string) (string, string, error) {
	cleanKey := strings.TrimSpace(filepath.ToSlash(key))
	cleanKey = path.Clean("/" + cleanKey)
	cleanKey = strings.TrimPrefix(cleanKey, "/")
	if cleanKey == "" || cleanKey == "." {
		return "", "", errors.New("object key is required")
	}
	fullPath := filepath.Join(s.root, filepath.FromSlash(cleanKey))
	rootAbs, err := filepath.Abs(s.root)
	if err != nil {
		return "", "", err
	}
	fullAbs, err := filepath.Abs(fullPath)
	if err != nil {
		return "", "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(os.PathSeparator)) {
		return "", "", errors.New("object key escapes local storage root")
	}
	return cleanKey, fullAbs, nil
}

func (s *LocalStorage) publicObjectURL(key string) string {
	if s.publicURL == "" {
		return ""
	}
	escaped := strings.ReplaceAll(url.PathEscape(key), "%2F", "/")
	return s.publicURL + "/" + escaped
}
