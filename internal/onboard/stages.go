package onboard

import "strings"

// platformStages 按平台返回「权限 / 发布」的声明式阶段。
//
// 设计约束(与 setup.go 的编排器共同保证):
//   - probe 阶段的 Ops 只读(等菜单、点菜单、读整页文本),用于幂等检测;
//   - apply 阶段才产生真实副作用,全部受 setup 的 confirmed 门保护;
//   - 未命中任何选择器立即 selector_miss 并回传页面候选,不静默重试提交;
//   - 各阶段独立执行、独立记状态,单阶段失败不阻断后续阶段。
//
// 诚实边界:飞书的 Ops 来自真实后台截图校准;钉钉/企微/腾讯文档的后台 DOM
// 未经真机实测,菜单文本是当前版本的推测值。Portal.DomVerified 会如实标注,
// 失败时由 setup 降级到零 DOM 的 hub.onboard.set_credentials。
func platformStages(p *Portal) []setupStage {
	if p == nil {
		return nil
	}
	switch p.Name {
	case "feishu":
		return feishuStages()
	case "dingtalk":
		return dingtalkStages()
	case "wecom":
		return wecomStages()
	default:
		// token_page(腾讯文档):没有应用、权限、版本的概念,登录授权页即凭据页。
		return nil
	}
}

// feishuSetupStages 保留为薄包装,供既有单测与只读调用使用;
// 真正的多平台分发入口是 platformStages(p)。
func feishuSetupStages() []setupStage { return platformStages(Portals["feishu"]) }

func feishuStages() []setupStage {
	return []setupStage{
		{
			ID: "enable_perms",
			Action: &BrowserAction{Script: "feishu@2026-11-perms", Ops: []Op{
				// 先等菜单文本渲染出来(SPA 异步),再点:旧版直接 click 会在页面刚加载时 6s 超时
				{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*权限管理(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*权限管理(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(开通权限|添加权限|申请权限|批量导入权限)\s*$`, Scope: "apply"},
				{Kind: "fill", Target: "input[placeholder*='搜索'], input[type=text], textarea",
					Text: strings.Join(Portals["feishu"].Perms, ", "), Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(确定|确认|提交|开通)\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				for _, k := range Portals["feishu"].Perms {
					if !strings.Contains(text, k) {
						return false
					}
				}
				return true
			},
		},
		{
			ID: "publish",
			Action: &BrowserAction{Script: "feishu@2026-11-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*(版本管理与发布|应用发布|版本管理)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(版本管理与发布|应用发布|版本管理)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(创建版本|新建版本)\s*$`, Scope: "apply"},
				{Kind: "fill", Target: "input[placeholder*='版本'], input[type=text], textarea", Text: "1.0.0", Scope: "apply"},
				{Kind: "fill", Target: "textarea, input[placeholder*='说明'], input[placeholder*='描述']",
					Text: "mcp-hub 接入自动发布", Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(保存|确定|提交)\s*$`, Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(申请发布|发布|提交发布)\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				return strings.Contains(text, "当前修改均已发布") || strings.Contains(text, "已发布且生效")
			},
		},
	}
}

func dingtalkStages() []setupStage {
	perms := strings.Join(Portals["dingtalk"].Perms, ", ")
	return []setupStage{
		{
			ID: "enable_perms",
			Action: &BrowserAction{Script: "dingtalk@2026-11-perms", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*(权限管理|权限申请|权限配置)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(权限管理|权限申请|权限配置)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(申请权限|添加权限|批量申请|开通权限)\s*$`, Scope: "apply"},
				{Kind: "fill", Target: "input[placeholder*='搜索'], input[type=text], textarea",
					Text: perms, Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(确定|确认|提交|申请)\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				for _, k := range Portals["dingtalk"].Perms {
					if !strings.Contains(text, k) {
						return false
					}
				}
				return true
			},
		},
		{
			ID: "publish",
			Action: &BrowserAction{Script: "dingtalk@2026-11-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*(版本管理与发布|应用发布|版本管理)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(版本管理与发布|应用发布|版本管理)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(创建版本|新建版本)\s*$`, Scope: "apply"},
				{Kind: "fill", Target: "input[type=text], textarea", Text: "1.0.0", Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(保存|确定|提交)\s*$`, Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*(申请发布|发布|提交发布)\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				return strings.Contains(text, "已发布") || strings.Contains(text, "当前版本已生效")
			},
		},
	}
}

func wecomStages() []setupStage {
	return []setupStage{
		{
			ID: "enable_perms",
			Action: &BrowserAction{Script: "wecom@2026-11-perms", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*(权限管理|能力配置|机器人能力)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(权限管理|能力配置|机器人能力)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(开通|启用|添加能力|开通权限)\s*$`, Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(确定|确认|保存)\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				return strings.Contains(text, "已开通") || strings.Contains(text, "已启用")
			},
		},
		{
			ID: "publish",
			Action: &BrowserAction{Script: "wecom@2026-11-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*(发布|启用机器人|保存并启用|版本管理)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(发布|启用机器人|保存并启用|版本管理)(?:\s*\d+)?\s*$`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*(确定|确认|发布|启用)\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				return strings.Contains(text, "已启用") || strings.Contains(text, "已发布")
			},
		},
	}
}
