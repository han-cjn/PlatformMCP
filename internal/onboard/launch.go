package onboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// hub.onboard.start 的后端实现:为非交互入口准备接入环境。
//
// 背景:Agent 在 stdio 宿主(WorkBuddy)里没有可交互终端,向导 Run() 中的
// prompt()/collectCreds() 无法执行;而"起调试 Chrome + 打开开放平台页面"
// 这两个动作完全不需要交互,可以安全地由 MCP 工具代做,把人工登录与
// 后续凭据抄写留在浏览器/终端里完成。
//
// 合规边界同 BrowserExecutor:只打开页面,绝不代做登录(扫码/验证码永远人工)。

const (
	chromeDebugPort = 9222
	chromeDebugURL  = "http://127.0.0.1:9222"
	chromeWaitMax   = 12 * time.Second
)

// cdpHTTPClient 直连本地调试端口:显式禁用代理。宿主注入 HTTP_PROXY 时,
// 127.0.0.1 的请求可能被转发到上游拿到 502,进而误判"Chrome 未启动"。
var cdpHTTPClient = &http.Client{
	Timeout: 2 * time.Second, // 本地回环,2s 足够;短超时让重试快速收敛
	Transport: &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true, // Chrome 调试端点会提前关连接,复用会拿到 EOF 误判"未启动"
	},
}

// StartResult 描述已完成的自动化动作与后续人工步骤,供 Agent 组织回复。
// 刻意不含任何凭据值,也不含需要等待人工输入的字段。
type StartResult struct {
	Platform    string   `json:"platform"`
	Phase       string   `json:"phase"` // ready_to_capture / logged_in / waiting_login / browser_started
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Chrome      string   `json:"chrome"` // already-running / started / unavailable
	ChromePort  int      `json:"chrome_port"`
	PageOpened  bool     `json:"page_opened"`
	ManualSteps []string `json:"manual_steps"`
	Next        string   `json:"next"`
	Warning     string   `json:"warning,omitempty"`
}

