package onboard

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 2026-11-11 实测回归:调试 Chrome 的持久化 profile 里留着上一次飞书接入的
// 详情页 https://open.feishu.cn/app/cli_xxx/baseinfo,它同时含 "/app/" 与
// "/baseinfo",正好命中钉钉 CredPages。结果 Watch("dingtalk") 误判已进详情页,
// start 提前 ready_to_capture 且不开钉钉页面,setup 的 app_url 指向飞书。
// 以下用例固化「所有 URL 判定必须先做域隔离」这条边界。

const (
	staleFeishuDetail = "https://open.feishu.cn/app/cli_aa6ee0lec8b85bf6/baseinfo"
	// 2026-11-11 真机实测:钉钉详情页是 /fe/ai?...#/app/<uuid>/dev,
	// /fe/app 是应用列表页(不是详情页),旧常量把两者混为一谈。
	dingDetail  = "https://open-dev.dingtalk.com/fe/ai?hash=%23%2Fapp%2F1f2e47b9-a105-48f6-9cdc-45b32563a88e%2Fdev#/app/1f2e47b9-a105-48f6-9cdc-45b32563a88e/dev"
	dingAppList = "https://open-dev.dingtalk.com/fe/app?hash=%23%2Fcorp%2Fapp#/corp/app"
	// 2026-11-12 真机实测:企微机器人详情页是 /wework_admin/frame#/aiHelper/detail?aibotid=<数字>,
	// 旧常量 /ai/robot?botid= 是推测值。
	wecomDetail = "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/detail?aibotid=33776999461281670"
	qqAuthPage  = "https://docs.qq.com/open/auth/mcp.html"
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

// ---- 2026-11-12 报障回归:start 连开 5 个官网标签 + 后台页面样式错乱 ----

// TestPlatformEntryURLPrefersAppList 企微 Portal.URL 是官网营销页
// (work.weixin.qq.com/#/app/workbenchEditor/index),start 必须落在实测可直达
// 的管理后台列表页;没有 AppListURL 的平台仍保留原入口。
func TestPlatformEntryURLPrefersAppList(t *testing.T) {
	if got := platformEntryURL(Portals["wecom"]); got != Portals["wecom"].AppListURL {
		t.Fatalf("企微 start 落点=%q,期望应用列表页 %q", got, Portals["wecom"].AppListURL)
	}
	if got := platformEntryURL(Portals["dingtalk"]); got != Portals["dingtalk"].AppListURL {
		t.Fatalf("钉钉 start 落点=%q,期望应用列表页 %q", got, Portals["dingtalk"].AppListURL)
	}
	if got := platformEntryURL(Portals["feishu"]); got != Portals["feishu"].URL {
		t.Fatalf("飞书无 AppListURL 应保留官网入口,得到 %q", got)
	}
}

// TestOpenPlatformEntryTabReusesPendingTab 新标签刚创建、URL 仍是 about:blank
// 时,连续两次 start 只能开一个标签(注册窗口期靠 pending ID 兜底)。
func TestOpenPlatformEntryTabReusesPendingTab(t *testing.T) {
	entryPendingTab.Lock()
	delete(entryPendingTab.byPlatform, "wecom")
	entryPendingTab.Unlock()
	t.Cleanup(func() {
		entryPendingTab.Lock()
		delete(entryPendingTab.byPlatform, "wecom")
		entryPendingTab.Unlock()
	})

	var paths []string
	mockCDP(t, `[{"type":"page","url":"about:blank","id":"NEW-2"}]`, &paths)
	p := Portals["wecom"]
	if !openPlatformEntryTab(p, p.AppListURL) {
		t.Fatal("首次应成功打开入口页")
	}
	paths = nil
	if !openPlatformEntryTab(p, p.AppListURL) {
		t.Fatal("pending 标签仍存活时应复用成功")
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "/json/new") {
			t.Fatalf("pending 标签仍存活时不应再新建,请求路径: %v", paths)
		}
	}
	if !hasPath(paths, "/json/activate/NEW-2") {
		t.Fatalf("pending 标签未被激活,请求路径: %v", paths)
	}
}

