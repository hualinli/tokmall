// Package reconcile 用"可执行的检查"回答一个面试常问的问题：
// 你怎么知道数据是对的？
//
// 每个 Check 都是一条能返回**差异行**的 SQL，而不是"看起来没问题"的断言：
// 库存余额与流水是否守恒、订单与流水是否一一对应、退款单与订单状态是否
// 一致、事件驱动投影与实时聚合是否收敛、已发布事件是否有消费回执。
//
// 差异行会被原样输出（谁、差多少），因此对账报告既能告警也能直接指导修复；
// 对账任务本身不做任何"自动改数"，唯一的写操作是把"已发布但没有消费回执"
// 的事件重新放回 Outbox（重放是幂等的，因为消费侧按 event_id 去重）。
package reconcile

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Difference 是一行差异。Columns 保留原始列值，便于定位到具体业务对象。
type Difference struct {
	Columns map[string]any `json:"columns"`
}

// CheckResult 是单个检查的结果。
type CheckResult struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Differences []Difference `json:"differences"`
}

func (r CheckResult) OK() bool { return len(r.Differences) == 0 }

// Report 是一次对账的完整结果。OK 表示所有检查都没有差异。
type Report struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Duration    time.Duration `json:"duration_ms"`
	Checks      []CheckResult `json:"checks"`
}

func (r *Report) OK() bool {
	for _, check := range r.Checks {
		if !check.OK() {
			return false
		}
	}
	return true
}

// TotalDifferences 汇总差异条数，用于指标与退出码。
func (r *Report) TotalDifferences() int {
	total := 0
	for _, check := range r.Checks {
		total += len(check.Differences)
	}
	return total
}

// check 是内部定义：一条 SQL + 元信息。
type check struct {
	name        string
	description string
	query       string
	args        []any
}

