package platform

import "time"

const (
	StatusQueued     = "queued"
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

type Device struct {
	ID        uint64     `gorm:"primaryKey" json:"id"`
	DeviceID  string     `gorm:"size:64;uniqueIndex;not null" json:"device_id"`
	Name      string     `gorm:"size:128;not null" json:"name"`
	Location  string     `gorm:"size:128;not null" json:"location"`
	Status    string     `gorm:"size:32;not null;index" json:"status"`
	LastSeen  *time.Time `json:"last_seen"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type ImageRecord struct {
	ID            uint64     `gorm:"primaryKey" json:"id"`
	ImageID       string     `gorm:"size:64;uniqueIndex;not null" json:"image_id"`
	DeviceID      string     `gorm:"size:64;not null;index" json:"device_id"`
	EdgeNodeID    string     `gorm:"size:64;not null" json:"edge_node_id"`
	OriginalPath  string     `gorm:"size:512;not null" json:"original_path"`
	ThumbnailPath string     `gorm:"size:512;not null" json:"thumbnail_path"`
	Hash          string     `gorm:"size:128;not null;index" json:"hash"`
	Width         int        `gorm:"not null;default:0" json:"width"`
	Height        int        `gorm:"not null;default:0" json:"height"`
	Size          int64      `gorm:"not null;default:0" json:"size"`
	Format        string     `gorm:"size:32;not null" json:"format"`
	Status        string     `gorm:"size:32;not null;index" json:"status"`
	ErrorMessage  string     `gorm:"type:text" json:"error_message"`
	CapturedAt    *time.Time `gorm:"index" json:"captured_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Tags          []ImageTag `gorm:"foreignKey:ImageID;references:ImageID" json:"tags"`
}

func (ImageRecord) TableName() string {
	return "images"
}

type ImageTag struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	ImageID    string    `gorm:"size:64;not null;index" json:"image_id"`
	Tag        string    `gorm:"size:64;not null;index" json:"tag"`
	Confidence float64   `gorm:"not null;default:0" json:"confidence"`
	CreatedAt  time.Time `json:"created_at"`
}

type User struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"size:64;uniqueIndex;not null" json:"username"`
	PasswordHash string    `gorm:"size:128;not null" json:"-"`
	Role         string    `gorm:"size:32;not null;index" json:"role"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func AutoMigrate(db Migrator) error {
	return db.AutoMigrate(&Device{}, &ImageRecord{}, &ImageTag{}, &User{})
}

type Migrator interface {
	AutoMigrate(dst ...interface{}) error
}