// TestOpenURLWithSystemThrottles 调试 Chrome 起不来时,轮询重试 start 不能
// 每轮都在日常浏览器里再开一个标签。
func TestOpenURLWithSystemThrottles(t *testing.T) {
	oldRun := systemOpenRun
	systemOpenGuard.Lock()
	oldURL, oldAt := systemOpenGuard.lastURL, systemOpenGuard.lastAt
	systemOpenGuard.lastURL, systemOpenGuard.lastAt = "", time.Time{}
	systemOpenGuard.Unlock()
	t.Cleanup(func() {
		systemOpenRun = oldRun
		systemOpenGuard.Lock()
		systemOpenGuard.lastURL, systemOpenGuard.lastAt = oldURL, oldAt
		systemOpenGuard.Unlock()
	})

	calls := 0
	systemOpenRun = func(string) error { calls++; return nil }
	const u = "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/list"
	if !openURLWithSystem(u) || !openURLWithSystem(u) {
		t.Fatal("同一页面重复兜底应视为已呈现")
	}
	if calls != 1 {
		t.Fatalf("节流失效:同一 URL 拉起系统浏览器 %d 次,期望 1 次", calls)
	}
	if !openURLWithSystem(u + "&tab=manage") {
		t.Fatal("不同 URL 不应被节流误伤")
	}
	if calls != 2 {
		t.Fatalf("不同 URL 应各自打开一次,实际 %d 次", calls)
	}
}

// TestChromeLaunchArgsHaveStableWindowAndGPUFlags 锁定启动参数:缺 GPU flag
// 会 exit 133,缺 window-size 会让固定宽度后台横向溢出。
func TestChromeLaunchArgsHaveStableWindowAndGPUFlags(t *testing.T) {
	args := strings.Join(chromeLaunchArgs("/tmp/hub-test-profile"), " ")
	for _, want := range []string{
		"--remote-debugging-port=9222",
		"--user-data-dir=/tmp/hub-test-profile",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--window-size=1440,900",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("调试 Chrome 启动参数缺少 %q: %s", want, args)
		}
	}
}

// TestOpenPlatformEntryTabPrefersAdminListOverMarketingPage 用户截图场景:
// profile 里残留多个官网营销页标签,真正要操作的是管理后台列表页。
func TestOpenPlatformEntryTabPrefersAdminListOverMarketingPage(t *testing.T) {
	entryPendingTab.Lock()
	delete(entryPendingTab.byPlatform, "wecom")
	entryPendingTab.Unlock()
	t.Cleanup(func() {
		entryPendingTab.Lock()
		delete(entryPendingTab.byPlatform, "wecom")
		entryPendingTab.Unlock()
	})

	var paths []string
	mockCDP(t, `[
		{"type":"page","url":"https://work.weixin.qq.com/#/app/workbenchEditor/index","id":"MKT-1"},
		{"type":"page","url":"https://work.weixin.qq.com/wework_admin/frame#/aiHelper/list?from=manage_tools&tab=manage","id":"LIST-1"}
	]`, &paths)
	if !openPlatformEntryTab(Portals["wecom"], Portals["wecom"].AppListURL) {
		t.Fatal("应命中已有的管理后台标签")
	}
	if !hasPath(paths, "/json/activate/LIST-1") {
		t.Fatalf("未激活管理后台列表页,请求路径: %v", paths)
	}
	if hasPath(paths, "/json/activate/MKT-1") {
		t.Fatalf("不应激活官网营销页,请求路径: %v", paths)
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "/json/new") {
			t.Fatalf("已有后台标签不应新开,请求路径: %v", paths)
		}
	}
}

// TestActivatePlatformTabPrefersDetailOverMarketing 真机发现的残留缺陷:
// start 走 ready_to_capture 提前返回时,前台仍是残留的官网营销页,
// 详情页藏在后面(vis=hidden),用户以为工具没干活。
func TestActivatePlatformTabPrefersDetailOverMarketing(t *testing.T) {
	var paths []string
	mockCDP(t, `[
		{"type":"page","url":"https://work.weixin.qq.com/#/app/workbenchEditor/index","id":"MKT-1"},
		{"type":"page","url":"`+wecomDetail+`","id":"DETAIL-1"}
	]`, &paths)
	if !activatePlatformTab(Portals["wecom"]) {
		t.Fatal("应命中已有的企微详情页标签")
	}
	if !hasPath(paths, "/json/activate/DETAIL-1") {
		t.Fatalf("详情页未被激活,请求路径: %v", paths)
	}
	if hasPath(paths, "/json/activate/MKT-1") {
		t.Fatalf("不应激活官网营销页,请求路径: %v", paths)
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "/json/new") {
			t.Fatalf("只读激活不应新开标签,请求路径: %v", paths)
		}
	}
}