// checks 是全部对账项。顺序刻意按"资金 → 库存 → 履约 → 投影 → 事件"排列：
// 越靠前的差异越严重（资金与库存错了就是事故）。
func checks(grace time.Duration) []check {
	return []check{
		{
			name:        "inventory_balance",
			description: "库存余额必须等于流水累计变更（期初为 0，所有变更都有流水）",
			query: `SELECT i.sku_id, i.quantity, COALESCE(sum(t.change), 0) AS ledger_total
				FROM inventory i
				LEFT JOIN inventory_transactions t ON t.sku_id = i.sku_id
				GROUP BY i.sku_id, i.quantity
				HAVING i.quantity <> COALESCE(sum(t.change), 0)`,
		},
		{
			name:        "order_stock_ledger",
			description: "每个非待支付订单恰好有一条下单流水，且回补流水条数与终态匹配",
			query: `SELECT o.id AS order_id, o.status,
					count(*) FILTER (WHERE t.reason = 'ORDER_CREATED') AS created_ledgers,
					count(*) FILTER (WHERE t.reason = 'ORDER_CANCELED') AS canceled_ledgers,
					count(*) FILTER (WHERE t.reason = 'ORDER_EXPIRED') AS expired_ledgers,
					count(*) FILTER (WHERE t.reason = 'ORDER_REFUNDED') AS refunded_ledgers
				FROM orders o
				LEFT JOIN inventory_transactions t ON t.order_id = o.id
				WHERE o.status <> 'PENDING_PAYMENT'
				GROUP BY o.id, o.status, o.refund_from_status
				HAVING count(*) FILTER (WHERE t.reason = 'ORDER_CREATED') <> 1
					OR (o.status = 'CANCELED' AND count(*) FILTER (WHERE t.reason = 'ORDER_CANCELED') <> 1)
					OR (o.status = 'EXPIRED' AND count(*) FILTER (WHERE t.reason = 'ORDER_EXPIRED') <> 1)
					OR (o.status = 'REFUNDED' AND o.refund_from_status = 'PAID'
						AND count(*) FILTER (WHERE t.reason = 'ORDER_REFUNDED') <> 1)
					OR (o.status = 'REFUNDED' AND o.refund_from_status = 'FULFILLING'
						AND count(*) FILTER (WHERE t.reason = 'ORDER_REFUNDED') <> 0)
					OR (o.status NOT IN ('CANCELED', 'EXPIRED', 'REFUNDED')
						AND count(*) FILTER (WHERE t.reason IN ('ORDER_CANCELED', 'ORDER_EXPIRED', 'ORDER_REFUNDED')) <> 0)`,
		},
		{
			name:        "refund_order_consistency",
			description: "订单的售后状态必须与退款单一致，且退款金额等于订单金额与成功支付金额",
			query: `SELECT o.id AS order_id, o.status, o.total_amount,
					COALESCE(r.status, 'NONE') AS refund_status, COALESCE(r.amount, 0) AS refund_amount,
					COALESCE(p.paid_amount, 0) AS paid_amount
				FROM orders o
				LEFT JOIN refunds r ON r.order_id = o.id AND r.status IN ('REQUESTED', 'SUCCEEDED')
				LEFT JOIN LATERAL (
					SELECT sum(amount) AS paid_amount FROM payments
					WHERE order_id = o.id AND status = 'SUCCESS'
				) p ON TRUE
				WHERE (o.status = 'REFUND_REQUESTED' AND COALESCE(r.status, '') <> 'REQUESTED')
					OR (o.status = 'REFUNDED' AND COALESCE(r.status, '') <> 'SUCCEEDED')
					OR (o.status NOT IN ('REFUND_REQUESTED', 'REFUNDED') AND r.id IS NOT NULL)
					OR (r.id IS NOT NULL AND r.amount <> o.total_amount)
					OR (r.id IS NOT NULL AND COALESCE(p.paid_amount, 0) <> o.total_amount)`,
		},
		{
			name:        "order_payment_consistency",
			description: "已支付订单必须有且仅有一笔成功支付，未支付订单不得有成功支付",
			query: `SELECT o.id AS order_id, o.status, count(p.id) FILTER (WHERE p.status = 'SUCCESS') AS success_payments
				FROM orders o
				LEFT JOIN payments p ON p.order_id = o.id
				GROUP BY o.id, o.status
				HAVING (o.status IN ('PAID', 'FULFILLING', 'COMPLETED', 'REFUND_REQUESTED', 'REFUNDED')
						AND count(p.id) FILTER (WHERE p.status = 'SUCCESS') <> 1)
					OR (o.status IN ('PENDING_PAYMENT', 'CANCELED', 'EXPIRED')
						AND count(p.id) FILTER (WHERE p.status = 'SUCCESS') <> 0)`,
		},
		{
			// 口径必须与投影完全一致，否则对账本身就会变成噪音源。
			// 这里第一次跑就抓到过真实差异：检查用"净额"（排除已退款订单），
			// 投影用"毛额 + 单独记退款"，两边对不上。最终把口径写成规则：
			//   order_count / units_sold / gmv      = 曾支付过的全部订单（含后来退款的）
			//   refunded_orders / refunded_amount   = 按退款发生日归集的退款
			// 这样"毛额"永远是历史事实，"净额"由使用方自己相减得到。
			name:        "author_sales_projection",
			description: "事件驱动投影必须与订单事实一致（毛额与退款分列口径）",
			query: `WITH live AS (
					SELECT o.source_author_id AS author_id,
						date_trunc('day', o.paid_at)::date AS stat_date,
						count(*) AS order_count,
						COALESCE(sum(i.quantity), 0) AS units_sold,
						COALESCE(sum(o.total_amount), 0) AS gmv
					FROM orders o
					JOIN order_items i ON i.order_id = o.id
					WHERE o.source_author_id IS NOT NULL
						AND o.paid_at IS NOT NULL
						AND o.status IN ('PAID', 'FULFILLING', 'COMPLETED', 'REFUND_REQUESTED', 'REFUNDED')
					GROUP BY 1, 2
				), live_refunds AS (
					SELECT o.source_author_id AS author_id,
						date_trunc('day', r.refunded_at)::date AS stat_date,
						count(*) AS refunded_orders,
						COALESCE(sum(r.amount), 0) AS refunded_amount
					FROM refunds r
					JOIN orders o ON o.id = r.order_id
					WHERE r.status = 'SUCCEEDED' AND o.source_author_id IS NOT NULL
					GROUP BY 1, 2
				), projected AS (
					SELECT author_id, stat_date, order_count, units_sold, gmv, refunded_orders, refunded_amount
					FROM author_sales_daily
				)
				SELECT COALESCE(l.author_id, lr.author_id, p.author_id) AS author_id,
					COALESCE(l.stat_date, lr.stat_date, p.stat_date) AS stat_date,
					COALESCE(l.order_count, 0) AS live_orders, COALESCE(p.order_count, 0) AS projected_orders,
					COALESCE(l.units_sold, 0) AS live_units, COALESCE(p.units_sold, 0) AS projected_units,
					COALESCE(l.gmv, 0) AS live_gmv, COALESCE(p.gmv, 0) AS projected_gmv,
					COALESCE(lr.refunded_orders, 0) AS live_refunded_orders,
					COALESCE(p.refunded_orders, 0) AS projected_refunded_orders,
					COALESCE(lr.refunded_amount, 0) AS live_refunded_amount,
					COALESCE(p.refunded_amount, 0) AS projected_refunded_amount
				FROM live l
				FULL OUTER JOIN live_refunds lr ON lr.author_id = l.author_id AND lr.stat_date = l.stat_date
				FULL OUTER JOIN projected p
					ON p.author_id = COALESCE(l.author_id, lr.author_id)
					AND p.stat_date = COALESCE(l.stat_date, lr.stat_date)
				WHERE COALESCE(l.order_count, 0) <> COALESCE(p.order_count, 0)
					OR COALESCE(l.units_sold, 0) <> COALESCE(p.units_sold, 0)
					OR COALESCE(l.gmv, 0) <> COALESCE(p.gmv, 0)
					OR COALESCE(lr.refunded_orders, 0) <> COALESCE(p.refunded_orders, 0)
					OR COALESCE(lr.refunded_amount, 0) <> COALESCE(p.refunded_amount, 0)`,
		},
		{
			name:        "outbox_delivery",
			description: "已发布事件必须在宽限期内拿到消费回执，否则说明事件可能已丢失",
			query: `SELECT e.event_id, e.event_type, e.aggregate_id, e.published_at
				FROM outbox_events e
				LEFT JOIN consumed_events c ON c.event_id = e.event_id
				WHERE e.status = 'PUBLISHED'
					AND c.event_id IS NULL
					AND e.published_at < now() - (? * interval '1 second')`,
			args: []any{int(grace.Seconds())},
		},
	}
}

