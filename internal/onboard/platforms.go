package onboard

import "sort"

// PlatformInfo 是项目支持的单个平台的展示信息,供 Agent 在接入选择框里全量列出。
// Capabilities 由调用方从语义层能力矩阵注入,避免 onboard 反向依赖 semantic。
type PlatformInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Title          string   `json:"title"`
	Credentials    string   `json:"credentials"` // configured / missing
	SetupSupported bool     `json:"setup_supported"`
	BrowserCapture bool     `json:"browser_capture"`
	Capabilities   []string `json:"capabilities"`
	Hint           string   `json:"hint,omitempty"`
}

var platformNames = map[string]string{
	"feishu":       "飞书",
	"dingtalk":     "钉钉",
	"wecom":        "企业微信",
	"tencent-docs": "腾讯文档",
}

var platformHints = map[string]string{
	"feishu":       "支持 Agent 全自动接入(start → setup → capture)",
	"dingtalk":     "应用创建与开权限需在钉钉后台人工完成,Hub 可接管凭据与能力调用",
	"wecom":        "智能机器人创建需在企微后台人工完成,Hub 可接管凭据与长连接",
	"tencent-docs": "Token 在开放平台自助获取,Hub 可接管凭据与文档能力",
}

// SupportedPlatforms 返回项目支持的全部平台,顺序与 PlatformOrder 一致。
//
// 刻意不按凭据状态过滤:Agent 应把全量平台列进选择框,用 credentials 标注
// 已配置/待接入,让用户自己选(已配置的平台也允许重新接入或体检)。
// setup_supported / browser_capture 从接入清单动态推导,不手写平台白名单。
func SupportedPlatforms(capabilities map[string][]string) []PlatformInfo {
	out := make([]PlatformInfo, 0, len(PlatformOrder()))
	for _, id := range PlatformOrder() {
		state := "configured"
		if len(MissingCreds(id)) > 0 {
			state = "missing"
		}
		caps := append([]string(nil), capabilities[id]...)
		sort.Strings(caps)
		var title string
		if p := Portals[id]; p != nil {
			title = p.Title
		}
		out = append(out, PlatformInfo{
			ID:             id,
			Name:           platformNames[id],
			Title:          title,
			Credentials:    state,
			SetupSupported: id == "feishu",
			BrowserCapture: Portals[id] != nil && credAction(Portals[id]) != nil,
			Capabilities:   caps,
			Hint:           platformHints[id],
		})
	}
	return out
}
