package video

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

// CurrentRankVersion 作者列表的排序版本：排序规则变化时递增，旧游标失效。
const CurrentRankVersion = "author-v1"

var ErrInvalidCursor = errors.New("cursor malformed, tampered or expired")

// Cursor 表示上一页最后一条记录的位置，排序键 (created_at, id)。
// 与 feed 的游标结构不同（feed 是 published_at），管理视图按创建时间排序，
// 未发布视频没有 published_at，无法用同一套键。
type Cursor struct {
	CreatedAt   time.Time
	VideoID     int64
	RankVersion string
}

type codec struct {
	key []byte
}

func NewCodec(secret string) *codec {
	return &codec{key: []byte(secret)}
}

// Encode 序列化为 JSON -> Base64URL -> HMAC-SHA256 签名（payload.signature）。
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
	if cur.CreatedAt.IsZero() || cur.VideoID <= 0 {
		return nil, ErrInvalidCursor
	}
	return &cur, nil
}

func (c *codec) sign(payloadB64 string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(payloadB64))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
