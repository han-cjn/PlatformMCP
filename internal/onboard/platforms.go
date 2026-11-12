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
	"dingtalk":     "支持 Agent 全自动接入(start → setup);创建/权限/发布 Ops 已在真实后台校准(dom_verified=true)。Agent 自动进应用列表页并点进已有应用、自动切到凭证/权限/版本三个 tab;权限按真机 code 自动「立即开通」(已开通的行幂等跳过);版本在无待发布版本时创建 1.0.1 并保存为「开发中」草稿(已有待发布版本则跳过),但**刻意不自动点「发布」**——发布不可逆,需人工在后台确认。2026-11-12 真机已验证「保存」真实落库;历史三次失败根因已修:版本号/描述未真正填入、「保存」点击命中外层 div 容器。Client Secret 页面渲染成掩码,但复制图标写入剪贴板的是完整明文(2026-11-12 用户手工实测);collect-creds 已改为 copyToCred 自动点复制读明文,capture 可全自动拿全;2026-11-12 第十四轮已在钉钉真实 /baseinfo 页端到端跑通(hook 主路径读到 64 位明文、Secret≠Client ID、格式与真连校验通过、.env 写入成功、复跑幂等),失败仍按 next 降级 set_credentials",
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
			// dom_verified 只标「创建/权限/发布 Ops 是否在真实后台 DOM 上完整校准」。
			// 钉钉 2026-11-12 第十二轮 setup(confirmed=true)后已满足:
			//   - 版本表单:input#version 与 textarea#description 均真机填成功,
			//     「保存」后版本列表出现 `1.0.1 开发中` ⇒ 表单字段与提交动作实测有效;
			//   - 发布按钮:点 1.0.1 行「查看」进 /version/app_publish/<id> 详情页,
			//     实测采集到 BUTTON.dtd-btn.dtd-btn-primary(外层 div.right 是干扰
			//     容器)。发布动作本身刻意不由 Hub 执行,因此不在 Ops 里,但按钮
			//     DOM 已校准,人工按提示点即可;
			//   - 权限点:Calendar.Event.Read 与 Contact.User.Read 已是「已开通」,
			//     逐 code 状态复核与幂等跳过序列真机跑通;
			//   - 创建应用表单(/app/create)属 create_app.go 独立链路,钉钉后台
			//     已有应用、本次未走该分支,因此它的实测状态由 app_list_verified
			//     单独标注,不掺进本 gate。
			DomVerified:     id == "feishu" || id == "dingtalk",
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
