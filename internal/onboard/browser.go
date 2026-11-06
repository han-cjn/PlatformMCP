package onboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// ErrSelectorMiss 浏览器执行器某步失败(选择器失效/页面结构变化/元素超时),
// 编排器收到后当场把该步骤降级为手动说明,流程不中断。
var ErrSelectorMiss = errors.New("浏览器步骤未命中(选择器失效或页面超时)")

// 浏览器执行器的时间常量(集中定义,便于按平台调优)。
const (
	browserClickTimeout = 8 * time.Second        // 单元素点击超时
	browserPollInterval = 500 * time.Millisecond // 标签轮询间隔
	browserWaitURLMax   = 60 * time.Second       // waitURL 最长等待
	browserMaskRetries  = 10                     // 掩码重试次数
)

// BrowserExecutor 直连用户已登录的 Chrome 调试端口执行清单步骤。
// 合规边界:只做表单代填与凭据抄写,绝不碰登录(扫码/短信/验证码永远人工)。
type BrowserExecutor struct {
	browser   *rod.Browser
	page      *rod.Page
	initial   *rod.Page         // NewBrowserExecutor 开的初始 about:blank 页;Close 只关它,不碰运行中切换到的用户页面
	Collected map[string]string // readText 抽取的值(如 App ID),供向导回显确认
}

// NewBrowserExecutor 连接调试端口(默认 http://127.0.0.1:9222)。
// 失败即返回错误,编排器降级为纯手动模式。
//
// 重要前提:Chrome 必须指向持久化 profile 启动(--user-data-dir=~/.chrome-debug-profile)。
// 若 Chrome 用了临时 profile(/tmp/...),会静默丢失登录态——这是最常见的「为什么
// 登录状态没了」的根因,此函数对此类情况直接拒绝并给出修复命令。
func NewBrowserExecutor(debugURL string) (*BrowserExecutor, error) {
	if debugURL == "" {
		debugURL = "http://127.0.0.1:9222"
	}
	// rod 不能直接连 http URL:先经 /json/version 解析真实 WebSocket 地址(2026-11 实测)
	wsURL, err := resolveWebSocketURL(debugURL)
	if err != nil {
		return nil, err
	}
	b := rod.New().ControlURL(wsURL)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("连接 Chrome 调试端口失败(需以 --remote-debugging-port=9222 启动并登录平台): %w", err)
	}
	bx := &BrowserExecutor{browser: b, Collected: map[string]string{}}
	// rod 以 Must* API 为主,统一 recover 转 ErrSelectorMiss
	var p *rod.Page
	if err := bx.safe(func() { p = b.MustPage("about:blank") }); err != nil {
		// 绝不能调 b.Close():rod 的 Browser.Close 会发 CDP Browser.close 关掉用户的整个 Chrome。
		// 这里只放弃本次连接(随进程退出自然断开),浏览器保持原样。
		return nil, err
	}
	bx.page = p
	bx.initial = p
	return bx, nil
}

// chromeDebugStartHint 指引用户用持久化 profile 重启 Chrome 的命令模板。
const chromeDebugStartHint = `请用持久化 profile 重启 Chrome,否则登录态会丢:
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    --remote-debugging-port=9222 \
    --user-data-dir="$HOME/.chrome-debug-profile" \
    --profile-directory=Default
(首次需在新窗口里登录一次飞书,之后登录态会保留在 ~/.chrome-debug-profile)`

