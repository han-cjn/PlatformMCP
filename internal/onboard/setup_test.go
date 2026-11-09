package onboard

import (
	"strings"
	"testing"
)

// TestSetupUnsupportedPlatform 非飞书平台不定义配置/发布动作,应快速降级且不碰浏览器。
func TestSetupUnsupportedPlatform(t *testing.T) {
	res, err := Setup("dingtalk", "", false, nil)
	if err != nil {
		t.Fatalf("unsupported 不应报错: %v", err)
	}
	if res.Phase != "unsupported" || !strings.Contains(res.Next, "capture") {
		t.Fatalf("应降级为人工配置 + capture: %+v", res)
	}
}

// TestSetupPlan 确认门必须把有副作用的动作(创建应用/发布版本/权限)明确列出来。
func TestSetupPlan(t *testing.T) {
	withCreate := strings.Join(setupPlan("", true), "|")
	for _, want := range []string{"创建企业自建应用", defaultAppName, "im:message", "bitable:app", "1.0.0", "自动打开应用详情页"} {
		if !strings.Contains(withCreate, want) {
			t.Fatalf("确认清单缺少 %q: %s", want, withCreate)
		}
	}
	if strings.Contains(strings.Join(setupPlan("", false), "|"), "创建企业自建应用") {
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
