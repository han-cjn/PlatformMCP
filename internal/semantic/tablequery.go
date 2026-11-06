package semantic

import "strconv"

// table.query 的筛选操作符:统一词汇 → 各平台词汇映射。
//
// 平台词汇表完全不同(2026-11 实测):
//   飞书:  is / isNot / contains / doesNotContain / isEmpty / isNotEmpty / isGreater / isGreaterEqual / isLess / isLessEqual
//   钉钉:  equal / notEqual / contain / notContain / empty / notEmpty / greater / greaterEqual / less / lessEqual
// 领域模型用统一词汇,由平台适配层翻译。

// TableFilter 一条筛选条件(领域模型)。
type TableFilter struct {
	Field string `json:"field"` // 字段名
	Op    string `json:"op"`    // 统一操作符(见 opMap)
	Value string `json:"value"` // 单值(数组由平台适配层包装)
}

// opMap 统一操作符 → 各平台操作符。
var opMap = map[string]map[string]string{
	"feishu": {
		"eq": "is", "ne": "isNot",
		"contains": "contains", "not_contains": "doesNotContain",
		"empty": "isEmpty", "not_empty": "isNotEmpty",
		"gt": "isGreater", "gte": "isGreaterEqual",
		"lt": "isLess", "lte": "isLessEqual",
	},
	"dingtalk": {
		"eq": "equal", "ne": "notEqual",
		"contains": "contain", "not_contains": "notContain",
		"empty": "empty", "not_empty": "notEmpty",
		"gt": "greater", "gte": "greaterEqual",
		"lt": "less", "lte": "lessEqual",
	},
}

// ValidOps 全部合法操作符。
var ValidOps = []string{"eq", "ne", "contains", "not_contains", "empty", "not_empty", "gt", "gte", "lt", "lte"}

// OpNeedsValue 判断操作符是否需要 value。
//
// 注意(2026-11 实测):两平台对 empty/notEmpty 的 value 要求**相反**:
//   - 飞书:所有操作符都强制要求 value(isNotEmpty 也得传,可为空字符串数组)
//   - 钉钉:empty/notEmpty 可不传 value
//
// 因此这里只提供"该操作符语义上是否需要值"的判定,具体是否写入由平台 adapter 决定。
func OpNeedsValue(op string) bool { return !noValueOps[op] }

// noValueOps 语义上不需要值的操作符(空/非空判断)。
var noValueOps = map[string]bool{"empty": true, "not_empty": true}

// TranslateOp 把统一操作符翻译为平台操作符;未映射返回错误。
func TranslateOp(platform, op string) (string, error) {
	m, ok := opMap[platform]
	if !ok {
		return "", &OpError{Platform: platform, Op: op, Reason: "平台无操作符映射表"}
	}
	out, ok := m[op]
	if !ok {
		return "", &OpError{Platform: platform, Op: op, Reason: "操作符不支持"}
	}
	return out, nil
}

// OpError 操作符映射错误。
type OpError struct {
	Platform string
	Op       string
	Reason   string
}

func (e *OpError) Error() string {
	return "table.query 平台 " + e.Platform + " 操作符 " + e.Op + " " + e.Reason +
		"(支持: " + joinKeys(ValidOps) + ")"
}

func joinKeys(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ParseFilters 把领域入参的 filters(任意形态)解析为强类型 TableFilter 列表。
// 兼容三种形态:[]TableFilter / []map[string]any / JSON 反序列化后的结构。
func ParseFilters(raw any) ([]TableFilter, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, &OpError{Reason: "filters 应为数组"}
	}
	out := make([]TableFilter, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &OpError{Reason: "filters[" + itoa(i) + "] 应为对象"}
		}
		f := TableFilter{
			Field: strField(m, "field"),
			Op:    strField(m, "op"),
			Value: strField(m, "value"),
		}
		if f.Field == "" {
			return nil, &OpError{Reason: "filters[" + itoa(i) + "] 缺少 field"}
		}
		if f.Op == "" {
			f.Op = "eq" // 缺省等值
		}
		if !contains(ValidOps, f.Op) {
			return nil, &OpError{Op: f.Op, Reason: "未知操作符"}
		}
		out = append(out, f)
	}
	return out, nil
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func itoa(i int) string { return strconv.Itoa(i) }
