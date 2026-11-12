package onboard

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// 2026-11-11 钉钉真机实测回归:入口页不渲染应用列表,Agent 必须自己进入
// /fe/app?...#/corp/app,并据此区分「已有应用」与「真的没有应用」。
// 下列用例固化三段 URL 的判定、幂等导航与菜单文本校准。

func TestIsAppListPage(t *testing.T) {
	ding := Portals["dingtalk"]
	listVariant := "https://open-dev.dingtalk.com/fe/app?hash=%23%2Fcorp%2Fapp%23group%3D1#/corp/app?group=1"
	cases := []struct {
		name string
		p    *Portal
		url  string
		want bool
	}{
		{"实测列表页", ding, dingAppList, true},
		{"SPA 改 hash/query 后仍在列表页", ding, listVariant, true},
		{"入口页不是列表页", ding, "https://open-dev.dingtalk.com/?hash=%23%2F#/", false},
		{"详情页不是列表页", ding, dingDetail, false},
		{"登录页不是列表页", ding, "https://login.dingtalk.com/oauth2/challenge.htm", false},
		{"飞书标签不是钉钉列表页", ding, staleFeishuDetail, false},
		{"钉钉列表页不是飞书列表页", Portals["feishu"], dingAppList, false},
		{"未配置列表页的平台一律 false", Portals["wecom"], dingAppList, false},
		{"空 URL", ding, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAppListPage(c.p, c.url); got != c.want {
				t.Fatalf("isAppListPage(%q)=%v,期望 %v", c.url, got, c.want)
			}
		})
	}
}

func TestDingtalkRealURLs(t *testing.T) {
	p := Portals["dingtalk"]
	if portalDetailMatch(p, dingAppList) {
		t.Fatal("应用列表页不能被当成凭据详情页")
	}
	if !portalDetailMatch(p, dingDetail) {
		t.Fatal("实测详情页 URL 应被识别为 in_app_detail")
	}
	if id := portalAppID(p, dingDetail); id != "1f2e47b9-a105-48f6-9cdc-45b32563a88e" {
		t.Fatalf("详情页应用 ID 抽取错误: %q", id)
	}
	state, inDetail, url := classifyPages(p, []string{dingAppList})
	if state != "logged_in" || inDetail || url != "" {
		t.Fatalf("停在列表页应为 logged_in 且未进详情: %q/%v/%q", state, inDetail, url)
	}
	state, inDetail, url = classifyPages(p, []string{dingAppList, dingDetail})
	if state != "logged_in" || !inDetail || url != dingDetail {
		t.Fatalf("列表页+详情页应识别详情: %q/%v/%q", state, inDetail, url)
	}
}

// resetAppListPending 清掉跨用例残留的 pending 标签 ID(全局缓存只在单个
// hub 进程内有用,单测之间必须隔离)。
func resetAppListPending(t *testing.T) {
	t.Helper()
	appListPendingTab.Lock()
	delete(appListPendingTab.byPlatform, "dingtalk")
	appListPendingTab.Unlock()
	t.Cleanup(func() {
		appListPendingTab.Lock()
		delete(appListPendingTab.byPlatform, "dingtalk")
		appListPendingTab.Unlock()
	})
}

func TestEnsureAppListTabIdempotent(t *testing.T) {
	resetAppListPending(t)
	p := Portals["dingtalk"]
	t.Run("已有列表页标签只激活不新建", func(t *testing.T) {
		var paths []string
		mockCDP(t, `[{"type":"page","url":"`+dingAppList+`","id":"LIST-1"}]`, &paths)
		got, ok := ensureAppListTab(p)
		if !ok || got != dingAppList {
			t.Fatalf("ensureAppListTab=(%q,%v)", got, ok)
		}
		if !hasPath(paths, "/json/activate/LIST-1") {
			t.Fatalf("已有列表页未被激活: %v", paths)
		}
		for _, path := range paths {
			if strings.HasPrefix(path, "/json/new") {
				t.Fatalf("已有列表页不应重复开标签: %v", paths)
			}
		}
	})
	t.Run("只有飞书残留标签时新开列表页", func(t *testing.T) {
		var paths []string
		mockCDP(t, `[{"type":"page","url":"`+staleFeishuDetail+`","id":"FS-1"}]`, &paths)
		if _, ok := ensureAppListTab(p); !ok {
			t.Fatal("应成功打开应用列表页")
		}
		if !hasPath(paths, "/json/activate/NEW-2") {
			t.Fatalf("新开的列表页未被激活: %v", paths)
		}
		opened := false
		for _, path := range paths {
			if strings.HasPrefix(path, "/json/new") {
				opened = true
			}
		}
		if !opened {
			t.Fatalf("应新开应用列表页标签: %v", paths)
		}
	})
	t.Run("未配置列表页的平台不做任何导航", func(t *testing.T) {
		var paths []string
		mockCDP(t, `[]`, &paths)
		if _, ok := ensureAppListTab(Portals["feishu"]); ok {
			t.Fatal("未配置 AppListURL 不应返回成功")
		}
		if len(paths) != 0 {
			t.Fatalf("不应发出任何 CDP 请求: %v", paths)
		}
	})
}

