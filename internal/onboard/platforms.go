package onboard

import "sort"

// PlatformInfo 是项目支持的单个平台的展示信息,供 Agent 在接入选择框里全量列出。
// Capabilities 由调用方从语义层能力矩阵注入,避免 onboard 反向依赖 semantic。
type PlatformInfo struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Title          string      `json:"title"`
	Credentials    string      `json:"credentials"` // configured / missing
	SetupSupported bool        `json:"setup_supported"`
	BrowserCapture bool        `json:"browser_capture"`
	DomVerified    bool        `json:"dom_verified"` // 声明式 Ops 是否在真实后台 DOM 上校准过
	Capabilities   []string    `json:"capabilities"`
	CredentialKeys []CredField `json:"credential_fields"`
	Hint           string      `json:"hint,omitempty"`
}

var platformNames = map[string]string{
	"feishu":       "飞书",
	"dingtalk":     "钉钉",
	"wecom":        "企业微信",
	"tencent-docs": "腾讯文档",
}

var platformHints = map[string]string{
	"feishu":       "支持 Agent 全自动接入(start → setup);创建/权限/发布 Ops 已在真实后台校准",
	"dingtalk":     "支持 Agent 自动创建应用、抓凭据、开权限与发布;后台 DOM 未实测,selector_miss 时降级 set_credentials",
	"wecom":        "支持 Agent 自动创建智能机器人、抓凭据与启用;后台 DOM 未实测,selector_miss 时降级 set_credentials",
	"tencent-docs": "授权页即凭据页,Agent 可自动读 Token 并完成接入;无需建应用/开权限/发布,DOM 未实测时降级 set_credentials",
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
			ID:          id,
			Name:        platformNames[id],
			Title:       title,
			Credentials: state,
			// 四个平台都挂了凭据动作,因此 setup 全链路都可用;
			// dom_verified 如实区分「真实后台校准过」与「按可见文本声明、待实测」。
			SetupSupported: Portals[id] != nil && credAction(Portals[id]) != nil,
			DomVerified:    id == "feishu",
			BrowserCapture: Portals[id] != nil && credAction(Portals[id]) != nil,
			Capabilities:   caps,
			CredentialKeys: CredentialFields(id),
			Hint:           platformHints[id],
		})
	}
	return out
}
