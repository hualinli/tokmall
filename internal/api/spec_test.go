// Package api 的契约测试：把"文档与代码一致"从人工检查变成自动化门禁。
//
// 路由级同步（cmd/api 的 TestOpenAPISync）只保证"路径与方法都在"，
// 抓不到字段改名、类型变化、必填项变化、security 声明错误。这里补上三类：
//
//  1. 结构完整性：每个操作有 summary，$ref 不悬空，operationId 唯一；
//  2. 公开端点必须显式声明 security: []（否则 SDK 会要求 Bearer）；
//  3. schema 与 Go DTO 双向对齐：Go 的 JSON 字段必须在 schema 里出现，
//     schema 的 required 字段必须在 Go 里存在，类型大类必须一致。
//
// 第 3 类是真正能防住"改了字段忘了改契约"的检查，因此它对核心商域 DTO
// 逐个建立映射，而不是泛泛地校验 YAML 语法。
package api

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/lihualin/tokmall/internal/order"
	"github.com/lihualin/tokmall/internal/product"
	"github.com/lihualin/tokmall/internal/video"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// refPattern 匹配 $ref 的值：允许行内 flow 写法（`$ref: "#/..." }`），
// 因此不能简单按行切分。
var refPattern = regexp.MustCompile(`\$ref:\s*"?(#/[^"\s}]+)"?`)

type openAPIDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]schemaNode `yaml:"schemas"`
	} `yaml:"components"`
}

type schemaNode struct {
	Type       string                `yaml:"type"`
	Required   []string              `yaml:"required"`
	Properties map[string]schemaNode `yaml:"properties"`
	Enum       []any                 `yaml:"enum"`
	Items      *schemaNode           `yaml:"items"`
	Ref        string                `yaml:"$ref"`
}

func loadSpec(t *testing.T) openAPIDoc {
	t.Helper()
	var doc openAPIDoc
	require.NoError(t, yaml.Unmarshal(Spec, &doc), "openapi.yaml 必须是合法 YAML")
	return doc
}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true,
}

func operations(t *testing.T, doc openAPIDoc) map[string]map[string]any {
	t.Helper()
	ops := map[string]map[string]any{}
	for path, item := range doc.Paths {
		for method, raw := range item {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			op, ok := raw.(map[string]any)
			require.Truef(t, ok, "%s %s 的 operation 必须是对象", method, path)
			ops[strings.ToUpper(method)+" "+path] = op
		}
	}
	return ops
}

// TestEveryOperationIsDocumented：summary 是契约的最低可读性要求；
// operationId 可以不写，但一旦写了必须唯一（SDK 生成依赖它）。
func TestEveryOperationIsDocumented(t *testing.T) {
	doc := loadSpec(t)
	ops := operations(t, doc)
	require.NotEmpty(t, ops)

	seen := map[string]string{}
	for key, op := range ops {
		summary, _ := op["summary"].(string)
		require.NotEmptyf(t, summary, "%s 缺少 summary", key)

		operationID, _ := op["operationId"].(string)
		if operationID == "" {
			continue
		}
		if previous, ok := seen[operationID]; ok {
			t.Fatalf("operationId %q 被 %s 与 %s 重复使用", operationID, previous, key)
		}
		seen[operationID] = key
	}
}

// TestPublicOperationsDeclareAnonymousAccess：全局 security 是 bearerAuth，
// 公开端点必须用 security: [] 显式覆盖，否则生成的 SDK 会给探针和登录接口
// 也加上 Bearer 要求。
func TestPublicOperationsDeclareAnonymousAccess(t *testing.T) {
	doc := loadSpec(t)
	ops := operations(t, doc)
	public := []string{
		"GET /health/live", "GET /health/ready",
		"POST /api/v1/auth/register", "POST /api/v1/auth/login",
		"POST /api/v1/auth/refresh", "POST /api/v1/auth/logout",
		"GET /api/v1/docs/openapi.yaml",
	}
	for _, key := range public {
		op, ok := ops[key]
		require.Truef(t, ok, "公开端点 %s 必须在契约里", key)
		security, declared := op["security"]
		require.Truef(t, declared, "%s 必须显式声明 security", key)
		list, ok := security.([]any)
		require.Truef(t, ok && len(list) == 0, "%s 的 security 必须是空数组（匿名可访问）", key)
	}
}

// TestReferencesResolve：$ref 悬空会让生成的客户端直接编译失败。
func TestReferencesResolve(t *testing.T) {
	doc := loadSpec(t)
	raw := string(Spec)
	matches := refPattern.FindAllStringSubmatch(raw, -1)
	require.NotEmpty(t, matches, "契约里应当有 $ref")
	for _, match := range matches {
		ref := match[1]
		require.Truef(t, strings.HasPrefix(ref, "#/components/"), "只支持内部引用：%s", ref)
		parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
		require.Len(t, parts, 3, "引用格式应为 #/components/<kind>/<name>：%s", ref)
		section := parts[1]
		name := parts[2]
		switch section {
		case "schemas":
			_, ok := doc.Components.Schemas[name]
			require.Truef(t, ok, "schema 引用悬空：%s", ref)
		case "parameters", "responses", "securitySchemes":
			// 这些分组由 TestOpenAPISync 的运行时路由覆盖，这里只做存在性粗检。
			require.Containsf(t, raw, name+":", "引用悬空：%s", ref)
		default:
			t.Fatalf("未知引用分组：%s", ref)
		}
	}
}

