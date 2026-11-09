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
			"变成 logged_in 后立即调用 hub.onboard.setup 自动打开/创建应用、抓凭据→写 .env→" +
			"热重载→开权限→发布版本(无需用户点进应用,无需在终端执行任何命令)",
	}

	// 状态感知:用户可能早就登录了、甚至已经停在应用凭证页(调试 profile 长期
	// 保留登录态)。此时再列「扫码登录/建应用/开权限/发布」是纯粹的误导,
	// Agent 会把这些当成待办转述,流程就永远卡在第一步。
	if w, err := Watch(platform); err == nil && w.ChromeReady {
		switch {
		case w.InAppDetail:
			res.Phase = "ready_to_capture"
			res.Chrome = "already-running"
			res.PageOpened = true // 目标页面已打开,不再新开标签干扰用户
			res.ManualSteps = []string{}
			res.Next = "检测到你已登录且已停在应用「凭证与基础信息」页,无需扫码、无需创建应用;" +
				"请立即调用 hub.onboard.setup(confirmed=false) 拿动作清单,用户确认后重调完成抓凭据→写 .env→热重载→开权限→发布。" +
				"不要再提示用户登录、点应用或建应用;只需抓凭据时才用 hub.onboard.capture"
			return res, nil
		case w.LoginState == "logged_in":
			res.Phase = "logged_in"
			res.ManualSteps = []string{} // 点应用/建应用/开权限/发布均由 setup 代做,不再列为人工待办
			res.Next = "已登录,无需扫码;立即调用 hub.onboard.setup(confirmed=false) 拿动作清单,确认后重调即可自动打开/创建应用并完成后续配置;" +
				"不要让用户自己点进应用"
		case w.LoginState == "waiting_login":
			res.Phase = "waiting_login"
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

	res.PageOpened = openDebugTab(p.URL)
	if !res.PageOpened {
		// 调试端口不可用时的兜底:至少把页面呈现给用户(登录态可能不共享)
		res.PageOpened = openURLWithSystem(p.URL)
		if res.Warning == "" {
			res.Warning = "调试 Chrome 中打开页面失败,已改用系统默认浏览器(登录态可能与调试实例不共享)"
		}
	}
	return res, nil
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

// startDebugChrome 后台启动常驻调试实例。
//
// 关键点:Hub 在 stdio 模式下 stdout 是 MCP 协议通道,子进程的 stdio 必须
// 全部丢弃(nil 由 os/exec 接到 os.DevNull),否则 Chrome 的日志会污染协议帧;
// Setsid 让 Chrome 脱离 Hub 的会话,Hub 重启/退出时不会带走浏览器。
func startDebugChrome(bin, profile string) error {
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return fmt.Errorf("创建 profile 目录失败: %w", err)
	}
	args := []string{
		"--remote-debugging-port=9222",
		"--user-data-dir=" + profile,
		"--profile-directory=Default",
		"--no-first-run",
		"--no-default-browser-check",
	}
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

// openDebugTab 通过 CDP 的 /json/new 在调试实例里开新标签。
// 相比 rod 连接,这个 HTTP 端点不阻塞、不需要等页面加载完成、不会误关用户 Chrome。
// 新版 Chrome 要求 PUT(GET 已被禁用)。
func openDebugTab(rawURL string) bool {
	// 幂等:已有同 URL 标签就不再新开。Agent 每次重试 start 都开一个新标签,
	// 会让 /json/list 堆满重复页,watch 的阶段判定也更容易被干扰。
	if resp, err := cdpHTTPClient.Get(chromeDebugURL + "/json/list"); err == nil {
		var items []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		}
		ok := json.NewDecoder(resp.Body).Decode(&items) == nil
		resp.Body.Close()
		if ok {
			for _, it := range items {
				if it.Type == "page" && it.URL == rawURL {
					return true
				}
			}
		}
	}
	req, err := http.NewRequest(http.MethodPut,
		chromeDebugURL+"/json/new?"+url.QueryEscape(rawURL), nil)
	if err != nil {
		return false
	}
	resp, err := cdpHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// openURLWithSystem 兜底:macOS 用 open 交给默认浏览器。
func openURLWithSystem(rawURL string) bool {
	return exec.Command("open", rawURL).Run() == nil
}
