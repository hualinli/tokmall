package user

import (
	"time"

	"gorm.io/gorm"
)

// UserStatus 是用户可用性状态。DISABLED 表示运维停用：不能登录、不能续签，
// 公开资料按"不存在"处理（与 interaction 的可见性规则保持一致）。
type UserStatus string

const (
	UserStatusActive   UserStatus = "ACTIVE"
	UserStatusDisabled UserStatus = "DISABLED"
	UserStatusDeleted  UserStatus = "DELETED"
)

// User 对应 users 表，schema 以 migrations/*.sql 为准。
// role 列由数据库默认值维护，业务侧暂不需要读写，故不映射为字段。
type User struct {
	UserID       int64  `gorm:"column:id;primaryKey"`
	Email        string `gorm:"uniqueIndex"`
	Username     string `gorm:"uniqueIndex"`
	PasswordHash string
	// default:ACTIVE 让 GORM 在零值时省略该列、交给数据库默认值。
	// 没有它会踩一个很隐蔽的坑：映射字段之后，Create 会把空字符串写进
	// status（而不是使用 DEFAULT 'ACTIVE'），于是新注册用户既不是 ACTIVE
	// 也不是 DISABLED，登录直接被"账号不可用"挡掉。
	// 这里是第二道防线：第一道是 Register 显式写入 ACTIVE，
	// 第三道是迁移 000018 的 users_status_check 约束。
	Status    UserStatus     `gorm:"column:status;default:ACTIVE"`
	AvatarKey *string        `gorm:"type:text"`
	Bio       string         `gorm:"type:text"`
	CreatedAt time.Time      `gorm:"autoCreateTime"`
	UpdatedAt time.Time      `gorm:"autoUpdateTime"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

func (User) TableName() string {
	return "users"
}

// Usable 判断账号是否可以登录/续签/被他人看到。
func (u *User) Usable() bool {
	return u != nil && u.Status == UserStatusActive
}
