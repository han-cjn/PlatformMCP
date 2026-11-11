package onboard

import (
	"regexp"
	"strings"
)

// platformStages 按平台返回「权限 / 发布」的声明式阶段。
//
// 设计约束(与 setup.go 的编排器共同保证):
//   - probe 阶段的 Ops 只读(等菜单、点菜单、读整页文本),用于幂等检测;
//   - apply 阶段才产生真实副作用,全部受 setup 的 confirmed 门保护;
//   - 未命中任何选择器立即 selector_miss 并回传页面候选,不静默重试提交;
//   - 各阶段独立执行、独立记状态,单阶段失败不阻断后续阶段。
//
// 诚实边界:飞书的 Ops 来自真实后台截图校准;钉钉 2026-11-11 真机实测了
// /baseinfo /permission /version 三个 tab 的导航、按钮与状态文本,并只读
// 打开 /version/app_publish/new 拿到表单字段(input#version / textarea#description
// /「保存」)与 /permission 行级 DOM(tr[data-row-key] + checkbox + span「立即开通」);
// 企微/腾讯文档的后台 DOM 仍未实测,菜单文本是推测值。
//
// dom_verified 只标「创建/权限/发布表单已端到端校准」:钉钉「保存」后的发布
// 按钮与「立即开通」后的二次确认框未经真机执行(采集它们等于真提交),创建
// 应用表单也未打开过,故仍为 false;已实测部分由 app_list_verified /
// tabs_verified 单独标注,未验证边界由 setupStage.ApplyWarning 如实回传。
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

// dingtalkStages 按 2026-11-11 真机实测的 /permission、/version 与
// /version/app_publish/new 表单页重写,并补上受 confirmed 门保护的 apply Ops。
//
// 演进:飞书模板套钉钉 → 两个 selector_miss → ApplyNote(not_required 只读核对)
// → 本次按真机结构自动提交:
//   - /permission:行 tr[data-row-key] 文本含英文 code,行内是 span「立即开通」。
//     先用搜索框按 code 过滤,再 clickInRow 只点该行的按钮,避免误开别的权限。
//     snsapi_base 真机是「默认开通」,行内没有按钮,只进 Perms 不进 ApplyPerms。
//   - /version:点「创建新版本」是同标签路由跳到 /version/app_publish/new
//     表单页(不是弹窗),input#version 已默认填 1.0.1,填描述后点「保存」。
//     真机核查:点开这个按钮本身没有产生任何版本记录。
//
// 未采集、因此绝不猜的边界:「立即开通」后的二次确认框、保存后的「发布」
// 按钮。前者若出现会让下一步 selector_miss 并回传候选元素;后者由
// ApplyWarning 明确告知「仅生成待发布版本,发布需人工确认」。
func dingtalkStages() []setupStage {
	permsOps := []Op{
		{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*权限管理\s*$`, Scope: "probe"},
		{Kind: "click", Target: menuCandidateSelector, Match: `^\s*权限管理\s*$`, Scope: "probe"},
		{Kind: "waitVisible", Target: "body", Match: `批量申请|权限点`, Scope: "probe"},
		{Kind: "readPage", Scope: "probe"},
	}
	for _, code := range Portals["dingtalk"].ApplyPerms {
		re := regexp.QuoteMeta(code)
		permsOps = append(permsOps,
			Op{Kind: "fill", Target: `input[placeholder*="权限或接口"]`, Text: code, Scope: "apply"},
			Op{Kind: "waitVisible", Target: "tr[data-row-key]", Match: re, Scope: "apply"},
			Op{Kind: "clickInRow", Target: "tr[data-row-key]", Match: re, Text: "立即开通", Scope: "apply"},
		)
	}
	return []setupStage{
		{
			ID:     "enable_perms",
			Action: &BrowserAction{Script: "dingtalk@2026-11-11-perms", Ops: permsOps},
			Done: func(text string) bool {
				// 逐 code 检查「无需审批 已开通」,而不是旧版「整页不含『未开通』」:
				// 后者要求把 23 项个人权限全开通,既不必要也永远满足不了。
				for _, code := range Portals["dingtalk"].Perms {
					re := regexp.MustCompile(regexp.QuoteMeta(code) + `\s*无需审批\s*已开通`)
					if !re.MatchString(text) {
						return false
					}
				}
				return true
			},
			PlanLine: "按权限点 code 自动开通 " + strings.Join(Portals["dingtalk"].ApplyPerms, " / ") +
				"(已开通则跳过;每项都会在企业内产生开通记录)",
		},
		{
			ID: "publish",
			Action: &BrowserAction{Script: "dingtalk@2026-11-11-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*版本管理与发布\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*版本管理与发布\s*$`, Scope: "probe"},
				{Kind: "waitVisible", Target: "body", Match: `版本状态|创建新版本`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*创建新版本\s*$`, Scope: "apply"},
				{Kind: "waitVisible", Target: "input#version", Scope: "apply"},
				{Kind: "fill", Target: "textarea#description", Text: "mcp-hub 接入自动发布", Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*保存\s*$`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				return strings.Contains(text, "版本状态") && strings.Contains(text, "已上线")
			},
			PlanLine:     "自动创建新版本并保存(版本号沿用后台默认值如 1.0.1;会在企业内产生版本记录)",
			ApplyWarning: "「保存」后只生成待发布版本;发布按钮未真机校准(采集它等于真发布),不自动点击。请在后台核对版本内容后再手动发布",
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