// Repository 是对账用的只读查询入口（定义在消费侧）。
type Repository interface {
	RunCheck(ctx context.Context, name, description, query string, args []any) (CheckResult, error)
	// MissingReceipts 返回"已发布但没有消费回执且已超过宽限期"的事件 ID。
	MissingReceipts(ctx context.Context, grace time.Duration) ([]string, error)
	// RequeueEvents 把指定事件重新放回待投递（重放，event_id 不变）。
	RequeueEvents(ctx context.Context, eventIDs []string, now time.Time) (int, error)
	// RebuildProjection 从事实（orders/order_items/refunds）重建作者日销投影。
	RebuildProjection(ctx context.Context) (int64, error)
}

type GormRepository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *GormRepository { return &GormRepository{db: db} }

// maxDifferencesPerCheck 限制单个检查返回的差异行数：对账报告要能读，
// 指标要能报警；全量差异留给 SQL 手工排查。
const maxDifferencesPerCheck = 50

func (r *GormRepository) RunCheck(ctx context.Context, name, description, query string, args []any) (CheckResult, error) {
	result := CheckResult{Name: name, Description: description, Differences: []Difference{}}
	rows, err := r.db.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return result, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return result, err
	}
	for rows.Next() {
		if len(result.Differences) >= maxDifferencesPerCheck {
			break
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return result, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = normalizeValue(values[i])
		}
		result.Differences = append(result.Differences, Difference{Columns: row})
	}
	return result, rows.Err()
}