// StartPlatform 确保带持久化 profile 的 Chrome 调试实例在运行,并在其中打开
// 指定平台的开放平台页面。全程不读 stdin、不等待人工输入、不阻塞 MCP 调用。
func StartPlatform(platform string) (*StartResult, error) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}

	res := &StartResult{
		Platform:    platform,
		Phase:       "browser_started",
		Title:       p.Title,
		URL:         p.URL,
		ChromePort:  chromeDebugPort,
		ManualSteps: manualSteps(p),
		Next: "请在打开的浏览器里完成登录(此步必须人工);随后轮询 hub.onboard.watch," +
			"变成 logged_in 后立即调用 hub.onboard.setup(confirmed=false) 拿该平台动作清单," +
			"用户确认后自动完成抓凭据→写 .env→热重载及适用的创建/权限/发布(无需用户点进应用,无需在终端执行任何命令)",
	}

	// 状态感知:用户可能早就登录了、甚至已经停在应用凭证页(调试 profile 长期
	// 保留登录态)。此时再列「扫码登录/建应用/开权限/发布」是纯粹的误导,
	// Agent 会把这些当成待办转述,流程就永远卡在第一步。
	if w, err := Watch(platform); err == nil && w.ChromeReady {
		switch {
		case readyToCapture(p, w):
			res.Phase = "ready_to_capture"
			res.Chrome = "already-running"
			res.PageOpened = true // 目标页面已打开,不再新开标签干扰用户
			res.URL = w.DetailURL // 返回真实停留的详情页,不要拿 Portal.URL 冒充"已打开"
			res.ManualSteps = []string{}
			// 2026-11-12 真机:提前返回也必须把详情页标签激活到前台。
			// 否则 profile 里残留的官网营销页标签仍是用户看到的页面,工具却报
			// ready_to_capture —— 表现就是「页面一直停在官网、看着像没干活」。
			activatePlatformTab(p)
			res.Next = "检测到你已登录且已停在凭据页,无需扫码、无需创建应用;" +
				"请立即调用 hub.onboard.setup(confirmed=false) 拿该平台动作清单,用户确认后重调完成抓凭据→写 .env→热重载及适用的权限/发布。" +
				"不要再提示用户登录、点应用或建应用;只需抓凭据时才用 hub.onboard.capture"
			return res, nil
		case w.LoginState == "logged_in":
			res.Phase = "logged_in"
			res.ManualSteps = []string{} // 点应用/建应用/开权限/发布均由 setup 代做,不再列为人工待办
			// 应用列表页由 Agent 自动打开/激活,不需要用户点顶部「应用开发」。
			// 停在入口页时列表读不到应用,setup 会误判成「需要创建新应用」。
			if listURL, ok := ensureAppListTab(p); ok {
				res.Chrome = "already-running"
				res.PageOpened = true
				res.URL = listURL
				res.Next = "已登录,Agent 已自动打开/激活" + p.Title + "的应用列表页(无需用户点「应用开发」、无需切终端);" +
					"立即调用 hub.onboard.setup(confirmed=false) 拿动作清单,确认后重调即可自动进入已有应用/创建新应用并完成后续配置"
				return res, nil
			}
			if p.AppListURL != "" {
				res.Warning = "应用列表页自动打开失败,已回退到平台入口页;可重跑 hub.onboard.start"
			}
			res.Next = "已登录,无需扫码;立即调用 hub.onboard.setup(confirmed=false) 拿动作清单,确认后重调即可自动打开/创建应用并完成后续配置;" +
				"不要让用户自己点进应用"
		case w.LoginState == "waiting_login":
			res.Phase = "waiting_login"
			if p.AppListURL != "" {
				res.Next = "用户尚未完成扫码登录;继续轮询 hub.onboard.watch,变成 logged_in 后调用 hub.onboard.setup(confirmed=false)。" +
					"Agent 会自动进入" + p.Title + "的应用列表页,不需要用户点「应用开发」,也不用切终端"
			}
		}
	}

	if debugChromeAlive() {
		res.Chrome = "already-running"
	} else {
		bin, err := findChrome()
		if err == nil {
			err = startDebugChrome(bin, chromeProfileDir())
		}
		if err != nil {
			res.Chrome = "unavailable"
			res.Warning = "调试 Chrome 未就绪: " + err.Error()
		} else {
			res.Chrome = "started"
		}
	}

	// 2026-11-12 用户截图报障:start 反复重试时浏览器里堆了 5 个「企业微信」
	// 官网标签(work.weixin.qq.com/#/app/workbenchEditor/index 营销页),而真正
	// 要操作的是管理后台 /wework_admin/frame。Portal.URL 是官网首页,有实测
	// AppListURL 的平台(钉钉/企微)一律直接进管理后台,不再堆无用的官网标签。
	entryURL := platformEntryURL(p)
	res.URL = entryURL
	res.PageOpened = openPlatformEntryTab(p, entryURL)
	if !res.PageOpened {
		// 调试端口不可用时的兜底:至少把页面呈现给用户(登录态可能不共享)。
		// openURLWithSystem 自带节流,Agent 轮询重试 start 不会反复开同一个标签。
		res.PageOpened = openURLWithSystem(entryURL)
		if res.Warning == "" {
			res.Warning = "调试 Chrome 中打开页面失败,已改用系统默认浏览器(登录态可能与调试实例不共享;同一页面不会重复打开)"
		}
	}
	return res, nil
}

// platformEntryURL 返回 start 应打开的真实入口页。
// 只改 start 的落点,不动 Portal.URL 本身:官网首页仍是有效的登录入口,
// 且 portalPageBelongs / matchURLPrefix 等判定都依赖它,改数据面影响更大。
func platformEntryURL(p *Portal) string {
	if p != nil && p.AppListURL != "" {
		return p.AppListURL
	}
	return p.URL
}

