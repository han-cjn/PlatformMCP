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
// 诚实边界:飞书的 Ops 来自真实后台截图校准;钉钉 2026-11-11 真机实测了
// /baseinfo /permission /version 三个 tab 的导航、按钮与状态文本(见
// dingtalk_dom_test.go 的脱敏样本);企微/腾讯文档的后台 DOM 仍未实测,
// 菜单文本是推测值。dom_verified 只标「创建/权限/发布表单已完整校准」,
// 钉钉的这些表单未实测故仍为 false,已实测部分由 app_list_verified /
// tabs_verified 单独标注;失败时由 setup 降级到零 DOM 的 set_credentials。
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

// dingtalkStages 按 2026-11-11 真机实测的 /permission 与 /version 页重写。
//
// 旧版把飞书模板直接套到钉钉:菜单/按钮文本是推测值,幂等检测用
// 「通讯录只读/消息推送/SmartForm」三个中文标签(真实页面是英文 scope code +
// 「未开通/立即开通」)。结果 probe 恒判未完成,apply 又在权限页找
// 「确定/确认/提交/申请」和「创建版本/新建版本」→ 两个 selector_miss。
//
// 现在每个阶段先点自己的 tab 并等页面渲染,再用真实状态文本做幂等检测:
//   - /version 的版本表格出现「版本状态 + 已上线」= 已发布,already_done;
//   - /permission 出现「批量申请」且没有「未开通」= 权限齐备,already_done。
//
// 未达成时**不声明任何 apply**:钉钉的权限是逐行「立即开通」(实测 23 个
// 个人权限,多数未开通)、版本按钮是「创建新版本」且其表单未实测,自动提交
// 会产生多项不可逆的企业记录。用 ApplyNote 如实标注为 not_required,
// 而不是伪装成 done 或反复 selector_miss。
func dingtalkStages() []setupStage {
	return []setupStage{
		{
			ID: "enable_perms",
			Action: &BrowserAction{Script: "dingtalk@2026-11-11-perms", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*权限管理\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*权限管理\s*$`, Scope: "probe"},
				{Kind: "waitVisible", Target: "body", Match: `权限点\s*code|批量申请`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
			}},
			Done: func(text string) bool {
				// 「批量申请」是权限页独有的按钮文本(真机实测),比表头「权限点code」
				// 更稳:后者可能被 SPA 拆成多个文本节点。
				return strings.Contains(text, "批量申请") && !strings.Contains(text, "未开通")
			},
			ApplyNote: "钉钉权限页按权限点逐个「立即开通」(实测个人权限 23 项、多数未开通),没有统一的「确定/提交」按钮;Hub 的钉钉能力只依赖 Client ID/Secret + Stream 长连接,不依赖这些 API 权限。逐个自动开通会造成多项不可逆变更,因此只读核对状态、不自动提交;确需开通请在企业后台人工操作",
		},
		{
			ID: "publish",
			Action: &BrowserAction{Script: "dingtalk@2026-11-11-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: `^\s*版本管理与发布\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: `^\s*版本管理与发布\s*$`, Scope: "probe"},
				{Kind: "waitVisible", Target: "body", Match: `版本状态|创建新版本`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
			}},
			Done: func(text string) bool {
				return strings.Contains(text, "版本状态") && strings.Contains(text, "已上线")
			},
			ApplyNote: "钉钉版本页按钮是「创建新版本」(不是「创建版本/新建版本」),其表单未真机校准;创建版本并申请发布会在企业里留下记录,故不自动提交。若版本表格里没有已上线版本,请在后台人工创建,或授权进一步实测该表单",
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
