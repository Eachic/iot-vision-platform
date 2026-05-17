package platform

import (
	"context"
	"io"
	"time"
)

type StorageProvider string

const (
	StorageProviderLocal  StorageProvider = "local"
	StorageProviderHuawei StorageProvider = "huawei_obs"
)

type PutObjectOptions struct {
	ContentType   string
	ContentLength int64
	Metadata      map[string]string
}

type StoredObject struct {
	Provider    StorageProvider `json:"provider"`
	Bucket      string          `json:"bucket"`
	Key         string          `json:"key"`
	ContentType string          `json:"content_type"`
	Size        int64           `json:"size"`
	URL         string          `json:"url"`
}

type Storage interface {
	Provider() StorageProvider
	Bucket() string
	Put(ctx context.Context, key string, body io.Reader, opts PutObjectOptions) (StoredObject, error)
	Get(ctx context.Context, key string) (io.ReadCloser, StoredObject, error)
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, expires time.Duration) (string, error)
}
