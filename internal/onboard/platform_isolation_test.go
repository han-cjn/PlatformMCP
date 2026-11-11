package onboard

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 2026-11-11 实测回归:调试 Chrome 的持久化 profile 里留着上一次飞书接入的
// 详情页 https://open.feishu.cn/app/cli_xxx/baseinfo,它同时含 "/app/" 与
// "/baseinfo",正好命中钉钉 CredPages。结果 Watch("dingtalk") 误判已进详情页,
// start 提前 ready_to_capture 且不开钉钉页面,setup 的 app_url 指向飞书。
// 以下用例固化「所有 URL 判定必须先做域隔离」这条边界。

const (
	staleFeishuDetail = "https://open.feishu.cn/app/cli_aa6ee0lec8b85bf6/baseinfo"
	dingDetail        = "https://open-dev.dingtalk.com/fe/app?appId=dingabc123"
	wecomDetail       = "https://work.weixin.qq.com/ai/robot?botid=bot123456"
	qqAuthPage        = "https://docs.qq.com/open/auth/mcp.html"
)

func TestPortalDetailMatchRejectsCrossPlatformTabs(t *testing.T) {
	rejects := []struct{ platform, url string }{
		{"dingtalk", staleFeishuDetail},
		{"wecom", staleFeishuDetail},
		{"tencent-docs", staleFeishuDetail},
		{"feishu", dingDetail},
		{"feishu", wecomDetail},
		{"feishu", qqAuthPage},
		{"dingtalk", wecomDetail},
		{"wecom", dingDetail},
		{"wecom", qqAuthPage},
		{"tencent-docs", wecomDetail},
		{"tencent-docs", dingDetail},
	}
	for _, c := range rejects {
		if portalDetailMatch(Portals[c.platform], c.url) {
			t.Errorf("%s 误把 %q 当成自己的详情页", c.platform, c.url)
		}
	}
	accepts := []struct{ platform, url string }{
		{"feishu", staleFeishuDetail},
		{"dingtalk", dingDetail},
		{"wecom", wecomDetail},
		{"tencent-docs", qqAuthPage},
	}
	for _, c := range accepts {
		if !portalDetailMatch(Portals[c.platform], c.url) {
			t.Errorf("%s 未识别自己的详情页 %q", c.platform, c.url)
		}
	}
}

func TestClassifyPagesIgnoresStalePlatformTabs(t *testing.T) {
	cases := []struct {
		name       string
		platform   string
		pages      []string
		wantState  string
		wantDetail bool
		wantURL    string
	}{
		{
			name:      "只有残留飞书标签时钉钉应视为未开始",
			platform:  "dingtalk",
			pages:     []string{staleFeishuDetail},
			wantState: "unknown",
		},
		{
			name:      "残留飞书标签加钉钉登录页只算等待登录",
			platform:  "dingtalk",
			pages:     []string{staleFeishuDetail, "https://login.dingtalk.com/"},
			wantState: "waiting_login",
		},
		{
			name:       "残留飞书标签加钉钉详情页只认钉钉",
			platform:   "dingtalk",
			pages:      []string{staleFeishuDetail, dingDetail},
			wantState:  "logged_in",
			wantDetail: true,
			wantURL:    dingDetail,
		},
		{
			name:       "残留钉钉标签不影响飞书判定",
			platform:   "feishu",
			pages:      []string{dingDetail, staleFeishuDetail},
			wantState:  "logged_in",
			wantDetail: true,
			wantURL:    staleFeishuDetail,
		},
		{
			name:      "只有残留钉钉标签时飞书应视为未开始",
			platform:  "feishu",
			pages:     []string{dingDetail},
			wantState: "unknown",
		},
		{
			name:      "腾讯文档授权页不应被企微认成自己的页面",
			platform:  "wecom",
			pages:     []string{qqAuthPage},
			wantState: "unknown",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, detail, url := classifyPages(Portals[c.platform], c.pages)
			if state != c.wantState || detail != c.wantDetail || url != c.wantURL {
				t.Fatalf("state=%q detail=%v url=%q,期望 %q/%v/%q",
					state, detail, url, c.wantState, c.wantDetail, c.wantURL)
			}
		})
	}
}

