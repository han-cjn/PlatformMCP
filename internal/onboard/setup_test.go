package onboard

import (
	"strings"
	"testing"
)

// TestSetupUnsupportedPlatform L3 后四平台都有 setup 链路:无调试 Chrome 时
// 应快速返回 browser_missing 而不是 unsupported,且不让用户切终端。
func TestSetupUnsupportedPlatform(t *testing.T) {
	withUnreachableCDP(t)
	res, err := Setup("dingtalk", "", false, nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res.Phase != "browser_missing" {
		t.Fatalf("期望 browser_missing,实际 %+v", res)
	}
	if strings.Contains(res.Next, "go run") {
		t.Fatalf("指引不应让用户切终端: %+v", res)
	}
}

// TestSetupPlan 确认门必须把有副作用的动作(创建应用/发布版本/权限)明确列出来。
func TestSetupPlan(t *testing.T) {
	withCreate := strings.Join(setupPlan(Portals["feishu"], "", true), "|")
	for _, want := range []string{"创建应用", defaultAppName, "im:message", "bitable:app", "1.0.0", "自动打开应用详情页", "App ID"} {
		if !strings.Contains(withCreate, want) {
			t.Fatalf("确认清单缺少 %q: %s", want, withCreate)
		}
	}
	if strings.Contains(strings.Join(setupPlan(Portals["feishu"], "", false), "|"), "创建应用") {
		t.Fatal("已有应用时不应把创建应用列进确认清单")
	}
}

// TestOpsForScope probe 只读、apply 才产生副作用,两者必须严格分离且各阶段都有幂等探测。
func TestOpsForScope(t *testing.T) {
	stages := feishuSetupStages()
	if len(stages) != 2 {
		t.Fatalf("应有权限与发布两个阶段,实际 %d", len(stages))
	}
	for _, s := range stages {
		probe := opsForScope(s.Action, "probe")
		apply := opsForScope(s.Action, "apply")
		if len(probe) == 0 || len(apply) == 0 {
			t.Fatalf("%s 的 probe/apply 不得为空: probe=%d apply=%d", s.ID, len(probe), len(apply))
		}
		hasReadPage := false
		for _, op := range probe {
			switch op.Kind {
			case "fill", "navigate", "screenshot", "waitURL":
				t.Fatalf("%s 的 probe 阶段出现非只读操作 %s", s.ID, op.Kind)
			case "readPage":
				hasReadPage = true
			}
		}
		if !hasReadPage {
			t.Fatalf("%s 缺少 readPage,无法做幂等检测", s.ID)
		}
	}
}

// TestSetupStageDoneMarkers 幂等判定:截图里的「当前修改均已发布」必须让发布阶段跳过,
// 权限必须三项齐全才算已开通,避免误跳过造成权限缺失。
func TestSetupStageDoneMarkers(t *testing.T) {
	stages := feishuSetupStages()
	publish, perms := stages[1].Done, stages[0].Done
	if !publish("个人测试模版 已启用 当前修改均已发布") {
		t.Fatal("已发布状态应命中幂等判定")
	}
	if publish("版本管理与发布 创建版本 申请发布") {
		t.Fatal("仅出现菜单文字不应误判为已发布")
	}
	if !perms("im:message 已开通 im:chat 已开通 bitable:app 已开通") {
		t.Fatal("三项权限齐全应命中幂等判定")
	}
	if perms("im:message 已开通") {
		t.Fatal("权限不齐不应误判为已完成")
	}
}

// TestRunSetupStageAPIShortCircuit A:API 探针通过或无法确认时,enable_perms
// 必须当场返回,不得打开浏览器、不得点任何按钮(否则会在权限已开通时重复提交)。
func TestRunSetupStageAPIShortCircuit(t *testing.T) {
	p, ok := FindPortal("feishu")
	if !ok {
		t.Fatal("缺少 feishu 清单")
	}
	stage := feishuSetupStages()[0]
	for _, tc := range []struct{ status, want string }{
		{"api_ready", "api_verified"},
		{"unknown", "skipped"},
		{"no_credentials", "skipped"},
		{"unsupported", "skipped"},
	} {
		probe := &PermProbeResult{Status: tc.status, Checked: []string{"im:chat"},
			Unverified: []string{"im:message"}, Detail: "mock"}
		step, candidates := runSetupStage(stage, p, probe)
		if step.Status != tc.want {
			t.Fatalf("probe=%s 应短路为 %s,实际 %s: %+v", tc.status, tc.want, step.Status, step)
		}
		if len(candidates) != 0 {
			t.Fatalf("短路时不应打开浏览器采集候选: %v", candidates)
		}
	}
}

// TestSetupStageSelectorsConsistent B:probe/click 的选择器必须覆盖 button,
// 且与 collectCandidates 的采集范围同源;并先等菜单文本渲染再点。
func TestSetupStageSelectorsConsistent(t *testing.T) {
	groups := strings.Split(menuCandidateSelector, ", ")
	hasButton := false
	for _, g := range groups {
		if g == "button" {
			hasButton = true
		}
	}
	if !hasButton {
		t.Fatalf("选择器集合必须含 button(实测踩坑): %s", menuCandidateSelector)
	}
	for _, s := range feishuSetupStages() {
		probeOps := opsForScope(s.Action, "probe")
		if len(probeOps) == 0 || probeOps[0].Kind != "waitVisible" || probeOps[0].Match == "" {
			t.Fatalf("%s 的 probe 第一步应为带 Match 的 waitVisible(等 SPA 渲染): %+v", s.ID, probeOps)
		}
		for _, op := range probeOps {
			if op.Kind == "click" && op.Target != menuCandidateSelector {
				t.Fatalf("%s 的 probe click 选择器与候选采集不一致: %s", s.ID, op.Target)
			}
		}
	}
}

// TestSetupStageOutcomesIndependent C:一个阶段失败不能掩盖另一个阶段的成功,
// 也不能把「跳过」与「失败」混为一谈。
func TestSetupStageOutcomesIndependent(t *testing.T) {
	steps := []SetupStep{
		{ID: "open_app", Status: "already_open"},
		{ID: "capture", Status: "done"},
		{ID: "enable_perms", Status: "selector_miss"},
		{ID: "publish", Status: "already_done"},
		{ID: "extra", Status: "skipped"},
	}
	failed, skipped := setupStageOutcomes(steps)
	if strings.Join(failed, ",") != "enable_perms" {
		t.Fatalf("只有失败阶段应进 failed: %v", failed)
	}
	if strings.Join(skipped, ",") != "extra" {
		t.Fatalf("skipped 不应被当成失败: %v", skipped)
	}
	// 凭据阶段非 done 必须算 failed,否则后续阶段成功会把整体洗成 done。
	failed2, _ := setupStageOutcomes([]SetupStep{
		{ID: "capture", Status: "capture_failed"},
		{ID: "publish", Status: "already_done"},
	})
	if strings.Join(failed2, ",") != "capture" {
		t.Fatalf("capture 失败必须进 failed: %v", failed2)
	}
}

// TestAbsoluteAppURL 列表页 href 常为相对路径,补成绝对 URL 才能直接打开详情页。
func TestAbsoluteAppURL(t *testing.T) {
	p, ok := FindPortal("feishu")
	if !ok {
		t.Fatal("缺少 feishu 清单")
	}
	cases := []struct{ in, want string }{
		{"/app/cli_abc/baseinfo", "https://open.feishu.cn/app/cli_abc/baseinfo"},
		{"app/cli_abc/baseinfo", "https://open.feishu.cn/app/cli_abc/baseinfo"},
		{"https://open.feishu.cn/app/cli_abc/baseinfo", "https://open.feishu.cn/app/cli_abc/baseinfo"},
	}
	for _, c := range cases {
		if got := absoluteAppURL(p, c.in); got != c.want {
			t.Fatalf("absoluteAppURL(%q)=%q,期望 %q", c.in, got, c.want)
		}
	}
}