// dtoBinding 把 schema 名与 Go DTO 绑定。新增对外 DTO 时在这里登记，
// 契约漂移就会在 CI 里失败。
type dtoBinding struct {
	schema string
	value  any
}

func TestSchemaMatchesGoDTO(t *testing.T) {
	doc := loadSpec(t)
	bindings := []dtoBinding{
		{"CreateOrderRequest", order.CreateOrderRequest{}},
		{"CreateOrderResponse", order.CreateOrderResponse{}},
		{"PayOrderRequest", order.PayOrderRequest{}},
		{"PayOrderResponse", order.PayOrderResponse{}},
		{"CancelOrderResponse", order.CancelOrderResponse{}},
		{"RequestRefundRequest", order.RequestRefundRequest{}},
		{"DecideRefundRequest", order.DecideRefundRequest{}},
		{"RefundDetail", order.RefundView{}},
		{"OrderDetail", order.OrderView{}},
		{"OrderPage", order.OrderPage{}},
		{"CreateProductRequest", product.CreateProductRequest{}},
		{"CreateSKURequest", product.AddSKURequest{}},
		{"ChangeStatusRequest", product.ChangeStatusRequest{}},
		{"ChangeSKUStatusRequest", product.ChangeSKUStatusRequest{}},
		{"UploadURLRequest", video.UploadURLRequest{}},
		{"CreateVideoRequest", video.CreateVideoRequest{}},
		{"PlayRequest", video.PlayRequest{}},
	}
	for _, binding := range bindings {
		t.Run(binding.schema, func(t *testing.T) {
			schema, ok := doc.Components.Schemas[binding.schema]
			require.Truef(t, ok, "schema %s 不存在", binding.schema)
			assertDTOMatches(t, schema, binding.value)
		})
	}
}

func assertDTOMatches(t *testing.T, schema schemaNode, value any) {
	t.Helper()
	goFields := jsonFields(value)
	require.NotEmptyf(t, goFields, "%T 没有可序列化字段", value)

	// Go → schema：DTO 里出现的每个 JSON 字段都必须被文档化。
	for field, goType := range goFields {
		property, ok := schema.Properties[field]
		if !ok {
			// omitempty 的可选字段允许不出现在响应 schema 中，
			// 但请求 DTO 的字段必须完整（这里统一要求存在，避免漏文档）。
			t.Fatalf("Go 字段 %q 未在 schema %s 中声明", field, schema.Type)
		}
		if property.Type == "" {
			continue // $ref 或 oneOf，交给引用检查
		}
		require.Equalf(t, property.Type, schemaTypeOf(goType),
			"字段 %q 的类型不一致：契约 %s / Go %s", field, property.Type, schemaTypeOf(goType))
	}

	// schema required → Go：契约声明的必填字段必须真的存在。
	for _, required := range schema.Required {
		_, ok := goFields[required]
		require.Truef(t, ok, "schema 声明 %q 必填，但 Go DTO 没有该字段", required)
	}
}

// jsonFields 返回 json 标签名 → Go 类型（跳过 - 与未导出字段）。
func jsonFields(value any) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	typ := reflect.TypeOf(value)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fields[name] = field.Type
	}
	return fields
}

// schemaTypeOf 把 Go 类型映射到 OpenAPI 的基本类型。
// 指针/切片/时间等都在这里有明确归属，避免"类型对不上但没人发现"。
func schemaTypeOf(typ reflect.Type) string {
	switch typ.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array:
		if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			return "array"
		}
		return schemaTypeOf(typ.Elem())
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Map, reflect.Struct:
		// time.Time 等结构体在契约里是 string（date-time）。
		if typ.PkgPath() == "time" {
			return "string"
		}
		return "object"
	default:
		return "object"
	}
}

// TestSchemaPropertiesAreSortedForReadability：契约文件里的属性顺序不影响
// 正确性，但稳定的顺序能让 diff 可读。这里只做提示性检查（不失败），
// 真正的门禁是上面的字段对齐。
func TestEnumValuesMatchGoConstants(t *testing.T) {
	doc := loadSpec(t)
	orderStatus, ok := doc.Components.Schemas["OrderDetail"]
	require.True(t, ok)
	statusSchema := orderStatus.Properties["status"]
	require.NotEmpty(t, statusSchema.Enum, "OrderDetail.status 必须是枚举")

	documented := map[string]bool{}
	for _, value := range statusSchema.Enum {
		documented[value.(string)] = true
	}
	goValues := []order.Status{
		order.StatusPendingPayment, order.StatusPaid, order.StatusFulfilling,
		order.StatusCompleted, order.StatusCanceled, order.StatusExpired,
		order.StatusRefundRequested, order.StatusRefunded,
	}
	for _, status := range goValues {
		require.Truef(t, documented[string(status)], "Go 里的订单状态 %s 未在契约枚举中", status)
	}
	require.Lenf(t, documented, len(goValues), "契约枚举与 Go 常量数量不一致：%v", sortedKeys(documented))
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