// readyToCapture 是 start 提前返回 ready_to_capture 的闸门。
// 除了 InAppDetail,还要求 DetailURL 确实属于本平台域:调试 profile 里
// 残留的其他平台详情页不能再让 start 跳过打开目标页面。
func readyToCapture(p *Portal, w *WatchResult) bool {
	return w != nil && w.ChromeReady && w.InAppDetail &&
		w.DetailURL != "" && portalPageBelongs(p, w.DetailURL)
}

// manualSteps 抽取清单中必须人工完成的步骤(无浏览器代填动作的步骤)。
func manualSteps(p *Portal) []string {
	var out []string
	for _, s := range p.Steps {
		// 只有登录(扫码/验证码)与管理员审批必须人工。点应用/创建应用/开权限/发布
		// 已由 hub.onboard.setup 代做,若仍按"Browser==nil"列进人工待办,Agent 会把
		// 这些当成待办转述给用户,又退回到让用户手动点的老流程。
		if s.ID == "login" {
			out = append(out, s.Brief)
		}
	}
	return out
}

func debugChromeAlive() bool {
	// 两个端点任一可用即认为调试服务在线:/json/version 偶发在页面刚打开时
	// 短暂不可用,但 /json/list 仍正常,单端点探测会把已启动误判成 browser_missing。
	for _, path := range []string{"/json/version", "/json/list"} {
		resp, err := cdpHTTPClient.Get(chromeDebugURL + path)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
	}
	return false
}