// resolveWebSocketURL Chrome 的 http 调试端口只提供元信息,WS 地址要从 /json/version 拿。
// 同步校验 profile:临时 profile(/tmp、/var/folders、Chrome 自建的 .org.chromium.*)
// 上的登录态写不回磁盘,下次启动即丢——直接拒绝,免得代填脚本跑到一半才发现。
func resolveWebSocketURL(debugURL string) (string, error) {
	if !strings.HasPrefix(debugURL, "http") {
		return debugURL, nil // 调用方直接给了 ws:// 地址
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(debugURL, "/") + "/json/version")
	if err != nil {
		return "", fmt.Errorf("访问 %s/json/version 失败(Chrome 未以调试端口启动?): %w", debugURL, err)
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		UserDataDir          string `json:"userDataDir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("解析 %s/json/version 失败: %w", debugURL, err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%s/json/version 未返回 webSocketDebuggerUrl", debugURL)
	}
	// 临时 profile 检测:macOS 临时目录、Chrome 默认临时数据目录都会清登录态
	if dir := v.UserDataDir; dir != "" && isEphemeralProfileDir(dir) {
		return "", fmt.Errorf("检测到 Chrome 使用临时 profile(%s),登录态不会持久化。\n%s", dir, chromeDebugStartHint)
	}
	return v.WebSocketDebuggerURL, nil
}

// isEphemeralProfileDir 判断 userDataDir 是否落在临时目录(登录态写不回去)。
func isEphemeralProfileDir(dir string) bool {
	ephemeral := []string{"/tmp/", "/var/folders/", "/private/tmp/", "/private/var/folders/", ".org.chromium."}
	for _, p := range ephemeral {
		if strings.Contains(dir, p) {
			return true
		}
	}
	return false
}

// Close 关闭自身开的页面(不关浏览器本身,不碰用户其他标签页)。
// 注意:rod 的 browser.Close() 会发 CDP Browser.close 把整个 Chrome 关掉——
// 该浏览器是用户手动启动并登录的,Hub 无权关闭,这里只关自己开的页面,连接随进程退出自然断开。
// 用户可能已手动关掉那个空白页,因此关闭动作全程 recover,失败静默忽略。
func (bx *BrowserExecutor) Close() {
	if bx == nil || bx.initial == nil {
		return
	}
	// 只关自己开的初始空白页;waitURL 可能已把 bx.page 切到用户的详情页,绝不能关
	_ = bx.safe(func() { _ = bx.initial.Close() })
	bx.initial = nil
	bx.page = nil
}

// RunAction 执行一个步骤的浏览器动作序列。
func (bx *BrowserExecutor) RunAction(act *BrowserAction, creds map[string]string) error {
	if act == nil {
		return nil
	}
	for i, op := range act.Ops {
		if err := bx.runOp(op, creds); err != nil {
			return fmt.Errorf("%w: 脚本 %s 第 %d 步(%s %s): %v",
				ErrSelectorMiss, act.Script, i+1, op.Kind, op.Target, err)
		}
	}
	return nil
}

func (bx *BrowserExecutor) runOp(op Op, creds map[string]string) error {
	switch op.Kind {
	case "navigate":
		return bx.safe(func() {
			// 幂等:已有该 URL 的标签就切换过去,不把用户当前页面导走
			for _, p := range bx.browser.MustPages() {
				if info, err := p.Info(); err == nil && info != nil && info.URL == op.Target {
					bx.page = p
					return
				}
			}
			bx.page.MustNavigate(op.Target)
		})
	case "waitVisible":
		return bx.safe(func() { bx.page.MustElement(op.Target) })
	case "click":
		return bx.safe(func() {
			el, err := bx.pick(op)
			if err != nil {
				panic(err)
			}
			el.Timeout(browserClickTimeout).MustClick()
		})
	case "hover": // 飞书 Secret 按钮组是 hover 才展开的(2026-11 实测)
		return bx.safe(func() {
			el, err := bx.pick(op)
			if err != nil {
				panic(err)
			}
			el.MustHover()
		})
	case "fill":
		text := op.Text
		if v, ok := strings.CutPrefix(text, "$env:"); ok { // 支持引用已收集凭据
			text = creds[v]
		}
		return bx.safe(func() {
			el := bx.page.MustElement(op.Target)
			el.MustSelectAllText().MustInput(text) // 全选旧值后输入,实现覆盖式填表
		})
	case "readText":
		return bx.safe(func() {
			el, err := bx.pick(op)
			if err != nil {
				panic(err)
			}
			mask := op.MaskRune
			if mask == 0 {
				mask = defaultMaskRune
			}
			// 掩码重试:点完「显示」后 DOM 文本更新有延迟,读到掩码字符时等一会重读
			for i := 0; i < browserMaskRetries; i++ {
				text := strings.TrimSpace(el.MustText())
				if op.ExtractKey == "" || !strings.ContainsRune(text, mask) {
					if op.ExtractKey != "" {
						bx.Collected[op.ExtractKey] = text
					}
					return
				}
				time.Sleep(browserPollInterval)
			}
			panic(fmt.Sprintf("读到的仍是掩码文本(掩码字符 %q,显示按钮未生效?)", string(mask)))
		})
	case "waitURL": // 等待并切换到 URL 前缀匹配的标签(点应用卡片会新开标签)
		return bx.safe(func() {
			deadline := time.Now().Add(browserWaitURLMax)
			for time.Now().Before(deadline) {
				for _, p := range bx.browser.MustPages() {
					info, err := p.Info()
					if err != nil || info == nil {
						continue // 标签已关闭/正在跳转,跳过
					}
					if !matchURLPrefix(info.URL, op.Target, op.URLExcludes) {
						continue
					}
					bx.page = p
					return
				}
				time.Sleep(browserPollInterval)
			}
			panic(fmt.Sprintf("%v 内未出现 URL 前缀为 %s 的标签", browserWaitURLMax, op.Target))
		})
	case "screenshot": // 辅助人工核对当前页面状态
		return bx.safe(func() { bx.page.MustScreenshot(op.Target) })
	default:
		return fmt.Errorf("未知操作类型 %q", op.Kind)
	}
}

// matchURLPrefix 判断 url 是否命中 waitURL 目标:必须比 target 更长(排除列表页自身),
// 且不含任意排除片段(排除分页/搜索等假命中)。
func matchURLPrefix(url, target string, excludes []string) bool {
	if target == "" || !strings.HasPrefix(url, target) || len(url) <= len(target) {
		return false
	}
	for _, ex := range excludes {
		if ex != "" && strings.Contains(url, ex) {
			return false
		}
	}
	return true
}

// safe 统一把 rod Must* 的 panic 转为 ErrSelectorMiss 语义的普通错误。
func (bx *BrowserExecutor) safe(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	f()
	return nil
}

// pick 按选择器取第 Index 个匹配元素(多匹配时跳过前面 N 个)。
func (bx *BrowserExecutor) pick(op Op) (*rod.Element, error) {
	if op.Index < 0 {
		return nil, fmt.Errorf("Index 不能为负数(%d)", op.Index)
	}
	if bx.page == nil {
		return nil, errors.New("浏览器页面未初始化")
	}
	els, err := bx.page.Elements(op.Target)
	if err != nil {
		return nil, err
	}
	if len(els) <= op.Index {
		return nil, fmt.Errorf("选择器 %s 匹配 %d 个,不足第 %d 个", op.Target, len(els), op.Index+1)
	}
	return els[op.Index], nil
}
