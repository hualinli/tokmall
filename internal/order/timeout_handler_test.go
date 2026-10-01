package order

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lihualin/tokmall/internal/outbox"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 超时延迟事件的单元测试（不需要数据库）：
// 只验证"载荷解码 + 命名空间声明 + 无效输入处理"这些与存储无关的部分。
// 真实的关单语义（条件更新、库存只回补一次、与支付竞争）由
// refund_postgres_integration_test.go / postgres_integration_test.go 覆盖。

func TestTimeoutHandlerDeclaresEventType(t *testing.T) {
	handler := NewTimeoutHandler(nil)
	require.Equal(t, []string{outbox.EventTypeOrderTimeout}, handler.EventTypes())
}

func TestTimeoutHandlerRejectsBrokenPayload(t *testing.T) {
	handler := NewTimeoutHandler(nil)
	event := outbox.Event{
		EventID:   uuid.New(),
		EventType: outbox.EventTypeOrderTimeout,
		Payload:   json.RawMessage(`{"order_id":`), // 截断的 JSON
	}
	err := handler.Handle(context.Background(), nil, event)
	require.Error(t, err, "无法解析的载荷必须报错，让消息重投并最终进 DLQ")
}

func TestTimeoutHandlerRejectsInvalidOrderID(t *testing.T) {
	handler := NewTimeoutHandler(nil)
	payload, err := json.Marshal(outbox.OrderTimeoutPayload{OrderID: 0})
	require.NoError(t, err)
	event := outbox.Event{EventType: outbox.EventTypeOrderTimeout, Payload: payload}
	require.Error(t, handler.Handle(context.Background(), nil, event))
}

type recordingStats struct {
	results []string
}

func (s *recordingStats) TimeoutEvent(result string) { s.results = append(s.results, result) }

var _ = gorm.DB{}
var _ = time.Now
