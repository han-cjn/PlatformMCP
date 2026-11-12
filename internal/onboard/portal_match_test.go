package onboard

import (
	"regexp"
	"testing"
)

// TestAllPlatformsHaveCredAction L2 的核心验收:四个平台都挂了浏览器凭据动作,
// 因此 status 里的 browser_capture 应全部为 true。
func TestAllPlatformsHaveCredAction(t *testing.T) {
	for _, id := range PlatformOrder() {
		p := Portals[id]
		if p == nil {
			t.Fatalf("%s 缺少 Portal", id)
		}
		if p.Flow != "app_console" && p.Flow != "token_page" {
			t.Fatalf("%s 的 Flow 非法: %q", id, p.Flow)
		}
		if credAction(p) == nil {
			t.Fatalf("%s 没有 collect-creds 浏览器动作", id)
		}
		info := SupportedPlatforms(nil)
		for _, x := range info {
			if x.ID == id && !x.BrowserCapture {
				t.Fatalf("%s 的 browser_capture 应为 true", id)
			}
		}
	}
}

// TestCredentialOpsWellFormed 声明式 Ops 必须可编译、覆盖全部凭据字段,
// 且 readPage 出现在 extract 之前(否则 extract 拿不到页面文本)。
func TestCredentialOpsWellFormed(t *testing.T) {
	for _, id := range PlatformOrder() {
		act := credAction(Portals[id])
		keys := map[string]bool{}
		readPageSeen := false
		for _, op := range act.Ops {
			switch op.Kind {
			case "readPage":
				readPageSeen = true
			case "extract":
				if !readPageSeen {
					t.Fatalf("%s 的 extract 出现在 readPage 之前", id)
				}
				if _, err := regexp.Compile(op.Target); err != nil {
					t.Fatalf("%s extract 正则非法 %q: %v", id, op.Target, err)
				}
				keys[op.ExtractKey] = true
			case "readText", "copyToCred", "copyInFrame":
				keys[op.ExtractKey] = true
			}
		}
		for _, c := range PortalCreds[id] {
			if !keys[c.EnvKey] {
				t.Fatalf("%s 的凭据动作未覆盖 %s", id, c.EnvKey)
			}
		}
		// capture 过滤后仍要保留抽取动作,否则浏览器抓取形同虚设
		filtered := captureOps(act)
		hasExtract := false
		for _, op := range filtered {
			if op.Kind == "extract" || (op.Kind == "readText" && op.ExtractKey != "") ||
				(op.Kind == "copyToCred" && op.ExtractKey != "") ||
				(op.Kind == "copyInFrame" && op.ExtractKey != "") {
				hasExtract = true
			}
		}
		if !hasExtract {
			t.Fatalf("%s 的 captureOps 过滤后没有抽取动作", id)
		}
	}
}

func TestPortalDetailMatchPerPlatform(t *testing.T) {
	cases := []struct {
		platform string
		url      string
		want     bool
	}{
		{"feishu", "https://open.feishu.cn/app/cli_abc123/baseinfo", true},
		{"feishu", "https://open.feishu.cn/app", false},
		{"feishu", "https://open.feishu.cn/app/create", false},
		{"feishu", "https://accounts.feishu.cn/accounts/page/login", false},
		{"dingtalk", "https://open-dev.dingtalk.com/", false},
		// 2026-11-11 真机实测:/fe/app 是应用列表页,/fe/ai?...#/app/<uuid>/dev
		// 才是应用详情页。旧用例把推测的 /fe/app?appId= 当详情页,会掩盖串页。
		{"dingtalk", dingAppList, false},
		{"dingtalk", dingDetail, true},
		{"dingtalk", "https://open-dev.dingtalk.com/fe/ai?appId=dingabc123", true},
		{"wecom", "https://work.weixin.qq.com/", false},
		// 2026-11-12 真机实测:详情页是 /wework_admin/frame#/aiHelper/detail?aibotid=<数字>,
		// 不再是旧的 /ai/robot?botid=(那是推测值)。
		{"wecom", "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/detail?aibotid=33776999461281670", true},
		{"wecom", "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/list?from=manage_tools&tab=manage", false},
		{"tencent-docs", "https://docs.qq.com/open/auth/mcp.html", true},
		{"tencent-docs", "https://docs.qq.com/open/auth/mcp.html?from=login", true},
		{"tencent-docs", "https://docs.qq.com/desktop", false},
	}
	for _, c := range cases {
		if got := portalDetailMatch(Portals[c.platform], c.url); got != c.want {
			t.Errorf("portalDetailMatch(%s, %q) = %v, 期望 %v", c.platform, c.url, got, c.want)
		}
	}
}

