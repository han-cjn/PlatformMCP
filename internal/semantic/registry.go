// Package semantic 实现统一语义层:把三平台能力收敛为最小公共语义集,
// 客户端只写 im.send_text / table.query,不感知平台差异。
package semantic

import (
	"fmt"
)

// Registry 维护 capability -> platform -> 下游工具名 的路由表(来自配置)。
type Registry struct {
	routes map[string]map[string]string
}

func NewRegistry(routes map[string]map[string]string) *Registry {
	return &Registry{routes: routes}
}

// Resolve 解析语义调用到具体平台的下游工具名。
func (r *Registry) Resolve(capability, platform string) (string, error) {
	byPlatform, ok := r.routes[capability]
	if !ok {
		return "", fmt.Errorf("未知能力 %q", capability)
	}
	tool, ok := byPlatform[platform]
	if !ok {
		return "", fmt.Errorf("能力 %q 不支持平台 %q", capability, platform)
	}
	return tool, nil
}

// Capabilities 返回已注册的全部统一能力名。
func (r *Registry) Capabilities() []string {
	out := make([]string, 0, len(r.routes))
	for c := range r.routes {
		out = append(out, c)
	}
	return out
}
