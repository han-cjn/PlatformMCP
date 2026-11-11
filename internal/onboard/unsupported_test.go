package onboard

import (
	"strings"
	"testing"
)

// assertNoDeadEnd 校验 unsupported 降级指引:必须指向 set_credentials,
// 不能让用户切终端,也不能把 Agent 推回同样不支持的 capture(旧版死路)。
func assertNoDeadEnd(t *testing.T, where, next string) {
	t.Helper()
	if !strings.Contains(next, "set_credentials") {
		t.Fatalf("%s 的 next 未指向 set_credentials: %s", where, next)
	}
	if strings.Contains(next, "go run") || strings.Contains(next, "终端执行") {
		t.Fatalf("%s 的 next 仍让用户切终端: %s", where, next)
	}
	if strings.Contains(next, "调用 hub.onboard.capture") || strings.Contains(next, "转去调 capture") {
		t.Fatalf("%s 的 next 又推回不支持的 capture: %s", where, next)
	}
}

// TestSetupBrowserMissingNoDeadEnd L3 后四平台的 setup 不再返回 unsupported;
// 无调试 Chrome 时统一返回 browser_missing,且指引里绝不能出现终端命令。
func TestSetupBrowserMissingNoDeadEnd(t *testing.T) {
	withUnreachableCDP(t)
	for _, id := range []string{"dingtalk", "wecom", "tencent-docs"} {
		res, err := Setup(id, "", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Phase != "browser_missing" {
			t.Fatalf("%s 期望 browser_missing,实际 %s", id, res.Phase)
		}
		if strings.Contains(res.Next, "go run") || strings.Contains(res.Next, "终端执行") {
			t.Fatalf("%s 的 next 仍让用户切终端: %s", id, res.Next)
		}
	}
}

func TestCreateAppUnsupportedNoDeadEnd(t *testing.T) {
	// 只有 token_page 平台没有创建动作;应用控制台类平台已各自声明 Ops。
	res, err := CreateApp("tencent-docs", "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != "unsupported" {
		t.Fatalf("期望 unsupported,实际 %s", res.Phase)
	}
	assertNoDeadEnd(t, "create_app/tencent-docs", res.Next)
}

// TestCreateAppBrowserMissingNoTerminal 已声明创建动作的平台在无调试 Chrome 时
// 快速返回 browser_missing,指引里不能出现终端命令(PUA:同类降级路径一起验)。
func TestCreateAppBrowserMissingNoTerminal(t *testing.T) {
	withUnreachableCDP(t)
	for _, id := range []string{"dingtalk", "wecom"} {
		res, err := CreateApp(id, "", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Phase != "browser_missing" {
			t.Fatalf("%s 期望 browser_missing,实际 %s", id, res.Phase)
		}
		if strings.Contains(res.Next, "go run") || strings.Contains(res.Next, "终端执行") {
			t.Fatalf("%s 的 next 仍让用户切终端: %s", id, res.Next)
		}
	}
}

func TestCaptureUnsupportedNextNoDeadEnd(t *testing.T) {
	assertNoDeadEnd(t, "capture", unsupportedCaptureNext())
}

func TestPlatformHintsNoTerminalWizard(t *testing.T) {
	for _, id := range PlatformOrder() {
		hint := platformHints[id]
		if hint == "" {
			t.Fatalf("%s 缺少 hint", id)
		}
		if strings.Contains(hint, "Hub 可接管凭据") {
			t.Fatalf("%s 的 hint 仍暗示终端向导可接管凭据: %s", id, hint)
		}
		if id != "feishu" && !strings.Contains(hint, "set_credentials") {
			t.Fatalf("%s 的 hint 未给出 set_credentials 路径: %s", id, hint)
		}
	}
}
