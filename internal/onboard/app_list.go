package onboard

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 应用列表页自动导航。
//
// 背景(2026-11-11 钉钉真机实测):开放平台入口页 ?hash=%23%2F#/ 只渲染
// 「应用开发」「创建快捷应用」等导航,不渲染应用列表;旧逻辑停在入口页时,
// listExistingApps 读不到任何 a[href] 应用链接,就把「读不到」当成「企业里
// 没有应用」,进而在 setup 的确认框里提出创建不可撤销的新应用。
//
// 实测链路(均为同一持久化 profile 内、登录态保留):
//   入口页 https://open-dev.dingtalk.com/?hash=%23%2F#/
//     → 点击顶部「应用开发」(li[role=menuitem],无 href 的 SPA 菜单)
//   列表页 https://open-dev.dingtalk.com/fe/app?hash=%23%2Fcorp%2Fapp#/corp/app
//     → 可直接导航该 URL,无需点击菜单;页面有 BUTTON「创建应用」、
//       <tr data-row-key><h5 title="应用名"> 的已有应用行(不是 a[href])
//   详情页 https://open-dev.dingtalk.com/fe/ai?...#/app/<uuid>/dev
//     → 点击应用行进入,侧栏含「凭证与基础信息 / 权限管理 / 版本管理与发布」
//
// 因此 AppListURL 走 CDP 直接打开/激活(不依赖 rod、不阻塞 MCP 调用),
// 只有「点击已有应用行进入详情」才用浏览器执行器(纯导航,不提交表单)。

// appListBase 返回列表页 URL 去掉 query/hash 后的稳定前缀。
func appListBase(p *Portal) string {
	if p == nil || p.AppListURL == "" {
		return ""
	}
	u, err := url.Parse(p.AppListURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// isAppListPage 判断标签是否停在该平台的应用列表页。
// 用 host+path 前缀而非整串相等:SPA 会在同一页面里改 hash/query(分组筛选),
// 整串相等会把「已在列表页」误判成「不在」,从而每次 start 都堆一个新标签。
func isAppListPage(p *Portal, raw string) bool {
	base := appListBase(p)
	return base != "" && strings.HasPrefix(raw, base) && portalPageBelongs(p, raw)
}

// findAppListTab 只读扫描 /json/list,命中即激活到前台。
func findAppListTab(p *Portal) (string, bool) {
	resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var items []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		ID   string `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&items) != nil {
		return "", false
	}
	for _, it := range items {
		if it.Type == "page" && isAppListPage(p, it.URL) {
			activateDebugTab(it.ID)
			return it.URL, true
		}
	}
	return "", false
}

// appListPendingTab 记住「刚创建、但还没注册进 /json/list」的标签 ID。
// Chrome 的注册窗口可能长到超过轮询时长,只靠轮询无法阻止连续调用各开一个标签;
// 用 ID 兜底:只要该标签还在,就直接激活它,不再新建。
var appListPendingTab = struct {
	sync.Mutex
	byPlatform map[string]string
}{byPlatform: map[string]string{}}

// debugTabAlive 判断某个标签 ID 是否仍在调试实例里(不看 URL,加载中也算)。
func debugTabAlive(id string) bool {
	if id == "" {
		return false
	}
	resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var items []struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&items) != nil {
		return false
	}
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// ensureAppListTab 保证调试 Chrome 里有一个已激活的应用列表页标签。
// AppListURL 为空的平台不做任何导航。
//
// Chrome 处理 /json/new 之后,新标签注册进 /json/list 有窗口期;只查一次时,
// 连续调用 start → start → setup 会各开一个标签(2026-11-11 实测堆了 3 个)。
// 三层防护:短轮询找已注册标签 → 记住的 pending ID 仍存活就复用 → 才新建。
func ensureAppListTab(p *Portal) (string, bool) {
	if p == nil || p.AppListURL == "" {
		return "", false
	}
	appListPendingTab.Lock()
	defer appListPendingTab.Unlock()

	for i := 0; i < 4; i++ {
		if raw, ok := findAppListTab(p); ok {
			delete(appListPendingTab.byPlatform, p.Name) // 标签已注册,无需再兜底
			return raw, true
		}
		if i < 3 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if id := appListPendingTab.byPlatform[p.Name]; id != "" && debugTabAlive(id) {
		activateDebugTab(id)
		return p.AppListURL, true
	}
	id, ok := openDebugTabID(p.AppListURL)
	if !ok {
		return "", false
	}
	appListPendingTab.byPlatform[p.Name] = id
	for i := 0; i < 5; i++ {
		if raw, ok := findAppListTab(p); ok {
			delete(appListPendingTab.byPlatform, p.Name)
			return raw, true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return p.AppListURL, true
}

// focusAppListPage 把浏览器执行器绑定到应用列表页标签(多个平台标签并存时)。
func (bx *BrowserExecutor) focusAppListPage(p *Portal) bool {
	if bx == nil || p == nil || p.AppListURL == "" {
		return false
	}
	found := false
	_ = bx.safe(func() {
		pages := bx.browser.MustPages()
		urls := make([]string, len(pages))
		for i, pg := range pages {
			if info, e := pg.Info(); e == nil && info != nil {
				urls[i] = info.URL
			}
		}
		for i, raw := range urls {
			if isAppListPage(p, raw) {
				bx.page = pages[i]
				found = true
				_, _ = pages[i].Activate()
				return
			}
		}
	})
	return found
}

// clickAppRow 点击列表页第一个已有应用,进入其详情页。
// 纯导航点击,不填表、不提交、不创建;失败返回 false,由调用方降级。
func clickAppRow(p *Portal) (string, bool) {
	if p == nil || p.AppListRowSelector == "" {
		return "", false
	}
	bx, err := NewBrowserExecutorForPlatform("", p)
	if err != nil {
		return "", false
	}
	defer bx.Close()
	if !bx.focusAppListPage(p) {
		return "", false
	}
	if err := bx.RunAction(&BrowserAction{
		Script: "app-list@2026-11-click-row",
		Ops:    []Op{{Kind: "click", Target: p.AppListRowSelector}},
	}, nil); err != nil {
		return "", false
	}
	// SPA 可能同标签跳转,也可能新开标签:两种都覆盖。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if pages, e := debugPages(); e == nil {
			for _, raw := range pages {
				if portalDetailMatch(p, raw) {
					return raw, true
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", false
}
