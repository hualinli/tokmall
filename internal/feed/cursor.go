package feed

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CurrentRankVersion 当前排序规则版本（时间排序 v1）。排序规则变化时递增，
// 旧游标因版本不匹配而失效（客户端重新请求第一页）。
// 版本校验在 service 层（codec 只验签名与格式）：游标携带的版本必须与
// 当前装配的 Ranker.Version() 一致，见 service.List/ListFollowing。
const CurrentRankVersion = "time-v1"

var ErrInvalidCursor = errors.New("cursor malformed, tampered or expired")

// Cursor 表示上一页最后一条记录的位置（不是当前时间）。
// 排序键是 (published_at, id)，游标携带这两个值即可稳定翻页。
type Cursor struct {
	PublishedAt time.Time
	VideoID     int64
	RankVersion string

	// 热榜游标字段（仅 hot-window-v2 使用）。
	//
	// 为什么热榜需要额外的字段：时间流的排序键 (published_at, id) 是**不可变**的，
	// 游标只要带这两个值就能稳定翻页；而热榜的排序键是"窗口内累计热度"，
	// 它随时在变。解法是把排序键**冻结**：WindowEnd 锁定一个物化窗口快照，
	// (HotScore, VideoID) 表示快照内的位置。三者一起进签名游标，
	// 已有快照不能重写；快照过期时拒绝旧游标，防止排序键漂移。
	WindowEnd time.Time `json:",omitempty"`
	HotScore  int64     `json:",omitempty"`
}

type codec struct {
	key []byte
}

func NewCodec(secret string) *codec {
	return &codec{key: []byte(secret)}
}

// Encode 序列化为 JSON -> Base64URL -> HMAC-SHA256 签名。
// 格式：payload.signature，两者都是 Base64URL 无填充。
func (c *codec) Encode(cur Cursor) (string, error) {
	payload, err := json.Marshal(cur)
	if err != nil {
		return "", fmt.Errorf("marshal cursor: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := c.sign(payloadB64)
	return payloadB64 + "." + sig, nil
}

// Decode 校验签名、解析 JSON，并核对排序版本。
// 注意：版本与当前 Ranker 的一致性由调用方（service）校验，这里只做
// 签名/格式/字段合法性检查——codec 不感知排序策略。
func (c *codec) Decode(s string) (*Cursor, error) {
	payloadB64, sig, ok := strings.Cut(s, ".")
	if !ok {
		return nil, ErrInvalidCursor
	}
	if !hmac.Equal([]byte(sig), []byte(c.sign(payloadB64))) {
		return nil, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var cur Cursor
	if err := json.Unmarshal(payload, &cur); err != nil {
		return nil, ErrInvalidCursor
	}
	if cur.VideoID <= 0 {
		return nil, ErrInvalidCursor
	}
	// 两种游标形态：时间流需要 published_at；热榜需要 window_end。
	// 由调用方按排序版本判断该用哪一种（codec 不感知排序策略）。
	if cur.PublishedAt.IsZero() && cur.WindowEnd.IsZero() {
		return nil, ErrInvalidCursor
	}
	return &cur, nil
}

func (c *codec) sign(payloadB64 string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(payloadB64))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
