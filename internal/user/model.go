package user

import (
	"time"

	"gorm.io/gorm"
)

// User 对应 users 表，schema 以 migrations/*.sql 为准。
// role/status 列由数据库默认值维护，业务侧暂不需要读写，故不映射为字段。
type User struct {
	UserID       int64  `gorm:"column:id;primaryKey"`
	Email        string `gorm:"uniqueIndex"`
	Username     string `gorm:"uniqueIndex"`
	PasswordHash string
	AvatarKey    *string        `gorm:"type:text"`
	Bio          string         `gorm:"type:text"`
	CreatedAt    time.Time      `gorm:"autoCreateTime"`
	UpdatedAt    time.Time      `gorm:"autoUpdateTime"`
	DeletedAt    gorm.DeletedAt `gorm:"index"`
}

func (User) TableName() string {
	return "users"
}
