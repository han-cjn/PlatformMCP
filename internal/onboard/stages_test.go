package onboard

import (
	"regexp"
	"strings"
	"testing"
)

// TestPlatformStagesPerPlatform L3 验收:应用控制台类平台都有权限/发布阶段,
// token_page 平台没有;每个阶段的 probe 只读、apply 有界,且脚本标记带平台前缀。
func TestPlatformStagesPerPlatform(t *testing.T) {
	wantCount := map[string]int{"feishu": 2, "dingtalk": 2, "wecom": 2, "tencent-docs": 0}
	for _, id := range PlatformOrder() {
		stages := platformStages(Portals[id])
		if len(stages) != wantCount[id] {
			t.Fatalf("%s 阶段数应为 %d,实际 %d", id, wantCount[id], len(stages))
		}
		ids := map[string]bool{}
		for _, s := range stages {
			ids[s.ID] = true
			if s.Action == nil || s.Done == nil {
				t.Fatalf("%s/%s 缺 Action 或 Done", id, s.ID)
			}
			if !strings.HasPrefix(s.Action.Script, id+"@") {
				t.Fatalf("%s/%s 脚本缺平台前缀: %s", id, s.ID, s.Action.Script)
			}
			probe, apply := opsForScope(s.Action, "probe"), opsForScope(s.Action, "apply")
			if len(probe) == 0 || (len(apply) == 0 && s.ApplyNote == "") {
				t.Fatalf("%s/%s 的 probe 不得为空;无 apply 时必须带 ApplyNote 说明降级原因", id, s.ID)
			}
			if s.ApplyNote != "" && len(apply) > 0 {
				t.Fatalf("%s/%s 声明了 not_required 却仍保留会误提交的 apply Ops", id, s.ID)
			}
			if probe[0].Kind != "waitVisible" {
				t.Fatalf("%s/%s 的 probe 首步应为 waitVisible: %+v", id, s.ID, probe[0])
			}
			// 声明了 ApplyNote 的阶段是「纯只读整页文本核对」,不需要(也不该)
			// 依赖未校准的菜单选择器:首步等 body 即可,待匹配文本交给 Done 判定。
			if s.ApplyNote == "" {
				if probe[0].Target != menuCandidateSelector || probe[0].Match == "" {
					t.Fatalf("%s/%s 的 probe 首步应为统一选择器的带文本等待: %+v", id, s.ID, probe[0])
				}
				if _, err := regexp.Compile(probe[0].Match); err != nil {
					t.Fatalf("%s/%s 菜单正则非法: %v", id, s.ID, err)
				}
			}
			hasReadPage := false
			for _, op := range probe {
				switch op.Kind {
				case "fill", "navigate", "screenshot", "waitURL":
					t.Fatalf("%s/%s 的 probe 出现副作用操作 %s", id, s.ID, op.Kind)
				case "readPage":
					hasReadPage = true
				}
			}
			if !hasReadPage {
				t.Fatalf("%s/%s 缺 readPage,无法做幂等检测", id, s.ID)
			}
			for _, op := range apply {
				switch op.Kind {
				case "click", "fill":
				case "waitVisible": // 等表单页渲染(钉钉 /version/app_publish/new)
				case "clickInRow": // 按权限点 code 只点该行的「立即开通」
				default:
					t.Fatalf("%s/%s 的 apply 出现越界操作 %s", id, s.ID, op.Kind)
				}
				if op.Kind == "click" && op.Match == "" {
					t.Fatalf("%s/%s 的 apply click 必须按可见文本匹配: %+v", id, s.ID, op)
				}
				if op.Kind == "clickInRow" && (op.Match == "" || op.Text == "") {
					t.Fatalf("%s/%s 的 clickInRow 必须同时声明行正则与行内按钮文本: %+v", id, s.ID, op)
				}
			}
		}
		if wantCount[id] > 0 && (!ids["enable_perms"] || !ids["publish"]) {
			t.Fatalf("%s 缺权限或发布阶段: %v", id, ids)
		}
	}
}

