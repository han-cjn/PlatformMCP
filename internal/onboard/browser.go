package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// ErrSelectorMiss 浏览器执行器某步失败(选择器失效/页面结构变化/元素超时),
// 编排器收到后当场把该步骤降级为手动说明,流程不中断。
var ErrSelectorMiss = errors.New("浏览器步骤未命中(选择器失效或页面超时)")

// 浏览器执行器的时间常量(集中定义,便于按平台调优)。
const (
	browserClickTimeout  = 8 * time.Second        // 单元素点击超时
	browserOpTimeout     = 6 * time.Second        // 单步元素查询/读文本的硬上限
	browserSessionMax    = 25 * time.Second       // 整个执行器会话的总时限(兜住所有 rod 无限重试)
	browserPollInterval  = 500 * time.Millisecond // 标签轮询间隔
	browserWaitURLMax    = 15 * time.Second       // waitURL 最长等待(须小于 session 时限)
	browserMaskRetries   = 10                     // 掩码重试次数
	browserNavigateLimit = 10 * time.Second       // 单次页面导航上限
)

// BrowserExecutor 直连用户已登录的 Chrome 调试端口执行清单步骤。
// 合规边界:只做表单代填与凭据抄写,绝不碰登录(扫码/短信/验证码永远人工)。
type BrowserExecutor struct {
	browser   *rod.Browser
	page      *rod.Page
	initial   *rod.Page          // 仅在浏览器一个标签都没有时才创建的 about:blank;Close 只关它
	cancel    context.CancelFunc // 会话总时限:取消后所有 rod 无限重试会立刻返回
	Collected map[string]string  // readText 抽取的值(如 App ID),供向导回显确认
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
	// 会话总时限:rod 的 Element/MustElement 是"重试到 ctx 结束",默认 ctx 永不过期。
	// 给整个执行器套一层 deadline,任何一步选择器失效都能在有限时间内返回,而不是
	// 让 MCP 调用永远停在"正在执行中"。
	ctx, cancel := context.WithTimeout(context.Background(), browserSessionMax)
	b := rod.New().ControlURL(wsURL).Context(ctx)
	if err := b.Connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("连接 Chrome 调试端口失败(需以 --remote-debugging-port=9222 启动并登录平台): %w", err)
	}
	bx := &BrowserExecutor{browser: b, cancel: cancel, Collected: map[string]string{}}

	// 绝不无条件新建 about:blank——那会在用户浏览器里留下空白页签(capture 一旦卡住,
	// defer Close 不执行,空白页就永久残留)。优先复用一个已存在的真实标签。
	var p *rod.Page
	if err := bx.safe(func() {
		for _, pg := range b.MustPages() {
			info, e := pg.Info()
			if e != nil || info == nil {
				continue
			}
			if !strings.HasPrefix(info.URL, "about:") && !strings.HasPrefix(info.URL, "chrome://") {
				p = pg
				return
			}
		}
	}); err != nil {
		cancel()
		return nil, err
	}
	if p == nil {
		// 只有浏览器里一个页面都没有时才开空白页,并记下来由 Close 负责关掉。
		// 绝不能调 b.Close():rod 的 Browser.Close 会发 CDP Browser.close 关掉用户整个 Chrome。
		if err := bx.safe(func() { p = b.MustPage("about:blank") }); err != nil {
			cancel()
			return nil, err
		}
		bx.initial = p
	}
	bx.page = p
	return bx, nil
}

// NewBrowserExecutorForPlatform 连接调试 Chrome,并把执行器绑定到目标平台的标签。
//
// 不能只复用 NewBrowserExecutor 的默认页:它取的是 MustPages() 里第一个真实标签,
// 顺序由 Chrome 决定。接入钉钉时若还留着飞书标签,创建表单 / inspect / readPage
// 会全跑在飞书页面上,表现为 selector_miss 且 candidates 是飞书元素。
// 找不到该平台标签时直接报错,由调用方降级,绝不在别的平台页面上执行动作。
func NewBrowserExecutorForPlatform(debugURL string, p *Portal) (*BrowserExecutor, error) {
	bx, err := NewBrowserExecutor(debugURL)
	if err != nil {
		return nil, err
	}
	if p != nil && !bx.FocusPlatform(p) {
		bx.Close()
		return nil, fmt.Errorf("调试 Chrome 里没有 %s 的标签(当前标签可能属于其他平台);先调用 hub.onboard.start 打开目标平台页面", p.Title)
	}
	return bx, nil
}

