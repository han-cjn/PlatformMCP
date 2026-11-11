package onboard

import "sort"

// PlatformInfo 是项目支持的单个平台的展示信息,供 Agent 在接入选择框里全量列出。
// Capabilities 由调用方从语义层能力矩阵注入,避免 onboard 反向依赖 semantic。
type PlatformInfo struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Title           string      `json:"title"`
	Credentials     string      `json:"credentials"` // configured / missing
	SetupSupported  bool        `json:"setup_supported"`
	BrowserCapture  bool        `json:"browser_capture"`
	DomVerified     bool        `json:"dom_verified"`      // 创建/权限/发布 Ops 是否在真实后台 DOM 上完整校准过
	AppListVerified bool        `json:"app_list_verified"` // 应用列表页 URL/行选择器是否真机实测过
	TabsVerified    bool        `json:"tabs_verified"`     // 应用详情内 tab(凭证/权限/版本)导航是否真机实测
	AppListURL      string      `json:"app_list_url,omitempty"`
	Capabilities    []string    `json:"capabilities"`
	CredentialKeys  []CredField `json:"credential_fields"`
	Hint            string      `json:"hint,omitempty"`
}

var platformNames = map[string]string{
	"feishu":       "飞书",
	"dingtalk":     "钉钉",
	"wecom":        "企业微信",
	"tencent-docs": "腾讯文档",
}

var platformHints = map[string]string{
	"feishu":       "支持 Agent 全自动接入(start → setup);创建/权限/发布 Ops 已在真实后台校准",
	"dingtalk":     "Agent 自动进应用列表页并点进已有应用、自动切到凭证/权限/版本三个 tab(真机实测);权限按真机 code 自动「立即开通」,版本仅在无上线版本时创建保存,均需 setup(confirmed=true) 显式确认;Client Secret 后台只显示掩码,需按 next 用 set_credentials 补一次;2026-11-11 端到端实测 phase=partial(第二个权限点 selector_miss、版本因已有 1.0.0 已上线幂等跳过),发布按钮与二次确认框仍未采集,dom_verified 保持 false",
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
		p := Portals[id]
		var title, listURL string
		if p != nil {
			title = p.Title
			listURL = p.AppListURL
		}
		out = append(out, PlatformInfo{
			ID:          id,
			Name:        platformNames[id],
			Title:       title,
			Credentials: state,
			// 四个平台都挂了凭据动作,因此 setup 全链路都可用;
			// dom_verified 如实区分「真实后台校准过」与「按可见文本声明、待实测」。
			SetupSupported: p != nil && credAction(p) != nil,
			// dom_verified 只标「创建/权限/发布表单已端到端校准」。钉钉 2026-11-11
			// 已跑过一次 setup(confirmed=true),但 phase=partial:第二个权限点
			// selector_miss、publish 因已有 1.0.0 已上线被幂等跳过,版本表单页
			// 根本没进,发布按钮与开通后的二次确认框仍未采集。按「gate 只标
			// 已验证状态」的规则,这里必须保持 false,不能用部分成功洗成 true。
			DomVerified:     id == "feishu",
			AppListVerified: p != nil && p.AppListURL != "" && p.AppListRowSelector != "",
			TabsVerified:    p != nil && p.CredTab != "" && p.CredTabFragment != "",
			AppListURL:      listURL,
			BrowserCapture:  p != nil && credAction(p) != nil,
			Capabilities:    caps,
			CredentialKeys:  CredentialFields(id),
			Hint:            platformHints[id],
		})
	}
	return out
}
