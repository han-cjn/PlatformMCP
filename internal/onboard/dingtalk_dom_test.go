package onboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 2026-11-11 钉钉开放平台真机 dump 的脱敏样本:结构与可见文本逐字一致,
// 凭据值全部替换为假值。用于把「按可见文本声明的 Ops」锁在真实 DOM 上,
// 防止再出现「按飞书模板推测按钮名」造成的 selector_miss。
const (
	dingBaseinfoText = `个人调试 已上线
应用凭证
App ID : 1f2e47b9-a105-48f6-9cdc-45b32563a88e
原企业内部应用AgentId 5110249287
Client ID (原 AppKey 和 SuiteKey)
dingabcdefghijklmnop
Client Secret (原 AppSecret 和 SuiteSecret)
当前使用:-JxJlo7zXy****pY7Ah3PcyG
应用信息`

	dingPermissionText = `权限管理
选择开通状态 批量申请
全部（0） 个人权限（23）
权限信息 接口 权限点code 权限审批 全部状态 操作
Contact.User.mobile 无需审批 未开通 立即开通
Contact.User.Read 无需审批 未开通 立即开通
Calendar.Event.Read 无需审批 未开通 立即开通
snsapi_base 无需审批 已开通 默认开通
请输入权限或接口的名称进行搜索`

	// 2026-11-11 20:20 扫码恢复登录态后只读复核的 /permission 真机文本。
	// 关键事实:Contact.User.Read 已被上次 confirmed=true 真实点成「已开通 /
	// 移除权限」,Calendar.Event.Read 仍「未开通 / 立即开通」,整页无二次确认弹窗。
	// 这份样本用于锁定「部分开通后重跑」的幂等 SkipIfMatch 与状态复核正则。
	dingPermissionTextAfterOpen = `权限管理
选择审核要求 选择开通状态 批量申请
全部 个人权限 默认目录
权限信息 接口 权限点code 权限审批 全部状态 操作
个人手机号信息 获取用户个人信息 Contact.User.mobile 无需审批 未开通 立即开通
日历应用中日程读权限 查询会议室忙闲 查看更多 Calendar.Event.Read 无需审批 未开通 立即开通
通讯录个人信息读权限 获取用户个人信息 Contact.User.Read 无需审批 已开通 移除权限
调用SNS API时需要具备的基本权限 创建快捷方式 查询个人授权记录 snsapi_base 无需审批 已开通 默认开通
请输入权限或接口的名称进行搜索`

	dingVersionText = `版本管理与发布
创建新版本
版本号 版本状态 发布人 发布时间 操作
1.0.0 已上线 韩业超 2026-10-30 16:41:48 查看`

	// 2026-11-11 真机点开「创建新版本」后 /version/app_publish/new 的脱敏文本。
	// 只读打开、未提交;核查过版本表格仍只有 1.0.0 一行,没有产生草稿记录。
	dingVersionFormText = `返回 版本详情 开发中
应用版本可设置应用版本号、应用能力和应用可用范围，所有变更需要保存并发布后才会正式生效，请谨慎操作
应用版本号 版本描述 待发布内容
版本号格式1.0.0 请输入版本描述
应用可用范围 全部员工 部分员工 仅限管理员 仅我可见
保存`
)

// collect-creds 必须先切到 /baseinfo,再按真实标签抽取;Secret 只有掩码。
func TestDingtalkCredOpsSwitchToBaseinfoTab(t *testing.T) {
	act := credAction(Portals["dingtalk"])
	if act == nil {
		t.Fatal("钉钉缺少浏览器凭据动作")
	}
	if act.Script != "dingtalk@2026-11-11-creds" {
		t.Fatalf("脚本标记未更新: %s", act.Script)
	}
	if len(act.Ops) != 6 {
		t.Fatalf("凭据 Ops 应为 6 步,实际 %d: %+v", len(act.Ops), act.Ops)
	}
	if act.Ops[0].Kind != "waitVisible" || act.Ops[0].Target != menuCandidateSelector ||
		!strings.Contains(act.Ops[0].Match, "凭证与基础信息") {
		t.Fatalf("第 1 步应等「凭证与基础信息」菜单: %+v", act.Ops[0])
	}
	if act.Ops[1].Kind != "click" || !strings.Contains(act.Ops[1].Match, "凭证与基础信息") {
		t.Fatalf("第 2 步应点「凭证与基础信息」菜单: %+v", act.Ops[1])
	}
	if act.Ops[2].Kind != "waitVisible" || act.Ops[2].Target != "body" {
		t.Fatalf("第 3 步应等凭据页渲染: %+v", act.Ops[2])
	}
	if act.Ops[3].Kind != "readPage" {
		t.Fatalf("第 4 步应 readPage: %+v", act.Ops[3])
	}

	reID := regexp.MustCompile(act.Ops[4].Target)
	if got := reID.FindStringSubmatch(dingBaseinfoText); len(got) < 2 || got[1] != "dingabcdefghijklmnop" {
		t.Fatalf("Client ID 正则未从真机文本抽到明文: %v /%s/", got, act.Ops[4].Target)
	}
	reSecret := regexp.MustCompile(act.Ops[5].Target)
	got := reSecret.FindStringSubmatch(dingBaseinfoText)
	if len(got) < 2 || !strings.Contains(got[1], "****") {
		t.Fatalf("Client Secret 正则应命中掩码串(交给格式校验拒绝): %v /%s/", got, act.Ops[5].Target)
	}
}

