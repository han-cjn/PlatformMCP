package onboard

import "testing"

// TestClassifyPagesFeishu 固化「扫码监听」的 URL 信号判定:这三个阶段的
// 跳转是 hub.onboard.watch 唯一的依据,判错会导致 Agent 反复催用户或提前抓凭据。
func TestClassifyPagesFeishu(t *testing.T) {
	p := Portals["feishu"]
	cases := []struct {
		name       string
		pages      []string
		wantState  string
		wantDetail bool
	}{
		{
			name:      "停在扫码登录页",
			pages:     []string{"https://accounts.feishu.cn/accounts/page/login?app_id=7&redirect_uri=https%3A%2F%2Fopen.feishu.cn%2Fapp"},
			wantState: "waiting_login",
		},
		{
			name:      "扫码成功跳回开放平台列表页",
			pages:     []string{"https://open.feishu.cn/app"},
			wantState: "logged_in",
		},
		{
			name: "登录页标签残留不应误判为未登录",
			pages: []string{
				"https://accounts.feishu.cn/accounts/page/login?app_id=7",
				"https://open.feishu.cn/app",
			},
			wantState: "logged_in",
		},
		{
			name:      "跳回 URL 带 login 查询参数仍算已登录",
			pages:     []string{"https://open.feishu.cn/app?from=login"},
			wantState: "logged_in",
		},
		{
			name:       "进入应用详情页",
			pages:      []string{"https://open.feishu.cn/app/cli_abc123/baseinfo"},
			wantState:  "logged_in",
			wantDetail: true,
		},
		{
			name:      "列表页查询参数不是详情页",
			pages:     []string{"https://open.feishu.cn/app?from=login"},
			wantState: "logged_in",
		},
		{
			name:      "创建页不算详情页",
			pages:     []string{"https://open.feishu.cn/app/create"},
			wantState: "logged_in",
		},
		{
			name:      "无关页面",
			pages:     []string{"https://example.com/"},
			wantState: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, _ := classifyPages(p, tc.pages)
			if state != tc.wantState || detail != tc.wantDetail {
				t.Fatalf("state=%q in_app_detail=%v,期望 state=%q in_app_detail=%v",
					state, detail, tc.wantState, tc.wantDetail)
			}
		})
	}
}

// TestCaptureOps 确认 Capture 只保留读取类动作:用户已停在详情页时,
// navigate 会导走页面、waitURL 会白等 60s、screenshot 会往工作目录写文件。
func TestCaptureOps(t *testing.T) {
	act := credAction(Portals["feishu"])
	if act == nil {
		t.Fatal("飞书清单缺少凭据抓取动作")
	}
	ops := captureOps(act)
	if len(ops) == 0 {
		t.Fatal("过滤后不应为空")
	}
	for _, op := range ops {
		switch op.Kind {
		case "navigate", "waitURL", "screenshot":
			t.Fatalf("不应保留 %s 动作", op.Kind)
		}
	}
	hasReadText := false
	for _, op := range ops {
		if op.Kind == "readText" && op.ExtractKey != "" {
			hasReadText = true
		}
	}
	if !hasReadText {
		t.Fatal("过滤后必须保留 readText 凭据抽取动作")
	}
}
