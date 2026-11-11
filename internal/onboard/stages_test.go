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
			if probe[0].Kind != "waitVisible" || probe[0].Target != menuCandidateSelector || probe[0].Match == "" {
				t.Fatalf("%s/%s 的 probe 首步应为统一选择器的带文本等待: %+v", id, s.ID, probe[0])
			}
			if _, err := regexp.Compile(probe[0].Match); err != nil {
				t.Fatalf("%s/%s 菜单正则非法: %v", id, s.ID, err)
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
		"自动创建新版本并保存", "产生开通记录", "产生版本记录"} {
		if !strings.Contains(ding, want) {
			t.Fatalf("钉钉 plan 缺 %q: %s", want, ding)
		}
	}
	for _, bad := range []string{"im:message", "创建版本 1.0.0 并申请发布", "开通权限:", "核对版本发布状态", "不自动开通", "通讯录只读", "消息推送", "SmartForm"} {
		if strings.Contains(ding, bad) {
			t.Fatalf("钉钉 plan 仍含旧版只读/推测文案 %q: %s", bad, ding)
		}
	}
	wecom := strings.Join(setupPlan(Portals["wecom"], "", false), "|")
	if !strings.Contains(wecom, "收发消息") || !strings.Contains(wecom, "Bot ID") {
		t.Fatalf("企微 plan 未按平台声明: %s", wecom)
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

func TestSetupSupportedAllPlatforms(t *testing.T) {
	for _, x := range SupportedPlatforms(nil) {
		if !x.SetupSupported || !x.BrowserCapture {
			t.Fatalf("%s 应支持 setup 与浏览器抓凭据: %+v", x.ID, x)
		}
		if x.DomVerified != (x.ID == "feishu") {
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