// 掩码绝不能被写进 .env;降级文案必须点明「只差 Secret」。
func TestDingtalkMaskedSecretNeverWritten(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	_, ce := commitCredentials("dingtalk", map[string]string{
		"DINGTALK_Client_ID":     "dingabcdefghijklmnop",
		"DINGTALK_Client_Secret": "-JxJlo7zXy****pY7Ah3PcyG",
	}, CredOptions{EnvPath: envPath, SkipSetEnv: true})
	if ce == nil || ce.Phase != "invalid_format" {
		t.Fatalf("掩码应被格式校验拒绝,实际 %+v", ce)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("掩码凭据不得写入 .env")
	}
	next := captureNext(capturePhase(ce.Phase), "dingtalk")
	for _, want := range []string{"掩码", "set_credentials", "Client Secret", "不要"} {
		if !strings.Contains(next, want) {
			t.Fatalf("降级指引缺 %q: %s", want, next)
		}
	}
}

// 幂等检测必须用真机状态文本,不能再用飞书式权限名或「已发布」猜测。
func TestDingtalkStageDoneOnRealPages(t *testing.T) {
	stages := platformStages(Portals["dingtalk"])
	byID := map[string]setupStage{}
	for _, s := range stages {
		byID[s.ID] = s
	}
	if !byID["publish"].Done(dingVersionText) {
		t.Fatal("版本表格有「版本状态 + 已上线」应判 already_done")
	}
	if byID["publish"].Done(dingPermissionText) {
		t.Fatal("权限页不应被误判为已发布")
	}
	if byID["enable_perms"].Done(dingPermissionText) {
		t.Fatal("存在「未开通」权限时不应判已完成")
	}
	allOpen := "批量申请\n权限点code\n" +
		"Contact.User.Read 无需审批 已开通\n" +
		"Calendar.Event.Read 无需审批 已开通\n" +
		"snsapi_base 无需审批 已开通 默认开通"
	if !byID["enable_perms"].Done(allOpen) {
		t.Fatal("声明的权限点全部「已开通」时应判已完成")
	}
	// 只开了一部分、或还有别的权限未开通,都不影响幂等判断(不要求 23 项全开)。
	partial := allOpen + "\nContact.User.mobile 无需审批 未开通 立即开通"
	if !byID["enable_perms"].Done(partial) {
		t.Fatal("只按声明的 Perms 判断,不应被其他未开通权限拖住")
	}
	// 2026-11-11 起钉钉改为受 confirmed 门保护的自动提交,不再 not_required。
	for _, s := range stages {
		if s.ApplyNote != "" {
			t.Fatalf("%s 仍在做 not_required 降级: %s", s.ID, s.ApplyNote)
		}
		if len(opsForScope(s.Action, "apply")) == 0 {
			t.Fatalf("%s 缺 apply Ops", s.ID)
		}
		if s.PlanLine == "" || !strings.Contains(s.PlanLine, "记录") {
			t.Fatalf("%s 的确认清单未提示会在企业内产生记录: %q", s.ID, s.PlanLine)
		}
	}
	// 20:20 扫码后只读复核:点击一步生效、无二次确认框(旧版「可能有确认框」
	// 已证伪);但新增的逐 code 状态复核 Ops 未经真机执行,仍须 applied_unverified。
	perms := byID["enable_perms"]
	for _, want := range []string{"一步生效", "无二次确认框", "未完整验证", "不重试提交"} {
		if !strings.Contains(perms.ApplyWarning, want) {
			t.Fatalf("权限阶段未如实标注端到端验证边界 %q: %q", want, perms.ApplyWarning)
		}
	}
	if strings.Contains(perms.ApplyWarning, "可能出现的二次确认框") {
		t.Fatalf("二次确认框已被真机证伪,不应继续写成待确认: %q", perms.ApplyWarning)
	}
}