// FocusPlatform 把执行器绑定到属于目标平台域的标签并切到前台。
// 优先级由 pickPlatformPage 决定;找不到返回 false,保留 bx.page 不动。
func (bx *BrowserExecutor) FocusPlatform(p *Portal) bool {
	if bx == nil || p == nil {
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
		idx := pickPlatformPage(p, urls)
		if idx < 0 || idx >= len(pages) {
			return
		}
		bx.page = pages[idx]
		found = true
		_, _ = pages[idx].Activate()
	})
	return found
}

// CurrentURL 只读返回执行器当前绑定标签的 URL,供调用方确认没跑错平台。
func (bx *BrowserExecutor) CurrentURL() string {
	var raw string
	_ = bx.safe(func() {
		if info, e := bx.page.Info(); e == nil && info != nil {
			raw = info.URL
		}
	})
	return raw
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
	// 统一用 cdpHTTPClient:已禁用代理与 KeepAlives,否则宿主 HTTP_PROXY 会把
	// 127.0.0.1 的 CDP 请求转发成 502,表现为"调试端点不可达"。
	resp, err := cdpHTTPClient.Get(strings.TrimRight(debugURL, "/") + "/json/version")
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
	if bx == nil {
		return
	}
	// 只关自己开的初始空白页;waitURL 可能已把 bx.page 切到用户的详情页,绝不能关
	if bx.initial != nil {
		_ = bx.safe(func() { _ = bx.initial.Close() })
		bx.initial = nil
	}
	bx.page = nil
	// 取消会话 ctx:即便某步还在 rod 的重试循环里,也会立刻退出,不留悬挂 goroutine
	if bx.cancel != nil {
		bx.cancel()
		bx.cancel = nil
	}
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
			bx.page.Timeout(browserNavigateLimit).MustNavigate(op.Target)
		})
	case "waitVisible":
		// 必须有界:rod 的 Element 会无限重试到 ctx 结束,默认 ctx 没有 deadline。
		// 选择器失效时(例如详情页里等列表页的 app-card)会永久卡住 MCP 调用。
		// 带 Match 时走 pick(ElementR 自带 6s 重试),用来等 SPA 把菜单文本渲染出来,
		// 而不是只等到任意一个 div 出现就继续。
		if op.Match != "" {
			return bx.safe(func() {
				if _, err := bx.pick(op); err != nil {
					panic(err)
				}
			})
		}
		return bx.safe(func() { bx.page.Timeout(browserOpTimeout).MustElement(op.Target) })
	case "click":
		return bx.safe(func() {
			el, err := bx.pick(op)
			if err != nil {
				panic(err)
			}
			el.Timeout(browserClickTimeout).MustClick()
		})
	case "clickInRow": // 按行文本定位,再点该行内的动作按钮(见下方方法注释)
		return bx.clickInRow(op)
	case "hover": // 飞书 Secret 按钮组是 hover 才展开的(2026-11 实测)
		return bx.safe(func() {
			el, err := bx.pick(op)
			if err != nil {
				panic(err)
			}
			el.Timeout(browserOpTimeout).MustHover()
		})
	case "fill":
		text := op.Text
		if v, ok := strings.CutPrefix(text, "$env:"); ok { // 支持引用已收集凭据
			text = creds[v]
		}
		return bx.safe(func() {
			el, err := bx.pick(op)
			if err != nil {
				panic(err)
			}
			el.Timeout(browserOpTimeout).MustSelectAllText().MustInput(text) // 全选旧值后输入,实现覆盖式填表
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
				text := strings.TrimSpace(el.Timeout(browserOpTimeout).MustText())
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
	case "readPage": // 只读:抓整页可见文本,供幂等检测(权限已开通 / 版本已发布)
		return bx.safe(func() {
			// SPA 点完左侧菜单后 DOM 异步渲染,给一帧时间再读,减少读到旧页面的概率
			time.Sleep(500 * time.Millisecond)
			el := bx.page.Timeout(browserOpTimeout).MustElement("body")
			bx.Collected["__page_text__"] = el.Timeout(browserOpTimeout).MustText()
		})
	case "extract": // 从 readPage 抓到的整页文本里按正则抽取凭据,不依赖任何易变的 DOM class
		text := bx.Collected["__page_text__"]
		if text == "" {
			return fmt.Errorf("extract 必须先执行 readPage 采集页面文本")
		}
		re, err := regexp.Compile(op.Target)
		if err != nil {
			return fmt.Errorf("extract 正则非法 %q: %w", op.Target, err)
		}
		m := re.FindStringSubmatch(text)
		if len(m) == 0 {
			return fmt.Errorf("页面文本未匹配 /%s/,该平台可用 hub.onboard.inspect 校准,或改用 hub.onboard.set_credentials", op.Target)
		}
		val := strings.TrimSpace(m[0])
		if len(m) > 1 {
			val = strings.TrimSpace(m[1])
		}
		if val == "" {
			return fmt.Errorf("页面文本匹配到空值 /%s/", op.Target)
		}
		if op.ExtractKey != "" {
			bx.Collected[op.ExtractKey] = val
		}
		return nil
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
		return bx.safe(func() { bx.page.Timeout(browserOpTimeout).MustScreenshot(op.Target) })
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

// clickInRow 先在「可见文本命中 op.Match 的行」里定位,再点该行内文本为
// op.Text 的子元素。用于钉钉 /permission 页:每行 tr[data-row-key] 的文本
// 含权限点 code 与「立即开通」,若直接对整页按「立即开通」文本匹配,会点到
// 第一个未开通权限(而不是声明要开的那一个),造成误开通。
//
// 行或行内按钮没找到时返回普通错误(编排器降级为 selector_miss 并回传候选),
// 绝不重试提交、不点其他行、不猜可能出现的二次确认弹窗按钮。
//
// SkipIfMatch 提供幂等:钉钉 /permission 的权限开通后,行内按钮从「立即开通」
// 变成「移除权限」。若重跑 setup 时不跳过,第一个已开通权限就会 selector_miss,
// 后面的权限永远轮不到(2026-11-11 真机部分开通后复核发现)。
func (bx *BrowserExecutor) clickInRow(op Op) error {
	if op.Match == "" || op.Text == "" {
		return fmt.Errorf("clickInRow 需要 Match(行文本正则)与 Text(行内按钮文本)")
	}
	return bx.safe(func() {
		row, err := bx.page.Timeout(browserOpTimeout).ElementR(op.Target, op.Match)
		if err != nil {
			panic(fmt.Errorf("未找到文本匹配 /%s/ 的行 %q: %w", op.Match, op.Target, err))
		}
		if op.SkipIfMatch != "" {
			rowText, e := row.Text()
			if e == nil && regexp.MustCompile(op.SkipIfMatch).MatchString(rowText) {
				return // 该行已达成目标状态,不重复点击
			}
		}
		els, err := row.Elements(rowActionSelector)
		if err != nil {
			panic(err)
		}
		for _, el := range els {
			text, e := el.Text()
			if e != nil || strings.TrimSpace(text) != op.Text {
				continue
			}
			if e := el.Timeout(browserClickTimeout).Click(proto.InputMouseButtonLeft, 1); e != nil {
				panic(e)
			}
			return
		}
		panic(fmt.Errorf("行 /%s/ 内未找到按钮 %q(该权限可能已开通,或页面结构已变)", op.Match, op.Text))
	})
}

// pick 按选择器取第 Index 个匹配元素(多匹配时跳过前面 N 个)。
// op.Match 非空时改用「按可见文本匹配」:飞书开放平台等 React SPA 的 class 随
// 改版变化,按钮文本比 class 稳定得多;此时 Target 仍可写逗号分隔的多候选。
func (bx *BrowserExecutor) pick(op Op) (*rod.Element, error) {
	if op.Index < 0 {
		return nil, fmt.Errorf("Index 不能为负数(%d)", op.Index)
	}
	if bx.page == nil {
		return nil, errors.New("浏览器页面未初始化")
	}
	if op.Match != "" {
		if op.Index != 0 {
			return nil, fmt.Errorf("Match 与 Index 不能同时使用(Index=%d)", op.Index)
		}
		el, err := bx.page.Timeout(browserOpTimeout).ElementR(op.Target, op.Match)
		if err != nil {
			return nil, fmt.Errorf("按文本 /%s/ 在 %q 中未匹配到元素: %w", op.Match, op.Target, err)
		}
		return el, nil
	}
	els, err := bx.page.Timeout(browserOpTimeout).Elements(op.Target)
	if err != nil {
		return nil, err
	}
	if len(els) <= op.Index {
		return nil, fmt.Errorf("选择器 %s 匹配 %d 个,不足第 %d 个", op.Target, len(els), op.Index+1)
	}
	return els[op.Index], nil
}