// normalizeValue 把 []byte（部分驱动返回的文本）转成字符串，
// 否则 JSON 报告里会出现 base64 编码，人工排查很痛苦。
func normalizeValue(value any) any {
	if raw, ok := value.([]byte); ok {
		return string(raw)
	}
	return value
}

func (r *GormRepository) MissingReceipts(ctx context.Context, grace time.Duration) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).Raw(`
		SELECT e.event_id::text
		FROM outbox_events e
		LEFT JOIN consumed_events c ON c.event_id = e.event_id
		WHERE e.status = 'PUBLISHED'
			AND c.event_id IS NULL
			AND e.published_at < now() - (? * interval '1 second')`,
		int(grace.Seconds())).Scan(&ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// RebuildProjection 从订单事实重建整个读模型。
//
// 这是 CQRS 读模型的关键能力：投影只是事实的一种物化形式，任何时候都可以
// 丢弃并重建。因此"投影漂移"是可修复的运维问题，而不是数据损坏——
// 前提是事实侧（orders/refunds/流水）始终是权威。
//
// 重建使用与投影完全相同的口径（毛额 + 按退款日归集退款），
// 并且是幂等的：重复执行结果相同。
func (r *GormRepository) RebuildProjection(ctx context.Context) (int64, error) {
	var affected int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM author_sales_daily`).Error; err != nil {
			return err
		}
		result := tx.Exec(`
			INSERT INTO author_sales_daily
				(author_id, stat_date, order_count, units_sold, gmv, refunded_orders, refunded_amount, updated_at)
			SELECT author_id, stat_date,
				sum(order_count), sum(units_sold), sum(gmv), sum(refunded_orders), sum(refunded_amount), now()
			FROM (
				SELECT o.source_author_id AS author_id,
					date_trunc('day', o.paid_at)::date AS stat_date,
					count(*) AS order_count,
					COALESCE(sum(i.quantity), 0) AS units_sold,
					COALESCE(sum(o.total_amount), 0) AS gmv,
					0::bigint AS refunded_orders,
					0::bigint AS refunded_amount
				FROM orders o
				JOIN order_items i ON i.order_id = o.id
				WHERE o.source_author_id IS NOT NULL
					AND o.paid_at IS NOT NULL
					AND o.status IN ('PAID', 'FULFILLING', 'COMPLETED', 'REFUND_REQUESTED', 'REFUNDED')
				GROUP BY 1, 2
				UNION ALL
				SELECT o.source_author_id AS author_id,
					date_trunc('day', r.refunded_at)::date AS stat_date,
					0::bigint, 0::bigint, 0::bigint,
					count(*), COALESCE(sum(r.amount), 0)
				FROM refunds r
				JOIN orders o ON o.id = r.order_id
				WHERE r.status = 'SUCCEEDED' AND o.source_author_id IS NOT NULL
				GROUP BY 1, 2
			) parts
			GROUP BY author_id, stat_date`)
		if result.Error != nil {
			return result.Error
		}
		affected = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

func (r *GormRepository) RequeueEvents(ctx context.Context, eventIDs []string, now time.Time) (int, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	res := r.db.WithContext(ctx).Exec(`
		UPDATE outbox_events
		SET status = 'PENDING', available_at = ?, leased_until = NULL, attempts = 0, published_at = NULL, last_error = NULL
		WHERE event_id::text IN ? AND status = 'PUBLISHED'`, now.UTC(), eventIDs)
	if res.Error != nil {
		return 0, res.Error
	}
	return int(res.RowsAffected), nil
}