// 每个 Match 都要能在真机可见文本上命中;旧版推测按钮不得残留。
func TestDingtalkStageOpsUseRealMenus(t *testing.T) {
	realTexts := []string{
		"凭证与基础信息", "权限管理", "版本管理与发布", "批量申请",
		"创建新版本", "权限点code", "版本状态", "应用凭证", "保存",
		dingBaseinfoText, dingPermissionText, dingPermissionTextAfterOpen,
		dingVersionText, dingVersionFormText,
		// apply 成功后的目标状态(真机行结构 + 「已开通」)。状态复核 Ops 等的
		// 就是这个文本,不是推测按钮;Calendar.Event.Read 当前真机仍未开通,
		// 所以不能要求它在「开通前」样本里也命中。
		"Contact.User.Read 无需审批 已开通 移除权限\n" +
			"Calendar.Event.Read 无需审批 已开通 移除权限\n" +
			"snsapi_base 无需审批 已开通 默认开通",
	}
	for _, s := range platformStages(Portals["dingtalk"]) {
		for _, op := range s.Action.Ops {
			if op.Match == "" {
				continue
			}
			re := regexp.MustCompile(op.Match)
			hit := false
			for _, txt := range realTexts {
				if re.MatchString(txt) {
					hit = true
					break
				}
			}
			if !hit {
				t.Fatalf("%s 的 Match /%s/ 在真机文本上无命中(推测选择器?)", s.ID, op.Match)
			}
			for _, bad := range []string{"创建版本|新建版本", "确定|确认|提交|申请", "权限申请|权限配置"} {
				if strings.Contains(op.Match, bad) {
					t.Fatalf("%s 残留旧推测 Match %q", s.ID, op.Match)
				}
			}
		}
	}
}

