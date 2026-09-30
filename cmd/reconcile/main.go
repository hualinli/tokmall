// Command reconcile 执行一次全量对账，适合放进 cron 或运维流水线：
//
//	reconcile                # 打印人类可读报告；有差异时退出码 1
//	reconcile --json         # 输出 JSON（供监控/工单系统消费）
//	reconcile --replay       # 额外重放"已发布但没有消费回执"的事件
//
// 退出码约定：0=账实一致，1=发现差异（不是"程序出错"，而是"数据需要人看"），
// 2=执行失败。把"数据不一致"和"任务失败"分开，才能让告警有意义。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/database"
	"github.com/lihualin/tokmall/internal/reconcile"
)

func main() {
	os.Exit(run())
}

func run() int {
	asJSON := flag.Bool("json", false, "以 JSON 输出报告")
	replay := flag.Bool("replay", false, "重放已发布但没有消费回执的事件")
	rebuild := flag.Bool("rebuild", false, "从事实重建作者日销投影后再对账")
	grace := flag.Duration("grace", time.Minute, "事件发布后等待消费回执的宽限期")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: load config: %v\n", err)
		return 2
	}
	db, err := database.Connect(cfg.DBConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: connect database: %v\n", err)
		return 2
	}
	sqlDB, err := db.DB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: get sql db: %v\n", err)
		return 2
	}
	defer sqlDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	runner := reconcile.NewRunner(reconcile.NewRepository(db), reconcile.WithGrace(*grace))
	if *rebuild {
		rows, err := runner.RebuildProjection(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: rebuild projection: %v\n", err)
			return 2
		}
		fmt.Printf("rebuilt_projection_rows=%d\n", rows)
	}
	// 先补偿再对账：报告要反映"修完之后"的状态，否则运维看到的是已经处理过的
	// 差异，无法判断这次运行到底有没有把问题收敛掉。
	if *replay {
		replayed, err := runner.ReplayMissingReceipts(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: replay: %v\n", err)
			return 2
		}
		fmt.Printf("replayed=%d\n", replayed)
	}
	report, err := runner.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: run: %v\n", err)
		return 2
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: encode report: %v\n", err)
			return 2
		}
	} else {
		printReport(report)
	}
	if !report.OK() {
		return 1
	}
	return 0
}

func printReport(report *reconcile.Report) {
	fmt.Printf("对账时间 %s（耗时 %s）\n", report.GeneratedAt.Format(time.RFC3339), report.Duration.Round(time.Millisecond))
	for _, check := range report.Checks {
		status := "OK"
		if !check.OK() {
			status = fmt.Sprintf("差异 %d 条", len(check.Differences))
		}
		fmt.Printf("  [%-8s] %-26s %s\n", status, check.Name, check.Description)
		for _, difference := range check.Differences {
			fmt.Printf("      %v\n", difference.Columns)
		}
	}
	if report.OK() {
		fmt.Println("结论：账实一致")
		return
	}
	fmt.Printf("结论：发现 %d 条差异，需要人工确认\n", report.TotalDifferences())
}
