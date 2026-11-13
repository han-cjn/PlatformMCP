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
// dom_verified 只标「创建/权限/发布表单已端到端校准」。钉钉真机执行史:
//   - 19:38 第六轮 setup(dingtalk, confirmed=true):phase=partial。
//     enable_perms 在第 2 个权限点 selector_miss(第 5 步 waitVisible
//     tr[data-row-key] 按 /Calendar.Event.Read/ 未命中),第 1 个权限点的
//     「立即开通」已执行;publish 因版本页已有 1.0.0 已上线判 already_done,
//     版本表单页没进,「保存」后的发布按钮未采集,也没生成 1.0.1 草稿。
//   - 20:20 第七轮只读复核:Contact.User.Read 已变「已开通 / 移除权限」,页面无
//     二次确认弹窗 → 「立即开通」一步生效,旧版「可能有确认框」猜测证伪;连续
//     3 次覆盖式 fill + ElementR 全部命中,证明第 5 步是点击后列表异步刷新的
//     时序问题(已补逐 code 状态复核与幂等 SkipIfMatch,但新增序列未经端到端执行)。
//   - 20:40 第八轮 setup(dingtalk, confirmed=true,用户明确授权「执行」):phase=partial。
//     enable_perms 五个 step 序列跑通(Calendar.Event.Read 的「立即开通」已点),
//     事后只读快照确认 Calendar.Event.Read 已是「无需审批 已开通 移除权限」⇒
//     权限副作用**真实生效**(运行时仍保守标 applied_unverified,但 DOM 已证);
//     publish 五个 step 也跑完、标 applied_unverified,但事后只读快照确认版本列表
//     仍只有 1.0.0 已上线、**没有 1.0.1 草稿** ⇒ 「保存」并未生成待发布版本。
//   - 11-12 第十/十一轮:补齐 fill 1.0.1 后仍无草稿,只读探针定位到两个真根因:
//     (a) 描述是必填项,而 MustInput 的键盘事件在焦点未落入 textarea 时静默
//     丢失(版本号 fill 生效、描述落空),已在 browser.go 的 fill 加读回校验 +
//     JS 原生 setter 降级;
//     (b) 决定性根因:ElementR 返回文档顺序第一个匹配元素,而
//     menuCandidateSelector 里 div 排在 button 之前,「保存」命中的是
//     div.sectionBottom(968px 宽的按钮容器),点它不触发内层 button 的 React
//     事件 ⇒ 点击静默无效。已在 pick 加 pickByText/clickability 打分,优先
//     选 BUTTON/[role=button]/A,容器与隐藏节点扣分。
//   - 11-12 第十二轮 setup(dingtalk, confirmed=true):publish 跑通,版本列表出现
//     `1.0.1 开发中` 草稿⇒「保存」真实生效;点 1.0.1 行的「查看」进详情页
//     (/version/app_publish/<versionID>),采集到「发布」按钮为
//     BUTTON.dtd-btn.dtd-btn-primary(外层 div.right 同样是干扰容器,老逻辑
//     会点错)。发布动作本身仍需人工确认,不由 Hub 自动点击。
//
// 「保存」后的「发布」按钮只在存在「待发布」版本时才出现。2026-11-12 第九轮
// 真机已越过这一步并采集到它(见下方 publish Ops 注释),因此该按钮不再属于
// 「未校准」范围;仍未打开过的是创建应用表单(/app/create),它由 create_app.go
// 单独承担,不在本函数的阶段里。dom_verified 的判定口径见 platforms.go。
// 已实测部分另由 app_list_verified / tabs_verified 标注,未验证边界由
// ApplyWarning / PlanLine 如实回传。
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
//     表单页(不是弹窗),input#version 默认空(显示「版本号格式1.0.0」提示),
//     需显式填 1.0.1 后点「保存」。真机核查:只读点开这个按钮本身没有产生任何版本记录。
//
// 「保存」后的「发布」按钮需先有「待发布」版本才出现;20:40 真机执行「保存」未
// 生成 1.0.1 草稿(版本号字段为空、旧版 apply 未填,已补 fill 1.0.1 待复跑),故
// 发布按钮仍无法只读采集,绝不猜按钮(采集它等于真保存/真发布)。「立即开通」的
// 二次确认框已由只读复核证伪:点击即生效、页面无弹窗。版本阶段由 ApplyWarning
// 明确告知「仅生成待发布版本,发布需人工确认」。
func dingtalkStages() []setupStage {
	permsOps := []Op{
		{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*权限管理\s*$`, Scope: "probe"},
		{Kind: "click", Target: menuCandidateSelector, Match: `^\s*权限管理\s*$`, Scope: "probe"},
		{Kind: "waitVisible", Target: "body", Match: `批量申请|权限点`, Scope: "probe"},
		{Kind: "readPage", Scope: "probe"},
	}
	for _, code := range Portals["dingtalk"].ApplyPerms {
		re := regexp.QuoteMeta(code)
		openedRe := re + `\s*无需审批\s*已开通`
		permsOps = append(permsOps,
			// 点「立即开通」会触发列表异步刷新。下一个 code 先等搜索框重新渲染
			// 完成,避免 fill 落在过渡态 DOM 上 —— 2026-11-11 真机第 5 步
			// (第二个 code 的 waitVisible)就是这样 context deadline exceeded 的;
			// 只读复现证明连续 3 次覆盖式 fill + ElementR 本身全部命中,
			// 选择器与正则都没问题,问题出在点击后的页面状态。
			Op{Kind: "waitVisible", Target: `input[placeholder*="权限或接口"]`, Scope: "apply"},
			Op{Kind: "fill", Target: `input[placeholder*="权限或接口"]`, Text: code, Scope: "apply"},
			Op{Kind: "waitVisible", Target: "tr[data-row-key]", Match: re, Scope: "apply"},
			// 已开通的行按钮是「移除权限」,SkipIfMatch 让它幂等跳过而不是 selector_miss。
			Op{Kind: "clickInRow", Target: "tr[data-row-key]", Match: re, Text: "立即开通",
				SkipIfMatch: openedRe, Scope: "apply"},
			// 逐 code 复核开通状态:既验证点击真的生效,也把「页面刷新完成」
			// 作为下一个 code 的稳定起点,不靠固定 sleep 猜时序。
			Op{Kind: "waitVisible", Target: "tr[data-row-key]", Match: openedRe, Scope: "apply"},
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
			// 2026-11-11 20:20 扫码恢复登录态后只读复核:权限页
			// Contact.User.Read 已是「已开通 / 移除权限」,整页无二次确认弹窗
			// → 「立即开通」是一步生效、不存在待确认框,旧文案里「可能出现的
			// 二次确认框」已被证伪。但上次端到端只跑通第 1 个 code(第 2 个
			// selector_miss),本轮新增的逐 code 状态复核 Ops 也未经真机执行,
			// 因此仍标 applied_unverified:如实说明「新增序列未端到端验证」,
			// 而不是笼统说无法确认开通状态。
			ApplyWarning: "「立即开通」一步生效、无二次确认框(20:20 只读复核证伪旧猜测);20:40 真机五个 step 已跑通且事后只读快照确认 Calendar.Event.Read 已「无需审批 已开通」,副作用真实生效;但运行时仍保守标 applied_unverified、不重试提交。请在后台核对各权限点状态",
		},
		{
			ID: "publish",
			// 2026-11-12 第十二轮真机跑通并采集到「发布」按钮,但它刻意不在 Ops 里:
			// 发布是不可逆的企业动作,Hub 只负责把版本草稿建好,由人工在后台点发布。
			// 详情页路径:/version/app_publish/<versionID>;按钮为
			// BUTTON.dtd-btn.dtd-btn-primary,外层包着 div.right 干扰容器
			// (老 ElementR 会点错,现由 pickByText/clickability 规避)。
			Action: &BrowserAction{Script: "dingtalk@2026-11-12-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*版本管理与发布\s*$`, Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*版本管理与发布\s*$`, Scope: "probe"},
				{Kind: "waitVisible", Target: "body", Match: `版本状态|创建新版本`, Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*创建新版本\s*$`, Scope: "apply"},
				{Kind: "waitVisible", Target: "input#version", Scope: "apply"},
				// 真机表单版本号字段默认空(显示「版本号格式1.0.0」提示),不填版本号「保存」
				// 会因校验失败、不生成草稿(20:40 就栽在这里)。
				// 版本描述同样必填(label 带 dtd-form-item-required),而 MustInput 的键盘
				// 事件在焦点未落入 textarea 时静默丢失(11-12 实测:版本号生效、描述落空)。
				// browser.go 的 fill 已加「读回校验 + JS 原生 setter 降级」兜住这一点。
				{Kind: "fill", Target: "input#version", Text: "1.0.1", Scope: "apply"},
				{Kind: "fill", Target: "textarea#description", Text: "mcp-hub 接入自动发布", Scope: "apply"},
				// 「保存」用按文本匹配的 click:menuCandidateSelector 里 div 排在 button
				// 之前,ElementR 会命中 div.sectionBottom(968px 宽的按钮容器),点它不
				// 触发内层 button 的 React 事件 ⇒ 三次真机点击全部静默无效。现在由
				// pickByText 的打分优先选 BUTTON(见 browser.go clickability)。
				{Kind: "click", Target: menuCandidateSelector, Match: `^\s*保存\s*$`, Scope: "apply"},
				// 保存后必须看到列表里出现待发布/开发中版本,才算真落库。
				// 这一条是 11-12 的教训:前几轮「保存」静默无效却仍返回 applied_unverified,
				// 用户看到「已保存未发布」而实际什么都没生成。宁可在这里 selector_miss,
				// 也不要再给出与事实不符的成功表述。
				{Kind: "waitVisible", Target: "body", Match: `开发中|待发布|未发布`, Scope: "apply"},
			}},
			Done: func(text string) bool {
				// 已存在待发布/未发布/草稿/编辑中/审批中的版本时才算达成,避免重复创建。
				// 只依赖「已上线」状态做幂等会让 1.0.0 一旦上线、publish 永远跳过、
				// 永远创建不出新版本(钉钉允许多版本,创建是增量动作,不是重复操作)。
				// 2026-11-12 真机:列表出现「1.0.1 开发中」⇒ 这里返回 true,不再重复创建。
				re := regexp.MustCompile(`开发中|待发布|未发布|草稿|编辑中|审批中|待审核|未上线`)
				return re.MatchString(text)
			},
			// 2026-11-11 19:38 端到端真机实测:旧版 Done 检测「版本状态+已上线」,
			// 1.0.0 已上线时判 already_done、apply 不执行、不会生成 1.0.1;当时把清单
			// 改为写明「跳过」。但创建新版本是钉钉支持的增量动作(应用可有多版本),
			// 幂等跳过反而让发布永远走不下去。20:40 起改为按「有待发布版本」做幂等:
			// 无待发布版本时执行 apply 创建并保存,已存在待发布版本则 already_done 跳过。
			// 2026-11-12 真机已跑通并被这条幂等覆盖:第二次执行时列表已有「1.0.1 开发中」,
			// publish 正确判 already_done、不重复创建第二个草稿。
			PlanLine:     "创建新版本并保存(版本号沿用后台默认值如 1.0.1;会在企业内产生版本草稿记录;已存在待发布版本则跳过)",
			ApplyWarning: "只在企业内生成「开发中」版本草稿,不自动点「发布」——发布是不可逆动作,请人工在后台核对内容后点发布。2026-11-12 真机已验证「保存」真实落库(版本列表出现 1.0.1 开发中),历史三次失败的两个根因均已修复:版本号/描述未真正填入,以及「保存」点击命中外层 div 容器导致 React 事件不触发",
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
			// 2026-11-12 真机创建 mcp-hub 时发现:企微智能机器人没有「版本 /
			// 申请发布」概念,旧实现沿用 app_console 通用模板,导致
			// setupPlan 在确认清单里输出「创建版本 1.0.0 并申请发布」;更危险的
			// 是 probe 里的 click 会去点未校准的「发布/启用机器人」按钮、
			// apply 里还会点「确定/发布」——都是可能不可逆的动作,而企微后台
			// DOM 至今未端到端校准(stages.go 顶部诚实边界)。
			// 改为纯只读状态核对 + ApplyNote 降级(not_required):
			// 不创建版本、不点发布、不因选择器未命中而误报 selector_miss。
			Action: &BrowserAction{Script: "wecom@2026-11-12-publish-readonly", Ops: []Op{
				{Kind: "waitVisible", Target: "body", Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
			}},
			PlanLine:  "核对机器人启用状态(智能机器人无版本概念,不创建版本、不点发布)",
			ApplyNote: "企微智能机器人无版本/申请发布流程,且后台 DOM 未真机校准;仅做只读状态核对,不执行任何发布动作。如需启用请在管理后台人工核对",
			Done: func(text string) bool {
				return strings.Contains(text, "已启用") || strings.Contains(text, "已发布")
			},
		},
	}
}
