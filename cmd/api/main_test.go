package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/lihualin/tokmall/internal/api"
	"github.com/lihualin/tokmall/internal/feed"
	"github.com/lihualin/tokmall/internal/interaction"
	"github.com/lihualin/tokmall/internal/user"
	"github.com/lihualin/tokmall/internal/video"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type openAPIDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

var hertzParamRe = regexp.MustCompile(`:([^/]+)`)

// TestOpenAPISync 契约同步测试：用与 run() 完全相同的注册逻辑（nil handler）
// 生成运行时路由表，与 openapi.yaml 的 paths 双向 diff——
// 路由表有 yaml 没有 = 忘写文档；yaml 有路由表没有 = 文档残留。
func TestOpenAPISync(t *testing.T) {
	var doc openAPIDoc
	require.NoError(t, yaml.Unmarshal(api.Spec, &doc), "openapi.yaml 必须是合法 yaml")

	h := server.New()
	registerRoutes(h, routes{
		user:        user.NewHandler(nil),
		video:       video.NewHandler(nil),
		feed:        feed.NewHandler(nil),
		interaction: interaction.NewHandler(nil),
	})

	routeKeys := map[string]bool{}
	for _, r := range h.Routes() {
		method := strings.ToUpper(r.Method)
		if method == "HEAD" || method == "OPTIONS" {
			continue
		}
		routeKeys[method+" "+hertzParamRe.ReplaceAllString(r.Path, "{$1}")] = true
	}

	docKeys := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			docKeys[strings.ToUpper(method)+" "+path] = true
		}
	}

	var missing, stale []string
	for k := range routeKeys {
		if !docKeys[k] {
			missing = append(missing, k)
		}
	}
	for k := range docKeys {
		if !routeKeys[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	require.Empty(t, missing, "路由表存在但 openapi.yaml 缺失（改端点必须同步改文档）")
	require.Empty(t, stale, "openapi.yaml 存在但路由表没有（文档残留，端点已删除）")
}