func TestPortalPageBelongsAllowlist(t *testing.T) {
	cases := []struct {
		platform, url string
		want          bool
	}{
		{"dingtalk", "https://open-dev.dingtalk.com/", true},
		{"dingtalk", "https://login.dingtalk.com/", true},
		{"dingtalk", "https://oapi.dingtalk.com/", true},
		{"dingtalk", staleFeishuDetail, false},
		{"feishu", "https://accounts.feishu.cn/accounts/page/login", true},
		{"feishu", "https://open-dev.dingtalk.com/", false},
		{"wecom", "https://login.work.weixin.qq.com/wwlogin", true},
		{"wecom", qqAuthPage, false},
		{"tencent-docs", qqAuthPage, true},
		{"tencent-docs", "https://ssl.ptlogin2.qq.com/ptlogin", true},
		{"tencent-docs", wecomDetail, false},
	}
	for _, c := range cases {
		if got := portalPageBelongs(Portals[c.platform], c.url); got != c.want {
			t.Errorf("portalPageBelongs(%s, %q)=%v,期望 %v", c.platform, c.url, got, c.want)
		}
	}
}

// TestReadyToCaptureRequiresSamePlatform 守住 start 的提前返回闸门:
// 即便上层误传了别平台的详情页,也不能跳过打开目标平台页面。
func TestReadyToCaptureRequiresSamePlatform(t *testing.T) {
	p := Portals["dingtalk"]
	w := &WatchResult{ChromeReady: true, InAppDetail: true, DetailURL: staleFeishuDetail}
	if readyToCapture(p, w) {
		t.Fatal("飞书详情页不应让钉钉进入 ready_to_capture")
	}
	w.DetailURL = dingDetail
	if !readyToCapture(p, w) {
		t.Fatal("钉钉自己的详情页应允许 ready_to_capture")
	}
	w.ChromeReady = false
	if readyToCapture(p, w) {
		t.Fatal("Chrome 未就绪不应提前返回")
	}
	w.ChromeReady, w.InAppDetail = true, false
	if readyToCapture(p, w) {
		t.Fatal("未进详情页不应提前返回")
	}
}

