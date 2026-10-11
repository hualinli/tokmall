// Command outboxctl 是 Outbox 的运维入口：查看积压、检查 DLQ、人工重放。
//
// 为什么需要一个独立命令而不是"再写个管理接口"：
//   - 这些操作是低频、需要审计的运维动作，放在 HTTP 上会扩大攻击面；
//   - 它们必须在"应用已经挂了"的时候仍然可用（只依赖数据库和 Redis）；
//   - 命令行天然带操作者上下文（谁在什么机器上执行了什么）。
//
// 用法：
//
//	outboxctl status                 # 积压与 DLQ 概览
//	outboxctl dlq list --limit 20    # 查看死信事件
//	outboxctl dlq requeue <event_id> # 重放单条（event_id 不变，消费侧仍会去重）
//	outboxctl dlq requeue --all      # 重放全部死信
//	outboxctl drain                  # 手动投递一批（中继停摆时的应急手段）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lihualin/tokmall/internal/cache"
	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/database"
	"github.com/lihualin/tokmall/internal/outbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "outboxctl: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: outboxctl <status|dlq|drain> [flags]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.Connect(cfg.DBConfig)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	repo := outbox.NewRepository(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch os.Args[1] {
	case "status":
		return status(ctx, repo)
	case "dlq":
		return dlq(ctx, repo, os.Args[2:])
	case "drain":
		return drain(ctx, cfg, repo)
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func status(ctx context.Context, repo *outbox.GormRepository) error {
	stats, err := repo.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("pending_due=%d pending_delayed=%d dead=%d oldest_pending_age=%s\n",
		stats.PendingDue, stats.PendingDelayed, stats.Dead, stats.OldestPendingAge.Round(time.Second))
	if stats.Dead > 0 {
		fmt.Println("存在死信事件：用 `outboxctl dlq list` 查看，确认后可 `dlq requeue`")
	}
	return nil
}

func dlq(ctx context.Context, repo *outbox.GormRepository, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: outboxctl dlq <list|requeue>")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("dlq list", flag.ContinueOnError)
		limit := fs.Int("limit", 20, "最多显示多少条")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		events, err := repo.ListDead(ctx, *limit)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			fmt.Println("没有死信事件")
			return nil
		}
		for _, event := range events {
			lastError := ""
			if event.LastError != nil {
				lastError = *event.LastError
			}
			fmt.Printf("%s type=%s aggregate=%s/%d attempts=%d created=%s error=%s\n",
				event.EventID, event.EventType, event.AggregateType, event.AggregateID,
				event.Attempts, event.CreatedAt.Format(time.RFC3339), lastError)
		}
		return nil
	case "requeue":
		fs := flag.NewFlagSet("dlq requeue", flag.ContinueOnError)
		all := fs.Bool("all", false, "重放全部死信")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		targets := fs.Args()
		if !*all && len(targets) == 0 {
			return fmt.Errorf("usage: outboxctl dlq requeue <event_id>... | --all")
		}
		if *all {
			events, err := repo.ListDead(ctx, 1000)
			if err != nil {
				return err
			}
			for _, event := range events {
				targets = append(targets, event.EventID.String())
			}
		}
		requeued := 0
		for _, eventID := range targets {
			ok, err := repo.Requeue(ctx, eventID, time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "requeue %s failed: %v\n", eventID, err)
				continue
			}
			if ok {
				requeued++
			}
		}
		fmt.Printf("requeued=%d\n", requeued)
		return nil
	default:
		return fmt.Errorf("unknown dlq command %q", args[0])
	}
}

// drain 手动投递一批：中继进程停摆（例如部署故障）时的应急手段。
// 它使用与生产完全相同的中继逻辑，因此不会产生第二条投递路径。
func drain(ctx context.Context, cfg *config.Config, repo *outbox.GormRepository) error {
	client, err := cache.NewClient(cfg.RedisConfig)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	publisher := outbox.NewRedisStreamPublisher(client, cfg.OutboxStream, cfg.OutboxStreamMaxLen,
		outbox.WithRequiredConsumerGroup(cfg.OutboxConsumerGroup))
	relay := outbox.NewRelay(repo, publisher, cfg.OutboxRelayInterval, cfg.OutboxRelayBatchSize,
		cfg.OutboxRelayLease, int32(cfg.OutboxMaxAttempts))
	processed, err := relay.RunOnce(ctx)
	if err != nil {
		return err
	}
	stats, err := repo.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("processed=%d pending_due=%d pending_delayed=%d dead=%d\n",
		processed, stats.PendingDue, stats.PendingDelayed, stats.Dead)
	return nil
}

// 保留：便于将来增加 "event inspect" 子命令时直接打印完整载荷。