func TestPortalIsLoginPagePerPlatform(t *testing.T) {
	cases := []struct {
		platform string
		url      string
		want     bool
	}{
		{"feishu", "https://accounts.feishu.cn/accounts/page/login", true},
		{"dingtalk", "https://login.dingtalk.com/", true},
		{"dingtalk", "https://open-dev.dingtalk.com/fe/app?appId=dingabc123", false},
		{"wecom", "https://login.work.weixin.qq.com/wwlogin", true},
		{"wecom", "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/detail?aibotid=33776999461281670", false},
		{"tencent-docs", "https://ssl.ptlogin2.qq.com/ptlogin", true},
	}
	for _, c := range cases {
		if got := portalIsLoginPage(Portals[c.platform], c.url); got != c.want {
			t.Errorf("portalIsLoginPage(%s, %q) = %v, 期望 %v", c.platform, c.url, got, c.want)
		}
	}
}

func TestClassifyPagesOtherPlatforms(t *testing.T) {
	cases := []struct {
		platform   string
		pages      []string
		wantState  string
		wantDetail bool
	}{
		{"dingtalk", []string{"https://login.dingtalk.com/"}, "waiting_login", false},
		{"dingtalk", []string{"https://open-dev.dingtalk.com/"}, "logged_in", false},
		{"dingtalk", []string{dingAppList}, "logged_in", false},
		{"dingtalk", []string{dingDetail}, "logged_in", true},
		{"wecom", []string{"https://login.work.weixin.qq.com/wwlogin"}, "waiting_login", false},
		{"wecom", []string{"https://work.weixin.qq.com/wework_admin/frame#/aiHelper/detail?aibotid=33776999461281670"}, "logged_in", true},
		{"tencent-docs", []string{"https://docs.qq.com/open/auth/mcp.html"}, "logged_in", true},
	}
	for _, c := range cases {
		state, detail, _ := classifyPages(Portals[c.platform], c.pages)
		if state != c.wantState || detail != c.wantDetail {
			t.Errorf("%s %v -> state=%q detail=%v, 期望 %q/%v",
				c.platform, c.pages, state, detail, c.wantState, c.wantDetail)
		}
	}
}

func TestPortalAppID(t *testing.T) {
	cases := []struct{ platform, url, want string }{
		{"feishu", "https://open.feishu.cn/app/cli_abc123/baseinfo", "cli_abc123"},
		{"dingtalk", dingDetail, "1f2e47b9-a105-48f6-9cdc-45b32563a88e"},
		{"wecom", "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/detail?aibotid=33776999461281670", "33776999461281670"},
	}
	for _, c := range cases {
		if got := portalAppID(Portals[c.platform], c.url); got != c.want {
			t.Errorf("portalAppID(%s, %q) = %q, 期望 %q", c.platform, c.url, got, c.want)
		}
	}
}

// TestExtractOp 验证新增的零 DOM 凭据抽取动作:正常/非法正则/无匹配/缺 readPage。
func TestExtractOp(t *testing.T) {
	page := "MCP Token\n您的令牌: tok_abcdefghijklmnopqrstuvwxyz\n请妥善保管"
	op := Op{Kind: "extract", Target: `(?i)(?:Token|令牌)[^\n]{0,80}?([A-Za-z0-9_.-]{20,})`,
		ExtractKey: "TENCENT_DOCS_TOKEN"}
	bx := &BrowserExecutor{Collected: map[string]string{"__page_text__": page}}
	if err := bx.runOp(op, nil); err != nil {
		t.Fatal(err)
	}
	if bx.Collected["TENCENT_DOCS_TOKEN"] != "tok_abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("抽取结果错误: %q", bx.Collected["TENCENT_DOCS_TOKEN"])
	}

	bad := []struct {
		name string
		bx   *BrowserExecutor
		op   Op
	}{
		{"缺 readPage", &BrowserExecutor{Collected: map[string]string{}}, op},
		{"非法正则", bx, Op{Kind: "extract", Target: "([", ExtractKey: "X"}},
		{"无匹配", bx, Op{Kind: "extract", Target: `NEVER_MATCH_[0-9]{40}`, ExtractKey: "X"}},
	}
	for _, c := range bad {
		if err := c.bx.runOp(c.op, nil); err == nil {
			t.Fatalf("%s 应返回错误", c.name)
		}
	}
}