// ---- 标签激活:start 打开目标页面后必须把它切到前台,否则用户看到的仍是上次飞书页 ----

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockCDP(t *testing.T, listBody string, paths *[]string) {
	t.Helper()
	old := cdpHTTPClient.Transport
	t.Cleanup(func() { cdpHTTPClient.Transport = old })
	cdpHTTPClient.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		*paths = append(*paths, r.URL.Path)
		body := "Target activated"
		switch {
		case r.URL.Path == "/json/list":
			body = listBody
		case strings.HasPrefix(r.URL.Path, "/json/new"):
			body = `{"id":"NEW-2","type":"page","url":"https://open-dev.dingtalk.com/"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	})
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func TestOpenDebugTabActivatesExistingTab(t *testing.T) {
	var paths []string
	mockCDP(t, `[{"type":"page","url":"https://open-dev.dingtalk.com/","id":"TARGET-1"}]`, &paths)
	if !openDebugTab("https://open-dev.dingtalk.com/") {
		t.Fatal("幂等命中已有标签应返回 true")
	}
	if !hasPath(paths, "/json/activate/TARGET-1") {
		t.Fatalf("已有标签未被激活,请求路径: %v", paths)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "/json/new") {
			t.Fatalf("幂等命中不应新开标签,请求路径: %v", paths)
		}
	}
}

func TestOpenDebugTabActivatesNewTab(t *testing.T) {
	var paths []string
	mockCDP(t, `[]`, &paths)
	if !openDebugTab("https://open-dev.dingtalk.com/") {
		t.Fatal("新开标签应返回 true")
	}
	if !hasPath(paths, "/json/activate/NEW-2") {
		t.Fatalf("新开标签未被激活,请求路径: %v", paths)
	}
}

// withUnreachableCDP 让「无调试 Chrome 时应降级」的用例不再受本机环境干扰。
// 之前这些用例硬编码假设 9222 不可达,一旦用户真的开着调试 Chrome 就会误报;
// 显式注入 CDP 不可达,保证降级路径在任何机器上都可复现。
func withUnreachableCDP(t *testing.T) {
	t.Helper()
	old := cdpHTTPClient.Transport
	t.Cleanup(func() { cdpHTTPClient.Transport = old })
	cdpHTTPClient.Transport = rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("CDP 不可达(测试注入)")
	})
}

// TestOpenPlatformEntryTabHandlesRedirectedLogin 入口 302 到登录域后,
// 重试 start 应激活已有标签而不是再开一个(实测钉钉会跳 login.dingtalk.com)。
func TestOpenPlatformEntryTabHandlesRedirectedLogin(t *testing.T) {
	var paths []string
	mockCDP(t, `[{"type":"page","url":"https://login.dingtalk.com/oauth2/challenge.htm?redirect_uri=x","id":"LOGIN-1"}]`, &paths)
	if !openPlatformEntryTab(Portals["dingtalk"], "https://open-dev.dingtalk.com/") {
		t.Fatal("已有同平台标签时应返回 true")
	}
	if !hasPath(paths, "/json/activate/LOGIN-1") {
		t.Fatalf("重定向后的登录页未被激活,请求路径: %v", paths)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "/json/new") {
			t.Fatalf("已有同平台标签不应重复新开,请求路径: %v", paths)
		}
	}
}

// TestOpenPlatformEntryTabIgnoresStaleFeishuTab 用户报障的原始场景:
// 只留着飞书详情页时接钉钉,不能把飞书标签当作已打开,必须真的开钉钉页。
func TestOpenPlatformEntryTabIgnoresStaleFeishuTab(t *testing.T) {
	var paths []string
	mockCDP(t, `[{"type":"page","url":"`+staleFeishuDetail+`","id":"FS-1"}]`, &paths)
	if !openPlatformEntryTab(Portals["dingtalk"], "https://open-dev.dingtalk.com/") {
		t.Fatal("应成功打开钉钉入口页")
	}
	if !hasPath(paths, "/json/activate/NEW-2") {
		t.Fatalf("新开的钉钉标签未被激活,请求路径: %v", paths)
	}
	if hasPath(paths, "/json/activate/FS-1") {
		t.Fatalf("不应激活残留的飞书标签,请求路径: %v", paths)
	}
	newOpened := false
	for _, p := range paths {
		if strings.HasPrefix(p, "/json/new") {
			newOpened = true
		}
	}
	if !newOpened {
		t.Fatalf("应新开钉钉入口页,请求路径: %v", paths)
	}
}

// TestPickPlatformPage 固化浏览器执行器的选标签优先级。
// 用户报障场景:飞书标签排在前面,执行器必须跳过它们绑定到钉钉标签,
// 否则创建表单 / inspect / readPage 全跑在飞书页面上。
func TestPickPlatformPage(t *testing.T) {
	dingEntry := "https://open-dev.dingtalk.com/?hash=%23%2F#/"
	dingLogin := "https://login.dingtalk.com/oauth2/challenge.htm?redirect_uri=x"
	feishuList := "https://open.feishu.cn/app"
	cases := []struct {
		name string
		p    *Portal
		urls []string
		want int
	}{
		{"飞书标签在前也要选钉钉", Portals["dingtalk"], []string{staleFeishuDetail, feishuList, dingEntry}, 2},
		{"钉钉标签在前选钉钉", Portals["dingtalk"], []string{dingEntry, staleFeishuDetail, feishuList}, 0},
		{"详情页优先于入口页", Portals["dingtalk"], []string{dingEntry, dingDetail}, 1},
		{"详情页优先于登录页", Portals["dingtalk"], []string{dingLogin, dingDetail}, 1},
		{"域内非登录页优先于登录页", Portals["dingtalk"], []string{dingLogin, dingEntry}, 1},
		{"只有登录页也可作为兜底", Portals["dingtalk"], []string{staleFeishuDetail, dingLogin}, 1},
		{"只有飞书标签则无匹配", Portals["dingtalk"], []string{staleFeishuDetail, feishuList}, -1},
		{"飞书视角忽略钉钉标签", Portals["feishu"], []string{dingDetail, staleFeishuDetail}, 1},
		{"空列表", Portals["dingtalk"], nil, -1},
		{"nil portal", nil, []string{dingEntry}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickPlatformPage(c.p, c.urls); got != c.want {
				t.Fatalf("pickPlatformPage=%d,期望 %d (urls=%v)", got, c.want, c.urls)
			}
		})
	}
}
