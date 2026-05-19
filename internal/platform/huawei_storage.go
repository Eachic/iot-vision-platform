package platform

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	obs "github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
)

type HuaweiStorageConfig struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string
	Endpoint      string
	Bucket        string
	PublicURL     string
}

type HuaweiStorage struct {
	client    *obs.ObsClient
	bucket    string
	publicURL string
}

func NewHuaweiStorage(cfg HuaweiStorageConfig) (*HuaweiStorage, error) {
	if strings.TrimSpace(cfg.AccessKey) == "" {
		return nil, errors.New("huawei OBS access key is required")
	}
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("huawei OBS secret key is required")
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("huawei OBS endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("huawei OBS bucket is required")
	}
	var (
		client *obs.ObsClient
		err    error
	)
	if strings.TrimSpace(cfg.SecurityToken) != "" {
		client, err = obs.New(cfg.AccessKey, cfg.SecretKey, cfg.Endpoint, obs.WithSecurityToken(cfg.SecurityToken))
	} else {
		client, err = obs.New(cfg.AccessKey, cfg.SecretKey, cfg.Endpoint)
	}
	if err != nil {
		return nil, err
	}
	return &HuaweiStorage{
		client:    client,
		bucket:    cfg.Bucket,
		publicURL: normalizePublicURL(cfg.PublicURL),
	}, nil
}

func NewHuaweiStorageFromEnv() (*HuaweiStorage, error) {
	cfg := HuaweiStorageConfig{
		AccessKey:     env("HUAWEI_OBS_ACCESS_KEY", ""),
		SecretKey:     env("HUAWEI_OBS_SECRET_KEY", ""),
		SecurityToken: env("HUAWEI_OBS_SECURITY_TOKEN", ""),
		Endpoint:      env("HUAWEI_OBS_ENDPOINT", ""),
		Bucket:        env("HUAWEI_OBS_BUCKET", ""),
		PublicURL:     env("HUAWEI_OBS_PUBLIC_URL", ""),
	}
	return NewHuaweiStorage(cfg)
}

func (s *HuaweiStorage) Provider() StorageProvider {
	return StorageProviderHuawei
}

func (s *HuaweiStorage) Bucket() string {
	return s.bucket
}

func (s *HuaweiStorage) Put(ctx context.Context, key string, body io.Reader, opts PutObjectOptions) (StoredObject, error) {
	if err := ctx.Err(); err != nil {
		return StoredObject{}, err
	}
	cleanKey, err := normalizeObjectKey(key)
	if err != nil {
		return StoredObject{}, err
	}
	input := &obs.PutObjectInput{
		PutObjectBasicInput: obs.PutObjectBasicInput{
			ObjectOperationInput: obs.ObjectOperationInput{
				Bucket:   s.bucket,
				Key:      cleanKey,
				Metadata: opts.Metadata,
			},
			HttpHeader: obs.HttpHeader{
				ContentType: opts.ContentType,
			},
			ContentLength: opts.ContentLength,
		},
		Body: body,
	}
	if _, err := s.client.PutObject(input); err != nil {
		return StoredObject{}, err
	}
	return StoredObject{
		Provider:    s.Provider(),
		Bucket:      s.Bucket(),
		Key:         cleanKey,
		ContentType: opts.ContentType,
		Size:        opts.ContentLength,
		URL:         s.publicObjectURL(cleanKey),
	}, nil
}

func (s *HuaweiStorage) Get(ctx context.Context, key string) (io.ReadCloser, StoredObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, StoredObject{}, err
	}
	cleanKey, err := normalizeObjectKey(key)
	if err != nil {
		return nil, StoredObject{}, err
	}
	output, err := s.client.GetObject(&obs.GetObjectInput{
		GetObjectMetadataInput: obs.GetObjectMetadataInput{
			Bucket: s.bucket,
			Key:    cleanKey,
		},
	})
	if err != nil {
		return nil, StoredObject{}, err
	}
	return output.Body, StoredObject{
		Provider:    s.Provider(),
		Bucket:      s.Bucket(),
		Key:         cleanKey,
		ContentType: output.ContentType,
		Size:        output.ContentLength,
		URL:         s.publicObjectURL(cleanKey),
	}, nil
}

func (s *HuaweiStorage) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanKey, err := normalizeObjectKey(key)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(&obs.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    cleanKey,
	})
	return err
}

func (s *HuaweiStorage) PresignGet(ctx context.Context, key string, expires time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cleanKey, err := normalizeObjectKey(key)
	if err != nil {
		return "", err
	}
	if expires <= 0 {
		expires = time.Hour
	}
	output, err := s.client.CreateSignedUrl(&obs.CreateSignedUrlInput{
		Method:  obs.HttpMethodGet,
		Bucket:  s.bucket,
		Key:     cleanKey,
		Expires: int(expires.Seconds()),
	})
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}

func (s *HuaweiStorage) PublicURL(key string) string {
	cleanKey, err := normalizeObjectKey(key)
	if err != nil {
		return ""
	}
	return s.publicObjectURL(cleanKey)
}

func (s *HuaweiStorage) publicObjectURL(key string) string {
	if s.publicURL == "" {
		return ""
	}
	return strings.TrimRight(s.publicURL, "/") + "/" + strings.TrimLeft(key, "/")
}

func normalizePublicURL(rawURL string) string {
	value := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" {
		return value
	}
	return "https://" + strings.TrimLeft(value, "/")
}

func normalizeObjectKey(key string) (string, error) {
	cleanKey := strings.TrimSpace(strings.ReplaceAll(key, "\\", "/"))
	cleanKey = strings.TrimLeft(cleanKey, "/")
	for strings.Contains(cleanKey, "//") {
		cleanKey = strings.ReplaceAll(cleanKey, "//", "/")
	}
	if cleanKey == "" || cleanKey == "." || strings.Contains(cleanKey, "../") || strings.HasPrefix(cleanKey, "..") {
		return "", errors.New("invalid object key")
	}
	return cleanKey, nil
}
