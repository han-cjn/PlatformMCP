package onboard

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// WatchResult 是 hub.onboard.watch 的返回:纯只读、无副作用,供 Agent 高频轮询
// 感知"用户是否扫完码 / 是否已进入应用详情页",不需要用户在对话里回报。
type WatchResult struct {
	ChromeReady bool     `json:"chrome_ready"`
	Platform    string   `json:"platform"`
	LoginState  string   `json:"login_state"` // browser_missing / waiting_login / logged_in / unknown
	InAppDetail bool     `json:"in_app_detail"`
	DetailURL   string   `json:"detail_url,omitempty"`
	Pages       []string `json:"pages"`
	Next        string   `json:"next"`
}

// Watch 通过 CDP /json/list 观察调试 Chrome 的标签 URL,推断人工接入进度。
//
// 判定依据(飞书实测):扫码页是 accounts.feishu.cn/accounts/page/login?...&redirect_uri=
// ...open.feishu.cn%2Fapp,扫码成功后跳回开放平台;点进应用后 URL 变成
// https://open.feishu.cn/app/<appId>/... —— 三个阶段的信号都在 URL 里,无需读
// 页面 DOM、不需要登录态 cookie 权限,也不会触碰登录动作本身。
func Watch(platform string) (*WatchResult, error) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}
	res := &WatchResult{Platform: platform, Pages: []string{}}
	if !debugChromeReady() {
		res.LoginState = "browser_missing"
		res.Next = "调试端点暂时不可达;先重试 hub.onboard.watch 2~3 次,仍为 browser_missing 才重新调用 hub.onboard.start(不要一次失败就重开窗口)"
		return res, nil
	}
	res.ChromeReady = true
	pages, err := debugPages()
	if err != nil {
		res.LoginState = "unknown"
		res.Next = "CDP 标签读取失败: " + err.Error()
		return res, nil
	}
	if pages == nil {
		pages = []string{} // JSON 里保持 [] 而不是 null
	}
	res.Pages = pages
	res.LoginState, res.InAppDetail, res.DetailURL = classifyPages(p, pages)

	switch {
	case res.LoginState == "waiting_login":
		res.Next = "用户尚未完成扫码登录;继续轮询 hub.onboard.watch,检测到 logged_in 后自动往下走,不要催用户去终端执行命令"
	case res.LoginState == "logged_in" && res.InAppDetail:
		res.Next = "已登录且已进入凭据页;立即调用 hub.onboard.setup(confirmed=false) 拿该平台的动作清单,确认后自动完成抓凭据→写 .env→热重载→权限/发布(按 plan);只需抓凭据时才用 hub.onboard.capture"
	case res.LoginState == "logged_in":
		res.Next = "已登录;立即调用 hub.onboard.setup(confirmed=false) 拿到该平台 plan,确认后自动打开/创建应用并完成后续配置,不要让用户自己点进应用。" +
			"若下一次轮询变回 waiting_login,是登录页重定向刚完成,属正常,按新状态继续等待扫码"
	default:
		res.Next = "页面加载中或停在无关页面,尚未识别到登录页/开放平台页;继续轮询 hub.onboard.watch,状态明确后再继续"
	}
	return res, nil
}

// debugPages 列出调试实例中所有真实网页标签(排除 chrome:// 内部页)。
func debugPages() ([]string, error) {
	resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var items []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	var out []string
	for _, it := range items {
		if it.Type != "page" || it.URL == "" {
			continue
		}
		if strings.HasPrefix(it.URL, "chrome://") || strings.HasPrefix(it.URL, "about:") {
			continue
		}
		out = append(out, it.URL)
	}
	return out, nil
}

// classifyPages 按 URL 特征推断阶段。已登录优先于登录页残留标签
// (扫码成功后原登录页有时不自动关闭,不能据此误判为未登录)。
func classifyPages(p *Portal, pages []string) (state string, inDetail bool, detailURL string) {
	targetHost := hostOf(p.URL)
	hasTarget, hasLogin := false, false
	for _, u := range pages {
		// 调试 profile 会长期留下其他平台的标签。只按路径片段判定时,
		// 飞书的 /app/<id>/baseinfo 会被钉钉 CredPages 命中,导致接入钉钉
		// 却停在飞书页面。先做域隔离,只有本平台生态的标签才参与阶段判定。
		if !portalPageBelongs(p, u) {
			continue
		}
		login := portalIsLoginPage(p, u)
		if login {
			hasLogin = true
		}
		if hostOf(u) == targetHost && !login {
			hasTarget = true
		}
		// 各平台的详情页/凭据页特征统一在 portalDetailMatch 里声明,
		// 不再把飞书的 /app/<appId> 模式硬编码成四平台通用规则。
		if portalDetailMatch(p, u) {
			inDetail = true
			detailURL = u
		}
	}
	switch {
	case inDetail, hasTarget:
		state = "logged_in"
	case hasLogin:
		state = "waiting_login"
	default:
		state = "unknown"
	}
	return state, inDetail, detailURL
}

// isLoginPage 判定登录页:看 host(accounts.*)或路径(/login),不能看整个 URL
// 字符串——登录成功后跳回的 URL 常带 ?from=login 之类的查询参数,按整串
// 匹配会把已登录误判成未登录。
func isLoginPage(raw string) bool {
	if strings.HasPrefix(hostOf(raw), "accounts.") {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(u.Path), "login")
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}
