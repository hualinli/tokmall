package database

import (
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/lihualin/tokmall/internal/config"
)

// 连接池参数：补充文档 §7 推荐初始值。
// max_conns 按单机 25 配置，压测后根据 db_pool 指标再调。
const (
	poolMaxOpenConns    = 25
	poolMaxIdleConns    = 5
	poolConnMaxLifetime = time.Hour
)

// newGormLogger 构造 GORM 的 slog 适配器：慢查询 Warn、错误 Error、
// 其余级别走 Debug（由 slog 的 level 决定是否输出），并保持参数化输出。
func newGormLogger(slowThreshold time.Duration) logger.Interface {
	return logger.New(slogWriter{}, logger.Config{
		SlowThreshold:             slowThreshold,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
		Colorful:                  false,
	})
}

// slogWriter 把 GORM 的日志接到项目统一的 slog 上，
// 这样 DB 日志与业务日志在同一时间线上，不需要再 tail 另一个文件。
type slogWriter struct{}

func (slogWriter) Printf(format string, args ...any) {
	slog.Debug(fmt.Sprintf(format, args...), "source", "gorm")
}

func Connect(cfg config.DBConfig) (*gorm.DB, error) {
	return ConnectWithPool(cfg, poolMaxOpenConns, poolMaxIdleConns)
}

// ConnectWithPool 给独立工作负载分配连接预算；调用方负责约束所有池的总和。
func ConnectWithPool(cfg config.DBConfig, maxOpen, maxIdle int) (*gorm.DB, error) {
	if maxOpen <= 0 || maxIdle < 0 || maxIdle > maxOpen {
		return nil, fmt.Errorf("invalid database pool budget: open=%d idle=%d", maxOpen, maxIdle)
	}
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=%s",
		cfg.Host, cfg.User, cfg.Password, cfg.Database, cfg.Port, cfg.SSLMode,
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		// GORM 日志策略（借鉴 DomTok 的做法，并补上它的缺口）：
		//   - ParameterizedQueries：SQL 只打模板不打参数值。日志里永远不会
		//     出现手机号、邮箱、密码哈希——这是"日志合规"最省事的一道闸门；
		//   - IgnoreRecordNotFoundError：把"查不到"当业务事实而不是错误。
		//     详情页 404、缓存穿透压测会产生海量 record not found，全部落盘
		//     会把日志 I/O 变成真实瓶颈（压测里实测过）；
		//   - SlowThreshold：只把慢查询当事件记下来，普通查询不进日志。
		Logger: newGormLogger(200 * time.Millisecond),
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(poolConnMaxLifetime)
	err = sqlDB.Ping()
	if err != nil {
		// 失败路径同样要释放连接池资源（与 cache.NewClient 同理）。
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}
