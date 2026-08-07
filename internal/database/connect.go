package database

import (
	"fmt"
	"time"

	"github.com/lihualin/tokmall/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 连接池参数：补充文档 §7 推荐初始值。
// max_conns 按单机 25 配置，压测后根据 db_pool 指标再调。
const (
	poolMaxOpenConns    = 25
	poolMaxIdleConns    = 5
	poolConnMaxLifetime = time.Hour
)

func Connect(cfg config.DBConfig) (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=%s",
		cfg.Host, cfg.User, cfg.Password, cfg.Database, cfg.Port, cfg.SSLMode,
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(poolMaxOpenConns)
	sqlDB.SetMaxIdleConns(poolMaxIdleConns)
	sqlDB.SetConnMaxLifetime(poolConnMaxLifetime)
	err = sqlDB.Ping()
	if err != nil {
		return nil, err
	}
	return db, nil
}