// 钉钉权限探针不再串出飞书 scope。
func TestProbePermsUnverifiedPerPlatform(t *testing.T) {
	d := ProbePerms("dingtalk")
	if d.Status != "unsupported" {
		t.Fatalf("钉钉探针应为 unsupported,实际 %s", d.Status)
	}
	got := strings.Join(d.Unverified, ",")
	if got != strings.Join(Portals["dingtalk"].Perms, ",") {
		t.Fatalf("钉钉 unverified 应为自己的 Perms,实际 %q", got)
	}
	if strings.Contains(got, "im:message") || strings.Contains(got, "bitable:app") {
		t.Fatalf("钉钉探针串入飞书 scope: %q", got)
	}
	if strings.Join(permsUnverified("feishu"), ",") != strings.Join(defaultPerms, ",") {
		t.Fatal("飞书 unverified 应保持默认权限集")
	}
	// 真机 /permission 页列的是英文 code;中文飞书式标签同样属于错误标注。
	for _, bad := range []string{"通讯录只读", "消息推送", "SmartForm"} {
		if strings.Contains(got, bad) {
			t.Fatalf("钉钉探针仍带推测的中文权限标签 %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "Contact.User.Read") {
		t.Fatalf("钉钉探针应带真机权限点 code: %q", got)
	}
	// 未声明 Perms 的平台也不能回退到飞书默认集。
	if u := permsUnverified("tencent-docs"); len(u) != 0 {
		t.Fatalf("腾讯文档不应回退飞书 scope: %v", u)
	}
}

// gate 只标已验证项:tab 导航实测过,但创建/权限/发布表单仍未校准。
func TestSupportedPlatformsTabsVerified(t *testing.T) {
	for _, x := range SupportedPlatforms(nil) {
		if x.TabsVerified != (x.ID == "dingtalk") {
			t.Fatalf("%s 的 tabs_verified 不实: %+v", x.ID, x)
		}
		if x.DomVerified != (x.ID == "feishu") {
			t.Fatalf("%s 的 dom_verified 不实: %+v", x.ID, x)
		}
	}
}

// apply Ops 必须逐字段对齐真机表单:权限按 code 定位行、版本用 #version/#description。
func TestDingtalkApplyOpsUseRealFormFields(t *testing.T) {
	byID := map[string]setupStage{}
	for _, s := range platformStages(Portals["dingtalk"]) {
		byID[s.ID] = s
	}

	permsApply := opsForScope(byID["enable_perms"].Action, "apply")
	codes := Portals["dingtalk"].ApplyPerms
	// 2026-11-11 真机第 5 步 selector_miss 后补了两步:每个 code 先等搜索框
	// 重新渲染(点击后的异步刷新),末尾再等该行变「已开通」作为下一个 code
	// 的稳定锚点;clickInRow 带 SkipIfMatch 让已开通的行幂等跳过。
	if len(permsApply) != 5*len(codes) {
		t.Fatalf("权限 apply 应为每个 code 5 步(等搜索框→过滤→等行→点行内按钮→复核已开通),实际 %d: %+v", len(permsApply), permsApply)
	}
	for i, code := range codes {
		b := 5 * i
		waitBox, fill, wait, click, opened := permsApply[b], permsApply[b+1], permsApply[b+2], permsApply[b+3], permsApply[b+4]
		if waitBox.Kind != "waitVisible" || !strings.Contains(waitBox.Target, "权限或接口") {
			t.Fatalf("%s 未等搜索框重新渲染(点击后异步刷新): %+v", code, waitBox)
		}
		if fill.Kind != "fill" || fill.Text != code || !strings.Contains(fill.Target, "权限或接口") {
			t.Fatalf("%s 未按真机搜索框过滤: %+v", code, fill)
		}
		if wait.Kind != "waitVisible" || wait.Target != "tr[data-row-key]" || wait.Match != regexp.QuoteMeta(code) {
			t.Fatalf("%s 未等真机权限行: %+v", code, wait)
		}
		if click.Kind != "clickInRow" || click.Target != "tr[data-row-key]" ||
			click.Match != regexp.QuoteMeta(code) || click.Text != "立即开通" {
			t.Fatalf("%s 未按行点「立即开通」: %+v", code, click)
		}
		openedRe := regexp.QuoteMeta(code) + `\s*无需审批\s*已开通`
		if click.SkipIfMatch != openedRe {
			t.Fatalf("%s 缺幂等 SkipIfMatch(已开通的行按钮是「移除权限」): %+v", code, click)
		}
		if opened.Kind != "waitVisible" || opened.Target != "tr[data-row-key]" || opened.Match != openedRe {
			t.Fatalf("%s 缺逐 code 开通状态复核: %+v", code, opened)
		}
	}
	for _, code := range codes {
		if code == "snsapi_base" {
			t.Fatal("snsapi_base 真机默认已开通、行内无「立即开通」,不得进 ApplyPerms")
		}
	}

	pub := byID["publish"]
	pubApply := opsForScope(pub.Action, "apply")
	matches, full := "", ""
	for _, op := range pubApply {
		matches += op.Match + "|"
		full += op.Kind + "|" + op.Target + "|" + op.Text + "\n"
		for _, bad := range []string{"发布", "确定", "提交", "申请"} {
			if strings.Contains(op.Match, bad) {
				t.Fatalf("版本 apply 含未真机校准的按钮 %q: %+v", bad, op)
			}
		}
	}
	for _, want := range []string{"创建新版本", "保存"} {
		if !strings.Contains(matches, want) {
			t.Fatalf("版本 apply 缺真机按钮 %q: %s", want, matches)
		}
	}
	for _, want := range []string{"input#version", "textarea#description"} {
		if !strings.Contains(full, want) {
			t.Fatalf("版本 apply 缺真机表单字段 %q: %s", want, full)
		}
	}
	if !strings.Contains(pub.ApplyWarning, "发布") || !strings.Contains(pub.ApplyWarning, "不自动点击") {
		t.Fatalf("未如实标注发布按钮边界: %q", pub.ApplyWarning)
	}
	failed, skipped := setupStageOutcomes([]SetupStep{
		{ID: "capture", Status: "done"},
		{ID: "publish", Status: "applied_unverified"},
	})
	if len(failed) != 0 || len(skipped) != 1 {
		t.Fatalf("applied_unverified 应计入 skipped 而非失败: %v / %v", failed, skipped)
	}
}

// 2026-11-11 19:38 真机 setup(dingtalk, confirmed=true) 的返回结果(脱敏)。
// 用途:把「端到端实测没有跑通」这个事实固化成回归样本,防止以后有人把
// publish=already_done 当成「版本已创建」、或把 selector_miss 洗成 done。
// 原始留证:/tmp/setup-true-20261111.json;执行耗时 8.85s(<25s 会话上限)。
const dingE2ESteps = `[
 {"id":"open_app","status":"already_open"},
 {"id":"capture","status":"incomplete","message":"Client Secret 只显示掩码"},
 {"id":"enable_perms","status":"selector_miss","message":"第 5 步 waitVisible tr[data-row-key] 按 /Calendar.Event.Read/ 未匹配"},
 {"id":"publish","status":"already_done"}
]`

func TestDingtalkE2EResultIsHonest(t *testing.T) {
	steps := []SetupStep{}
	if err := json.Unmarshal([]byte(dingE2ESteps), &steps); err != nil {
		t.Fatal(err)
	}
	failed, skipped := setupStageOutcomes(steps)
	// capture 是硬前提,非 done 必须计入 failed;enable_perms 的 selector_miss 同理。
	if len(failed) != 2 || failed[0] != "capture" || failed[1] != "enable_perms" {
		t.Fatalf("真机 partial 结果必须如实计入 failed,不能洗成 done: %v / %v", failed, skipped)
	}
	if len(skipped) != 0 {
		t.Fatalf("already_done 不该计入 skipped: %v", skipped)
	}
	// publish 的 already_done 只代表「已有上线版本、幂等跳过」,不代表本次创建了版本。
	pub := platformStages(Portals["dingtalk"])[1]
	if pub.ID != "publish" || !pub.Done(dingVersionText) {
		t.Fatal("真机版本页应判 already_done")
	}
	if !strings.Contains(pub.PlanLine, "幂等跳过") || !strings.Contains(pub.PlanLine, "不产生新记录") {
		t.Fatalf("确认清单必须写明「已有上线版本则跳过、不生成版本」,避免确认门失真: %q", pub.PlanLine)
	}
	// 「保存」后的发布按钮与创建应用表单仍未采集,dom_verified 必须保持 false。
	// 二次确认框已由 20:20 只读复核证伪(点击即生效、无弹窗),不再作为理由。
	for _, p := range SupportedPlatforms(nil) {
		if p.ID == "dingtalk" && p.DomVerified {
			t.Fatal("端到端未跑通(版本表单没进、发布按钮没采集),钉钉 dom_verified 不能置 true")
		}
	}
}

// 2026-11-11 20:20 真机只读复核:上次 confirmed=true 只开成功第 1 个权限点,
// 权限页现在是「部分开通」状态。重跑时已开通的行按钮变成「移除权限」,
// SkipIfMatch 必须让它幂等跳过,否则第一个 code 就 selector_miss、
// 第二个 code 永远轮不到;同时 Done 不能把部分开通误判成阶段已完成。
func TestDingtalkPermsApplyIdempotentAfterPartialOpen(t *testing.T) {
	stages := platformStages(Portals["dingtalk"])
	perms := Portals["dingtalk"].ApplyPerms
	apply := opsForScope(stages[0].Action, "apply")
	if len(apply) != 5*len(perms) {
		t.Fatalf("apply 序列长度异常: %d", len(apply))
	}
	lines := strings.Split(dingPermissionTextAfterOpen, "\n")
	for i, code := range perms {
		skip := regexp.MustCompile(apply[5*i+3].SkipIfMatch)
		opened := regexp.MustCompile(apply[5*i+4].Match)
		found := false
		for _, line := range lines {
			if !strings.Contains(line, code) {
				continue
			}
			found = true
			wantHit := strings.Contains(line, "已开通")
			if got := skip.MatchString(line); got != wantHit {
				t.Fatalf("%s 行「%s」SkipIfMatch=%v,期望 %v", code, line, got, wantHit)
			}
			if got := opened.MatchString(line); got != wantHit {
				t.Fatalf("%s 行「%s」状态复核=%v,期望 %v", code, line, got, wantHit)
			}
		}
		if !found {
			t.Fatalf("真机复核样本缺权限点 %s", code)
		}
	}
	if stages[0].Done(dingPermissionTextAfterOpen) {
		t.Fatal("部分开通时 Done 不能判阶段已完成,否则第二个权限点会被跳过")
	}
	// 二次确认框已证伪,ApplyWarning 不该再写「可能有确认框待人工确认」。
	for _, bad := range []string{"可能出现的二次确认框", "若有待确认弹窗请人工完成"} {
		if strings.Contains(stages[0].ApplyWarning, bad) {
			t.Fatalf("ApplyWarning 仍沿用已被证伪的旧结论: %q", stages[0].ApplyWarning)
		}
	}
}

func TestSetupStageOutcomesNotRequired(t *testing.T) {
	failed, skipped := setupStageOutcomes([]SetupStep{
		{ID: "capture", Status: "done"},
		{ID: "enable_perms", Status: "not_required"},
		{ID: "publish", Status: "already_done"},
	})
	if len(failed) != 0 {
		t.Fatalf("not_required 不应算失败: %v", failed)
	}
	if len(skipped) != 1 || skipped[0] != "enable_perms" {
		t.Fatalf("not_required 应计入 skipped: %v", skipped)
	}
}
