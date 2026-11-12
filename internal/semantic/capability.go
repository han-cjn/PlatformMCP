// Package semantic 实现统一语义层:把多平台能力收敛为最小公共语义集,
// 客户端只写 im.send_text / table.query / doc.create,不感知平台差异。
//
// 设计要点(2026-11 重构):
//   - 领域模型(Capability)描述「平台无关」的强类型入参,用于校验 + 生成 MCP inputSchema
//   - 映射声明(PlatformMapping)描述「领域字段 → 下游参数」的转换,纯数据,新增平台不改 Go 代码
//   - 执行引擎(Invoke)统一做:补默认值 → 校验 → 映射 → 调用,并给出可读的降级原因
package semantic

import (
	"fmt"
	"sort"
	"strings"
)

// FieldSpec 领域字段的类型与约束,同时用于入参校验和生成 MCP 工具 schema。
type FieldSpec struct {
	Type    string   `json:"type"`              // string / number / bool / object / array
	Desc    string   `json:"desc"`              // 给客户端的说明
	Enum    []string `json:"enum,omitempty"`    // 可选值约束
	Default any      `json:"default,omitempty"` // 缺省值(未设置且非必填时补入)
}

// Capability 一个统一能力的完整定义:领域模型 + 各平台映射规则。
type Capability struct {
	Name        string                     `json:"name"`        // 语义能力名,如 im.send_text
	Title       string                     `json:"title"`       // 人类可读名
	Description string                     `json:"description"` // 给 MCP 客户端看的能力说明
	Schema      map[string]FieldSpec       `json:"schema"`      // 领域字段定义
	Required    []string                   `json:"required"`    // 必填字段
	Platforms   map[string]PlatformMapping `json:"platforms"`   // platform -> 映射规则
}

// PlatformMapping 一个平台对某能力的映射规则。
// 普通差异用 Args/Const/Encode 声明;复杂场景(多步调用、需先建会话等)用 Adapter 兜底。
type PlatformMapping struct {
	Tool        string            `json:"tool"`                  // 下游工具名
	Args        map[string]string `json:"args,omitempty"`        // 下游参数路径 -> 表达式(如 "$.target")
	Const       map[string]any    `json:"const,omitempty"`       // 固定值,原样写入下游参数路径
	Encode      []FieldEncode     `json:"encode,omitempty"`      // 需序列化后嵌入的字段(如飞书 content)
	Adapter     string            `json:"adapter,omitempty"`     // 具名 Go 适配器(复杂场景兜底)
	Unsupported string            `json:"unsupported,omitempty"` // 非空表示该平台明确不支持,值为原因
}

// FieldEncode 把多个领域字段编码(JSON 或拼接)后写入某个下游参数路径。
type FieldEncode struct {
	Target string            `json:"target"` // 下游路径,如 data.content
	Format string            `json:"format"` // json(序列化为字符串) / plain(原样传对象)
	From   map[string]string `json:"from"`   // 编码前的字段名 -> 表达式
}

// Catalog 全部已注册能力的集合,按名字索引。
type Catalog struct {
	caps map[string]*Capability
}

// NewCatalog 由能力列表构建目录。
func NewCatalog(caps ...*Capability) *Catalog {
	c := &Catalog{caps: make(map[string]*Capability, len(caps))}
	for _, cap := range caps {
		if cap != nil && cap.Name != "" {
			c.caps[cap.Name] = cap
		}
	}
	return c
}

// Get 按名字取能力。
func (c *Catalog) Get(name string) (*Capability, bool) {
	cap, ok := c.caps[name]
	return cap, ok
}

// All 返回按名字排序的全部能力(顺序稳定,便于测试与自检输出)。
func (c *Catalog) All() []*Capability {
	names := make([]string, 0, len(c.caps))
	for n := range c.caps {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*Capability, 0, len(names))
	for _, n := range names {
		out = append(out, c.caps[n])
	}
	return out
}

// Names 返回按名字排序的能力名列表。
func (c *Catalog) Names() []string {
	caps := c.All()
	out := make([]string, 0, len(caps))
	for _, cap := range caps {
		out = append(out, cap.Name)
	}
	return out
}

// ---- 入参校验与归一化 ----

// Validate 补默认值、检查必填、校验类型与枚举,返回归一化后的领域入参。
// 返回的 map 是新副本,不修改调用方数据。
func (cap *Capability) Validate(in map[string]any) (map[string]any, error) {
	if cap == nil {
		return nil, fmt.Errorf("能力未定义")
	}
	out := make(map[string]any, len(cap.Schema))
	for k, v := range in {
		out[k] = v
	}
	// 补默认值
	for name, spec := range cap.Schema {
		if _, ok := out[name]; !ok && spec.Default != nil {
			out[name] = spec.Default
		}
	}
	// 必填
	var missing []string
	for _, name := range cap.Required {
		v, ok := out[name]
		if !ok || v == nil || (isString(v) && strings.TrimSpace(v.(string)) == "") {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("缺少必填字段 %s", strings.Join(missing, ", "))
	}
	// 类型与枚举
	var problems []string
	for name, v := range out {
		spec, ok := cap.Schema[name]
		if !ok {
			continue // 未声明的额外字段放行,便于平台特有透传
		}
		if v == nil {
			continue
		}
		if err := checkType(name, v, spec); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if len(spec.Enum) > 0 {
			s, ok := v.(string)
			if !ok || !containsStr(spec.Enum, s) {
				problems = append(problems, fmt.Sprintf("%s 取值 %v 不在允许范围 %v", name, v, spec.Enum))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return out, nil
}

func checkType(name string, v any, spec FieldSpec) error {
	switch spec.Type {
	case "", "string":
		if !isString(v) {
			return fmt.Errorf("%s 应为字符串,实际 %T", name, v)
		}
	case "number":
		switch v.(type) {
		case float64, float32, int, int32, int64, uint, uint32, uint64:
		default:
			return fmt.Errorf("%s 应为数字,实际 %T", name, v)
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s 应为布尔值,实际 %T", name, v)
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("%s 应为对象,实际 %T", name, v)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("%s 应为数组,实际 %T", name, v)
		}
	}
	return nil
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- 平台支持情况 ----

// SupportedPlatforms 返回该能力已声明可用的平台(按名字排序)。
// 显式标记 Unsupported 的平台不计入。
func (cap *Capability) SupportedPlatforms() []string {
	out := make([]string, 0, len(cap.Platforms))
	for p, m := range cap.Platforms {
		if m.Unsupported == "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Matrix 返回能力矩阵:capability -> platform -> 状态。
// 状态取值:supported / unsupported(显式声明不可用)/ absent(该平台未声明此能力)。
func (cap *Capability) Matrix(platforms []string) map[string]string {
	out := make(map[string]string, len(platforms))
	for _, p := range platforms {
		m, ok := cap.Platforms[p]
		switch {
		case !ok:
			out[p] = "absent"
		case m.Unsupported != "":
			out[p] = "unsupported"
		default:
			out[p] = "supported"
		}
	}
	return out
}