func TestSetupPlanPerPlatform(t *testing.T) {
	feishu := strings.Join(setupPlan(Portals["feishu"], "", true), "|")
	for _, want := range []string{"创建应用", "mcp-hub", "im:message", "bitable:app", "1.0.0", "App ID", "App Secret"} {
		if !strings.Contains(feishu, want) {
			t.Fatalf("飞书 plan 缺 %q: %s", want, feishu)
		}
	}
	ding := strings.Join(setupPlan(Portals["dingtalk"], "", false), "|")
	// 2026-11-11 真机 /permission 页的权限点是英文 code;确认门必须写清
	// 「会自动点哪些权限点」以及「会在企业里留记录」,不能再说只读核对。
	for _, want := range []string{"Contact.User.Read", "Calendar.Event.Read", "AppKey", "AppSecret",
		"创建新版本并保存", "产生开通记录", "版本草稿记录"} {
		if !strings.Contains(ding, want) {
			t.Fatalf("钉钉 plan 缺 %q: %s", want, ding)
		}
	}
	for _, bad := range []string{"im:message", "创建版本 1.0.0 并申请发布", "开通权限:", "核对版本发布状态", "不自动开通", "通讯录只读", "消息推送", "SmartForm"} {
		if strings.Contains(ding, bad) {
			t.Fatalf("钉钉 plan 仍含旧版只读/推测文案 %q: %s", bad, ding)
		}
	}
	// 2026-11-11 端到端真机(confirmed=true):旧版 Done 检测「已上线」让 publish 永远跳过、
	// 永远创建不出新版本(钉钉允许多版本,创建是增量动作)。20:40 用户授权生成 1.0.1
	// 草稿,改为按「有待发布版本」幂等:无待发布版本时创建并保存,已存在待发布版本则跳过。
	for _, want := range []string{"已存在待发布版本则跳过"} {
		if !strings.Contains(ding, want) {
			t.Fatalf("钉钉 plan 未如实说明发布幂等条件 %q: %s", want, ding)
		}
	}
	wecom := strings.Join(setupPlan(Portals["wecom"], "", false), "|")
	if !strings.Contains(wecom, "收发消息") || !strings.Contains(wecom, "Bot ID") {
		t.Fatalf("企微 plan 未按平台声明: %s", wecom)
	}
	// 2026-11-12 真机创建 mcp-hub:企微智能机器人没有版本/申请发布概念,
	// 通用 app_console 模板曾让确认清单出现「创建版本 1.0.0 并申请发布」,
	// 会诱导 Agent 去点不存在(且不可逆)的发布按钮。
	for _, bad := range []string{"创建版本 1.0.0 并申请发布", "核对版本发布状态", "申请发布"} {
		if strings.Contains(wecom, bad) {
			t.Fatalf("企微 plan 仍含不适用的版本发布文案 %q: %s", bad, wecom)
		}
	}
	for _, want := range []string{"核对机器人启用状态", "无版本概念", "不点发布"} {
		if !strings.Contains(wecom, want) {
			t.Fatalf("企微 plan 缺只读启用状态说明 %q: %s", want, wecom)
		}
	}
	tencent := strings.Join(setupPlan(Portals["tencent-docs"], "", false), "|")
	for _, bad := range []string{"创建应用", "开通权限", "版本"} {
		if strings.Contains(tencent, bad) {
			t.Fatalf("腾讯文档 plan 不应含 %q: %s", bad, tencent)
		}
	}
	if !strings.Contains(tencent, "MCP Token") || !strings.Contains(tencent, "凭据授权页") {
		t.Fatalf("腾讯文档 plan 应说明授权页读 Token: %s", tencent)
	}
}

// TestWecomPublishStageNeverClicksIrreversibleButtons 锁死 2026-11-12 的修复:
// 企微后台 DOM 未端到端校准,发布/启用阶段只能只读核对,不得保留任何 apply Ops。
func TestWecomPublishStageNeverClicksIrreversibleButtons(t *testing.T) {
	var pub setupStage
	found := false
	for _, s := range platformStages(Portals["wecom"]) {
		if s.ID == "publish" {
			pub, found = s, true
			break
		}
	}
	if !found {
		t.Fatal("企微 publish 阶段缺失;确认清单与阶段声明需同步调整")
	}
	if pub.ApplyNote == "" || pub.PlanLine == "" {
		t.Fatal("企微 publish 必须同时声明 PlanLine(确认清单)与 ApplyNote(not_required 降级)")
	}
	if apply := opsForScope(pub.Action, "apply"); len(apply) > 0 {
		t.Fatalf("企微 publish 仍保留会误提交的 apply Ops: %+v", apply)
	}
	for _, op := range opsForScope(pub.Action, "probe") {
		switch op.Kind {
		case "waitVisible", "readPage":
		default:
			t.Fatalf("企微 publish 的 probe 出现非只读操作 %s", op.Kind)
		}
	}
	if strings.Contains(pub.PlanLine, "申请发布") || strings.Contains(pub.PlanLine, "1.0.0") {
		t.Fatalf("企微 publish 的 PlanLine 仍含版本发布文案: %q", pub.PlanLine)
	}
}

func TestSetupSupportedAllPlatforms(t *testing.T) {
	for _, x := range SupportedPlatforms(nil) {
		if !x.SetupSupported || !x.BrowserCapture {
			t.Fatalf("%s 应支持 setup 与浏览器抓凭据: %+v", x.ID, x)
		}
		want := x.ID == "feishu" || x.ID == "dingtalk"
		if x.DomVerified != want {
			t.Fatalf("%s 的 dom_verified 不实: %+v", x.ID, x)
		}
	}
}

// TestAPIProbeOnlyFeishu 非飞书平台没有权限校验 API,不能被 unsupported 探针
// 短路成 skipped;否则 UI 开权限阶段永远不会执行。
func TestAPIProbeOnlyFeishu(t *testing.T) {
	probe := &PermProbeResult{Status: "unsupported"}
	if !apiProbeApplies("enable_perms", "feishu", probe) {
		t.Fatal("飞书 enable_perms 应启用 API 探针")
	}
	for _, id := range []string{"dingtalk", "wecom", "tencent-docs"} {
		if apiProbeApplies("enable_perms", id, probe) {
			t.Fatalf("%s 不应被 API 探针短路", id)
		}
	}
	if apiProbeApplies("publish", "feishu", probe) {
		t.Fatal("publish 阶段不应走权限 API 探针")
	}
	if apiProbeApplies("enable_perms", "feishu", nil) {
		t.Fatal("probe 为 nil 时不应短路")
	}
}

// TestSetupAllPlatformsNoUnsupported 四平台 setup 都不再撞 unsupported 死路;
// 无调试 Chrome 时统一快速返回 browser_missing 且不含终端命令。
func TestSetupAllPlatformsNoUnsupported(t *testing.T) {
	withUnreachableCDP(t)
	for _, id := range PlatformOrder() {
		res, err := Setup(id, "", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Phase == "unsupported" {
			t.Fatalf("%s 仍返回 unsupported", id)
		}
		if res.Phase != "browser_missing" {
			t.Fatalf("%s 期望 browser_missing,实际 %s", id, res.Phase)
		}
		if strings.Contains(res.Next, "go run") || strings.Contains(res.Next, "终端执行") {
			t.Fatalf("%s 的 next 让用户切终端: %s", id, res.Next)
		}
	}
}
