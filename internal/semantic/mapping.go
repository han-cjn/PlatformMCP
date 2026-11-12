package semantic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 表达式语法(刻意保持极小,避免变成需要调试的迷你编程语言):
//   $.field        取领域入参字段
//   $const.VALUE   字面量(string / number / bool 自动推断)
//   $.field ?? x   取字段,空值时用右侧字面量
// 仅支持以上形态;复杂场景请用 PlatformMapping.Adapter 兜底。

// Build 把领域入参展开为该平台的下游调用参数。
// 顺序:Const(固定值) → Args(字段映射) → Encode(序列化嵌入)。
// 后写入的同路径值会覆盖先写入的(Encode 优先级最高,符合"衍生字段覆盖原始字段"直觉)。
func (m PlatformMapping) Build(domain map[string]any) (map[string]any, error) {
	if m.Unsupported != "" {
		return nil, fmt.Errorf("%s", m.Unsupported)
	}
	args := make(map[string]any, len(m.Args)+len(m.Const))

	// 1. 固定值:路径也被支持(setPath 会按 . 拆嵌套),便于写 params.receive_id_type
	for _, path := range sortedKeysAny(m.Const) {
		setPath(args, path, m.Const[path])
	}

	// 2. 字段映射:按路径排序保证结果稳定
	for _, path := range sortedKeysStr(m.Args) {
		val, err := evalExpr(m.Args[path], domain)
		if err != nil {
			return nil, fmt.Errorf("映射 %s: %w", path, err)
		}
		if val == nil {
			continue // 空值不写入,避免给下游塞 null
		}
		setPath(args, path, val)
	}

	// 3. 编码嵌入:把多个字段组装后序列化写入某路径
	for _, enc := range m.Encode {
		if enc.Target == "" {
			return nil, fmt.Errorf("encode 缺少 target")
		}
		payload := make(map[string]any, len(enc.From))
		for _, field := range sortedKeysStr(enc.From) {
			val, err := evalExpr(enc.From[field], domain)
			if err != nil {
				return nil, fmt.Errorf("编码 %s.%s: %w", enc.Target, field, err)
			}
			if val == nil {
				continue
			}
			payload[field] = val
		}
		switch enc.Format {
		case "json", "":
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("编码 %s 为 JSON 失败: %w", enc.Target, err)
			}
			setPath(args, enc.Target, string(raw))
		case "plain":
			// 原样写入对象,不做序列化。用于下游 MCP 会自行序列化的场景:
			// 钉钉 msgParam 由官方 dingtalk-mcp 的 extendType:json 负责 stringify,
			// Hub 若先序列化会被二次 stringify → 钉钉报 msgParam 格式错误。
			setPath(args, enc.Target, payload)
		default:
			return nil, fmt.Errorf("encode format %q 不支持(仅 json / plain)", enc.Format)
		}
	}
	return args, nil
}

// evalExpr 求值单个表达式,返回 nil 表示"无值"(调用方跳过写入)。
func evalExpr(expr string, domain map[string]any) (any, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("空表达式")
	}
	// 支持 a ?? b:空值时回退右侧字面量
	if left, right, ok := cutFallback(expr); ok {
		v, err := evalExpr(left, domain)
		if err != nil {
			return nil, err
		}
		if v != nil {
			if s, isS := v.(string); !isS || strings.TrimSpace(s) != "" {
				return v, nil
			}
		}
		return evalExpr(right, domain)
	}
	switch {
	case strings.HasPrefix(expr, "$."):
		field := strings.TrimPrefix(expr, "$.")
		return domain[field], nil // 不存在返回 nil,由调用方跳过
	case strings.HasPrefix(expr, "$const."):
		return parseLiteral(strings.TrimPrefix(expr, "$const.")), nil
	case strings.HasPrefix(expr, "\"") || strings.HasPrefix(expr, "'"):
		return strings.Trim(expr, "\"'"), nil
	default:
		// 裸字面量
		return parseLiteral(expr), nil
	}
}

// cutFallback 在表达式顶层查找 "??"(忽略引号内的内容)。
func cutFallback(expr string) (left, right string, ok bool) {
	inQuote := rune(0)
	for i := 0; i < len(expr)-1; i++ {
		c := rune(expr[i])
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if c == '?' && expr[i+1] == '?' {
			return expr[:i], expr[i+2:], true
		}
	}
	return "", "", false
}

// parseLiteral 把字面量文本转成具体类型(true/false/整数/浮点/其余为字符串)。
func parseLiteral(s string) any {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// setPath 按 "a.b.c" 路径写入嵌套 map,中间层不存在则创建。
func setPath(root map[string]any, path string, val any) {
	if path == "" {
		return
	}
	parts := strings.Split(path, ".")
	cur := root
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			// 已有非对象值(如 const 写入了标量)时放弃下钻,避免静默改坏结构
			if existing, exists := cur[p]; exists && existing != nil {
				return
			}
			next = make(map[string]any)
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
}

// sortedKeysStr 返回排序后的 map key,保证映射结果稳定可测。
func sortedKeysStr(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysAny(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
