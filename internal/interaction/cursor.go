package interaction

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

// CurrentRankVersion 关注列表的排序版本：排序规则变化时递增，旧游标失效。
const CurrentRankVersion = "follow-v1"

var ErrInvalidCursor = errors.New("cursor malformed, tampered or expired")

// Cursor 表示上一页最后一条关系的位置，排序键 (created_at, user_id)。
type Cursor struct {
	CreatedAt   time.Time
	UserID      int64
	RankVersion string
}

// codec 与 feed/video 的游标实现同构（JSON -> Base64URL -> HMAC）。
// 这是第三个模块使用同一模式，若再出现重复应抽取 internal/cursor。
type codec struct {
	key []byte
}

func NewCodec(secret string) *codec {
	return &codec{key: []byte(secret)}
}

func (c *codec) Encode(cur Cursor) (string, error) {
	payload, err := json.Marshal(cur)
	if err != nil {
		return "", fmt.Errorf("marshal cursor: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := c.sign(payloadB64)
	return payloadB64 + "." + sig, nil
}

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
	if cur.RankVersion != CurrentRankVersion {
		return nil, ErrInvalidCursor
	}
	if cur.CreatedAt.IsZero() || cur.UserID <= 0 {
		return nil, ErrInvalidCursor
	}
	return &cur, nil
}

func (c *codec) sign(payloadB64 string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(payloadB64))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