// TestEnsureAppListTabWaitsForRegistration 固化实测踩坑:
// /json/new 返回后新标签要过几百毫秒才出现在 /json/list,连续调用
// start → start → setup 曾因此堆了 3 个重复列表页标签。
func TestEnsureAppListTabWaitsForRegistration(t *testing.T) {
	resetAppListPending(t)
	old := cdpHTTPClient.Transport
	t.Cleanup(func() { cdpHTTPClient.Transport = old })
	calls := 0
	var paths []string
	cdpHTTPClient.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		body := "Target activated"
		switch {
		case r.URL.Path == "/json/list":
			calls++
			if calls >= 3 {
				body = `[{"type":"page","url":"` + dingAppList + `","id":"LIST-LATE"}]`
			} else {
				body = `[]`
			}
		case strings.HasPrefix(r.URL.Path, "/json/new"):
			body = `{"id":"NEW-2","type":"page","url":"https://open-dev.dingtalk.com/"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	})
	got, ok := ensureAppListTab(Portals["dingtalk"])
	if !ok || got != dingAppList {
		t.Fatalf("轮询命中延迟注册的标签失败: (%q,%v)", got, ok)
	}
	if !hasPath(paths, "/json/activate/LIST-LATE") {
		t.Fatalf("延迟出现的列表页未被激活: %v", paths)
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "/json/new") {
			t.Fatalf("标签只是注册慢,不应重复新建: %v", paths)
		}
	}
}

// TestEnsureAppListTabReusesPendingTab 第二层防护:标签还没注册成列表页 URL
// 时,靠缓存的 ID 复用,连续 start 不再各开一个标签。
func TestEnsureAppListTabReusesPendingTab(t *testing.T) {
	resetAppListPending(t)
	var paths []string
	mockCDP(t, `[{"type":"page","url":"https://open-dev.dingtalk.com/?hash=%23%2F#/","id":"PENDING-1"}]`, &paths)
	appListPendingTab.Lock()
	appListPendingTab.byPlatform["dingtalk"] = "PENDING-1"
	appListPendingTab.Unlock()
	if _, ok := ensureAppListTab(Portals["dingtalk"]); !ok {
		t.Fatal("pending 标签仍存活时应直接复用")
	}
	if !hasPath(paths, "/json/activate/PENDING-1") {
		t.Fatalf("pending 标签未被激活: %v", paths)
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "/json/new") {
			t.Fatalf("pending 标签仍存活时不应新建: %v", paths)
		}
	}
}

func TestPickPlatformPagePrefersAppList(t *testing.T) {
	p := Portals["dingtalk"]
	entry := "https://open-dev.dingtalk.com/?hash=%23%2F#/"
	cases := []struct {
		name string
		urls []string
		want int
	}{
		{"列表页优先于入口页", []string{entry, dingAppList}, 1},
		{"列表页排在入口页前也仍选列表页", []string{dingAppList, entry}, 0},
		{"详情页优先于列表页", []string{dingAppList, dingDetail}, 1},
		{"跳过飞书残留标签", []string{staleFeishuDetail, entry, dingAppList}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickPlatformPage(p, c.urls); got != c.want {
				t.Fatalf("pickPlatformPage=%d,期望 %d (urls=%v)", got, c.want, c.urls)
			}
		})
	}
}

func TestDingtalkCreateOpsMatchRealButton(t *testing.T) {
	p := Portals["dingtalk"]
	act := createAppAction(p)
	if act == nil {
		t.Fatal("钉钉应声明创建应用动作")
	}
	clicks, first := 0, true
	for _, op := range act.Ops {
		if op.Kind != "click" || op.Match == "" {
			continue
		}
		clicks++
		re := regexp.MustCompile(op.Match)
		if first {
			// 第一个 click 必须是列表页实测的「创建应用」按钮,不能误点
			// 入口页的「创建快捷应用」或宣传卡片的「立即创建」。
			if !re.MatchString("创建应用") {
				t.Fatalf("首个 click 的 Match /%s/ 未命中实测按钮「创建应用」", op.Match)
			}
			if re.MatchString("创建快捷应用") || re.MatchString("立即创建") {
				t.Fatalf("首个 click 的 Match /%s/ 会误点入口页/宣传卡片按钮", op.Match)
			}
			first = false
		}
	}
	if clicks == 0 {
		t.Fatal("创建动作里没有 click 步骤")
	}
}

func TestSupportedPlatformsAppListVerified(t *testing.T) {
	// 2026-11-12 企微机器人列表页(/wework_admin/frame#/aiHelper/list?tab=manage)
	// 真机实测可直达、.d2c_bot_name 行可点进详情,与钉钉一样有实测列表页。
	// 因此有列表页的平台不再只有钉钉;飞书/腾讯文档仍未实测,保持无列表页。
	appListURLs := map[string]string{
		"dingtalk": dingAppList,
		"wecom":    "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/list?from=manage_tools&tab=manage",
	}
	for _, info := range SupportedPlatforms(nil) {
		wantURL, hasList := appListURLs[info.ID]
		if info.AppListVerified != hasList {
			t.Errorf("%s app_list_verified=%v,期望 %v", info.ID, info.AppListVerified, hasList)
		}
		if hasList && info.AppListURL != wantURL {
			t.Errorf("%s app_list_url=%q,期望 %q", info.ID, info.AppListURL, wantURL)
		}
		if !hasList && info.AppListURL != "" {
			t.Errorf("%s 不应暴露 app_list_url", info.ID)
		}
	}
}