// debugChromeReady 吸收"刚启动/刚开标签"的窗口期:Chrome 处理完 /json/new
// 后调试 HTTP 服务可能短暂不响应 /json/*(新标签初始化/页面重定向期间)。
// 只读类工具(Watch/Capture)用它,避免把在线的浏览器误报成 browser_missing
// 而诱导 Agent 反复重开窗口;启动轮询仍用单次的 debugChromeAlive 保持灵敏。
func debugChromeReady() bool {
	for i := 0; i < 4; i++ {
		if debugChromeAlive() {
			return true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

// chromeProfileDir 与 scripts/chrome-debug.sh 保持一致,支持 CHROME_DEBUG_PROFILE 覆盖。
func chromeProfileDir() string {
	if p := strings.TrimSpace(os.Getenv("CHROME_DEBUG_PROFILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".chrome-debug-profile")
	}
	return filepath.Join(home, ".chrome-debug-profile")
}

// findChrome 按优先级探测 Chrome 可执行文件,与 scripts/chrome-debug.sh 一致。
func findChrome() (string, error) {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	if p := os.Getenv("CHROME_BIN"); p != "" {
		candidates = append([]string{p}, candidates...)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("未找到 Chrome/Chromium 可执行文件(可用 CHROME_BIN 指定)")
}

// chromeLaunchArgs 是调试 Chrome 的唯一启动参数来源。
// scripts/chrome-debug.sh 必须与之保持同步(同一 profile、同一组 flag)。
//
// 2026-11-12 第十四轮真机实测(README):本机不带 GPU 相关 flag 时调试 Chrome
// 会 exit 133 / "GPU process isn't usable" → start 起不出实例 → 每轮重试都
// 走 openURLWithSystem 兜底,在日常浏览器里堆标签。
// --window-size 同轮报障补齐:企微/钉钉管理后台是固定宽度布局,默认窗口过窄
// 会横向溢出(左侧导航被裁掉半个词、底部出现横向滚动条),即「样式错乱」。
func chromeLaunchArgs(profile string) []string {
	return []string{
		"--remote-debugging-port=9222",
		"--user-data-dir=" + profile,
		"--profile-directory=Default",
		"--no-first-run",
		"--no-default-browser-check",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--window-size=1440,900",
	}
}

// startDebugChrome 后台启动常驻调试实例。
//
// 关键点:Hub 在 stdio 模式下 stdout 是 MCP 协议通道,子进程的 stdio 必须
// 全部丢弃(nil 由 os/exec 接到 os.DevNull),否则 Chrome 的日志会污染协议帧;
// Setsid 让 Chrome 脱离 Hub 的会话,Hub 重启/退出时不会带走浏览器。
func startDebugChrome(bin, profile string) error {
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return fmt.Errorf("创建 profile 目录失败: %w", err)
	}
	args := chromeLaunchArgs(profile)
	// 后台直接 exec(实测 /usr/bin/open -na 会把 --remote-debugging-port 等
	// 参数丢掉,起出来的是不带调试端口的普通实例)。stdio 全部丢弃防止污染
	// MCP 协议通道;Setsid 让 Chrome 脱离 Hub 会话,Hub 退出不连带关闭浏览器;
	// Process.Release 释放父进程句柄,避免 Go 运行时持有僵尸引用。
	cmd := exec.Command(bin, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	_ = cmd.Process.Release()
	// 轮询等调试端口就绪后再开页面
	deadline := time.Now().Add(chromeWaitMax)
	for time.Now().Before(deadline) {
		if debugChromeAlive() {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("Chrome 已启动但 %s 调试端点在 %s 内未就绪", chromeDebugURL, chromeWaitMax)
}

// openPlatformEntryTab 打开平台入口页(供 start 使用)。
//
// 开放平台入口常会 302 到独立登录域(2026-11-11 实测:钉钉
// open-dev.dingtalk.com → login.dingtalk.com/oauth2/challenge.htm),此时只按
// URL 精确匹配的幂等判断失效,Agent 每次重试 start 都会再开一个标签。
// 入口页只要求「该平台域内已有标签」,命中即激活到前台、不再新开;具体应用
// 详情页仍走 openDebugTab 的精确匹配,避免 setup 把首页误当成详情页。
//
// entryPendingTab 补上第二层幂等:新标签刚创建时 URL 还是 about:blank,
// 不属于任何平台域,上面的循环命中不了。ensureAppListTab 早已用同样的
// pending 机制解决列表页,入口页漏了这一层 —— 2026-11-12 用户截图里
// 连开 5 个「企业微信」标签即由此而来。
var entryPendingTab = struct {
	sync.Mutex
	byPlatform map[string]string
}{byPlatform: map[string]string{}}

func openPlatformEntryTab(p *Portal, rawURL string) bool {
	key := ""
	if p != nil {
		key = p.Name
	}
	entryPendingTab.Lock()
	defer entryPendingTab.Unlock()
	if p != nil {
		if resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list"); err == nil {
			var items []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
				ID   string `json:"id"`
			}
			ok := json.NewDecoder(resp.Body).Decode(&items) == nil
			resp.Body.Close()
			if ok {
				// 优先激活「有用的」页面:调试 profile 里常同时残留官网营销页
				// (work.weixin.qq.com/#/app/workbenchEditor/index)和管理后台标签,
				// 只按域匹配会激活前者,用户以为 start 没把自己带进后台。
				for _, it := range items {
					if it.Type == "page" && portalPageBelongs(p, it.URL) &&
						(isAppListPage(p, it.URL) || portalDetailMatch(p, it.URL)) {
						activateDebugTab(it.ID)
						delete(entryPendingTab.byPlatform, key)
						return true
					}
				}
				// 退化:登录页/入口页也算「已打开」,避免 302 后反复新开。
				for _, it := range items {
					if it.Type == "page" && portalPageBelongs(p, it.URL) {
						activateDebugTab(it.ID)
						delete(entryPendingTab.byPlatform, key)
						return true
					}
				}
			}
		}
		// 标签已创建但仍在加载中:URL 还不属于本平台域,靠 ID 兜底复用。
		if id := entryPendingTab.byPlatform[key]; id != "" && debugTabAlive(id) {
			activateDebugTab(id)
			return true
		}
	}
	id, ok := openDebugTabID(rawURL)
	if ok && id != "" && key != "" {
		entryPendingTab.byPlatform[key] = id
	}
	return ok
}

// openDebugTab 通过 CDP 的 /json/new 在调试实例里开新标签,并把目标标签切到前台。
// 相比 rod 连接,这个 HTTP 端点不阻塞、不需要等页面加载完成、不会误关用户 Chrome。
// 新版 Chrome 要求 PUT(GET 已被禁用)。
func openDebugTab(rawURL string) bool {
	_, ok := openDebugTabID(rawURL)
	return ok
}

// openDebugTabID 与 openDebugTab 相同,但返回命中/新建的标签 ID。
// 应用列表页的自动导航需要记住刚创建的 ID:新标签注册进 /json/list 之前,
// 连续调用会各开一个重复标签(2026-11-11 实测)。
func openDebugTabID(rawURL string) (string, bool) {
	// 幂等:已有同 URL 标签就不再新开,但仍要激活到前台——调试 profile 里常留着
	// 上一次接入的其他平台标签,不激活的话用户看到的仍是旧页面(2026-11-11 实测)。
	if resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list"); err == nil {
		var items []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
			ID   string `json:"id"`
		}
		ok := json.NewDecoder(resp.Body).Decode(&items) == nil
		resp.Body.Close()
		if ok {
			for _, it := range items {
				if it.Type == "page" && it.URL == rawURL {
					activateDebugTab(it.ID)
					return it.ID, true
				}
			}
		}
	}
	req, err := http.NewRequest(http.MethodPut,
		chromeDebugURL+"/json/new?"+url.QueryEscape(rawURL), nil)
	if err != nil {
		return "", false
	}
	resp, err := cdpHTTPClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var created struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&created) == nil {
		activateDebugTab(created.ID)
		return created.ID, true
	}
	return "", true
}

// activatePlatformTab 只读地把目标平台「最有用」的标签切到前台,绝不新开标签。
// 优先级与 openPlatformEntryTab 一致:详情页 > 应用列表页 > 任意域内标签。
// start 的 ready_to_capture 提前返回分支用它,避免「已就绪但前台是残留官网页」。
func activatePlatformTab(p *Portal) bool {
	if p == nil {
		return false
	}
	resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var items []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		ID   string `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&items) != nil {
		return false
	}
	fallback := ""
	for _, it := range items {
		if it.Type != "page" || !portalPageBelongs(p, it.URL) {
			continue
		}
		if portalDetailMatch(p, it.URL) || isAppListPage(p, it.URL) {
			activateDebugTab(it.ID)
			return true
		}
		if fallback == "" {
			fallback = it.ID
		}
	}
	if fallback != "" {
		activateDebugTab(fallback)
		return true
	}
	return false
}

// activateDebugTab 把指定标签切到前台(CDP /json/activate/<id>)。
// 激活失败不影响页面已打开这一结果,故静默忽略。
func activateDebugTab(id string) {
	if id == "" {
		return
	}
	resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/activate/" + url.PathEscape(id))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// systemOpenRun 可注入,单测里替换掉以免真的拉起系统浏览器。
var systemOpenRun = func(rawURL string) error {
	return exec.Command("open", rawURL).Run()
}

// systemOpenGuard 防止 start 重试时在系统默认浏览器里反复打开同一入口页。
// 调试 Chrome 起不来的场景(见 chromeLaunchArgs 的 GPU flag),Agent 每轮询
// 一次就重试一次 start,旧实现每次都调 `open <url>` → 日常浏览器标签无限
// 堆积;这些标签不受 CDP 管辖,执行器再也找不到目标页面,于是继续重试。
var systemOpenGuard = struct {
	sync.Mutex
	lastURL string
	lastAt  time.Time
}{}

const systemOpenThrottle = 5 * time.Minute

// openURLWithSystem 兜底:macOS 用 open 交给默认浏览器。
// 返回 true 表示目标页面已呈现给用户(含命中节流、本次未重复打开的情况)。
func openURLWithSystem(rawURL string) bool {
	systemOpenGuard.Lock()
	defer systemOpenGuard.Unlock()
	if systemOpenGuard.lastURL == rawURL && time.Since(systemOpenGuard.lastAt) < systemOpenThrottle {
		return true
	}
	if err := systemOpenRun(rawURL); err != nil {
		return false
	}
	systemOpenGuard.lastURL, systemOpenGuard.lastAt = rawURL, time.Now()
	return true
}
