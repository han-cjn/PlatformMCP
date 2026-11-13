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

// ensurePageVisible 在任何交互 Op 之前把绑定的标签切到前台。
//
// 2026-11-12 真机分步诊断(堆栈留证 /tmp/hub-step-probe.log):调试 profile 里
// 常同时留着官网营销页/新标签页,执行器即使按平台选中了详情页,该标签仍可能是
// visibilityState=hidden。Chrome 会暂停后台标签的 requestAnimationFrame,于是
// rod 的 Element.Click → WaitInteractable → ScrollIntoView → WaitStableRAF
// 永久等不到回调,CDP 调用挂死,直到会话 ctx(25s)到期,对外表现为
// capture_failed / "context deadline exceeded"。
// 复现条件与第十七轮相同(详情页已渲染、复制图标可定位、坐标在视口内),
// 唯一差异就是标签不在前台 —— 所以必须在执行器层兜住,不能靠调用方记得点激活。
func (bx *BrowserExecutor) ensurePageVisible() {
	if bx == nil || bx.page == nil {
		return
	}
	_ = bx.safe(func() {
		// 只把标签切到前台(有助于剪贴板权限与输入事件),不等待 visibilityState:
		// 窗口最小化 / 不在 macOS 前台时它仍是 hidden,而点击已改为 CDP 坐标
		// 派发、不依赖 rAF,无需在这里耗掉会话预算。
		_, _ = bx.page.Activate()
	})
}

func (bx *BrowserExecutor) runOp(op Op, creds map[string]string) error {
	bx.ensurePageVisible()
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
	case "clickAt": // CDP 坐标点击,不依赖 WaitStableRAF(见 clickByCoords 注释)
		return bx.clickByCoords(op)
	case "fillJS": // 纯 JS setter 填值,不依赖 rod 的 MustInput/WaitInteractable
		return bx.fillByJS(op, creds)
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
			// 读回校验:钉钉版本表单的 textarea#description 是 antd 受控组件,
			// MustInput 的键盘事件在焦点未落入时静默丢失(2026-11-12 真机实测:
			// 版本号 fill 生效、描述 fill 落空,版本描述又是必填项,导致「保存」
			// 校验失败、草稿未生成)。读回用 JS 读 el.value(input/textarea 的
			// textContent 不反映当前值);不一致时降级 JS 原生 setter + input
			// 事件(React 受控组件标准填法),再读回一次;仍不一致才报错。
			// 已验证平台(飞书)读回一致时行为与旧版完全一致。
			readJS := `(sel) => { const el = document.querySelector(sel); return el ? el.value : null }`
			readBack := func() string {
				res, err := bx.page.Timeout(browserOpTimeout).Eval(readJS, op.Target)
				if err != nil {
					return ""
				}
				return strings.TrimSpace(res.Value.String())
			}
			if readBack() != strings.TrimSpace(text) {
				setJS := `({sel, val}) => {
					const el = document.querySelector(sel);
					if (!el) return 'not-found';
					const proto = el.tagName === 'TEXTAREA' ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype;
					const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
					setter.call(el, val);
					el.dispatchEvent(new Event('input', {bubbles: true}));
					el.dispatchEvent(new Event('change', {bubbles: true}));
					return el.value;
				}`
				res, err := bx.page.Timeout(browserOpTimeout).Eval(setJS, map[string]string{"sel": op.Target, "val": text})
				if err != nil {
					panic(err)
				}
				if readBack() != strings.TrimSpace(text) {
					panic(fmt.Sprintf("fill 校验失败:目标=%s 读回=%q 期望=%q", op.Target, res.Value.String(), text))
				}
			}
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
	case "copyToCred": // 点「复制」图标读明文(钉钉 Client Secret 渲染成掩码但复制给出明文)
		return bx.copyToCred(op)
	case "copyInFrame": // 同源 iframe 内的文本型「复制」控件(钉钉 /bot 页的「复制 RobotCode」)
		return bx.copyInFrame(op)
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
// pickByText 在 op.Target 里按可见文本正则找元素,并优先返回「真正可点击的实体」。
//
// 为什么不能直接用 ElementR:ElementR 返回文档顺序的第一个匹配元素,而
// menuCandidateSelector 里 div 排在 button 之前。以钉钉「保存」为例,页面上有
// div.sectionBottom(宽 968px 的按钮容器)、内层 button.dtd-btn-primary 和
// 内层 span 三个元素的文本都是「保存」,ElementR 选中的是那个 div —— 点它的
// 中心/左边缘不会触发 button 上的 React 事件,点击静默无效。2026-11-12 三次
// 真机「保存」全部失败、草稿始终不生成,根因就是这个(与 fill 无关;
// rowActionSelector 的注释里记过同类坑,当时只修了 clickInRow)。
//
// 策略:先用 JS 在候选集合里按文本+可点击性打分,取出最优元素在列表中的
// 下标,再按同一选择器取回 Element;取不到时退回 ElementR 保持旧行为。
func (bx *BrowserExecutor) pickByText(target, match string) (*rod.Element, error) {
	re, err := regexp.Compile(match)
	if err != nil {
		return bx.page.Timeout(browserOpTimeout).ElementR(target, match)
	}
	els, err := bx.page.Timeout(browserOpTimeout).Elements(target)
	if err != nil || len(els) == 0 {
		return bx.page.Timeout(browserOpTimeout).ElementR(target, match)
	}

	best, bestScore := -1, -1
	for i, el := range els {
		text, err := el.Timeout(browserOpTimeout).Text()
		if err != nil || !re.MatchString(strings.TrimSpace(text)) {
			continue
		}
		score := clickability(el)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		// 文本匹配口径与 ElementR 不完全一致(如含嵌套文本)时退回旧实现
		return bx.page.Timeout(browserOpTimeout).ElementR(target, match)
	}
	return els[best], nil
}

// clickability 给候选元素的可点击性打分:越高越优先。
// 真正的交互实体(button/[role=button]/a)加分,纯容器(div/li 等)减分;
// 不可见或零尺寸的元素直接判负,避免点到隐藏节点。
func clickability(el *rod.Element) int {
	res, err := el.Eval(`() => {
		const el = this;
		const r = el.getBoundingClientRect();
		const cs = getComputedStyle(el);
		const tag = el.tagName;
		let score = 0;
		if (tag === 'BUTTON' || tag === 'A') score += 40;
		if (el.getAttribute('role') === 'button' || el.getAttribute('role') === 'menuitem') score += 35;
		if (tag === 'SPAN') score += 20;
		if (tag === 'LI') score += 5;
		if (tag === 'DIV') score -= 30;
		// 容器越宽越可能是包裹层:真正的按钮通常尺寸贴近文字
		if (r.width > 300) score -= 25;
		if (r.height > 60) score -= 15;
		if (cs.visibility !== 'visible' || cs.display === 'none' || cs.opacity === '0') score -= 200;
		if (r.width === 0 || r.height === 0) score -= 200;
		if (el.disabled === true || el.getAttribute('aria-disabled') === 'true') score -= 150;
		// 内层元素(其子节点里还有同名可点实体)通常是文本节点,轻微加分
		if (el.querySelector('button, [role=button], a')) score -= 10;
		return score;
	}`)
	if err != nil {
		return 0
	}
	n := res.Value.Num()
	return int(n)
}

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
		el, err := bx.pickByText(op.Target, op.Match)
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

// copyToCred 点击凭证块里的「复制」图标,把复制到剪贴板的**明文**存入
// bx.Collected[op.ExtractKey]。
//
// 背景(2026-11-12 用户手工实测证伪了旧结论):钉钉 /baseinfo 的 Client Secret
// 渲染成「当前使用: -JxJlo7zXy****pY7Ah3PcyG」掩码,但同一行右侧的复制图标
// 写入剪贴板的是**完整明文**。此前 collect-creds 只 readPage + extract,把掩码
// 交给格式校验后降级 set_credentials,并在注释/文案里把「渲染成掩码」错误写成
// 「平台不提供明文」——从没人点过那个复制按钮。
//
// 合规与隐私边界:
//   - 主路径注入 hook 捕获 navigator.clipboard.writeText / execCommand('copy')
//     的参数,**不读系统剪贴板**,不碰用户剪贴板里的其它内容;
//   - 只有 hook 没捕获(页面用了未知复制实现)时才降级 navigator.clipboard.readText(),
//     并通过 CDP Browser.grantPermissions 授予 clipboard 权限;
//   - 读到的值只进内存 Collected,绝不打印、不落日志;是否合规由
//     commitCredentials 的格式校验统一判定,这里只拒绝空值(掩码照常交给
//     invalid_format 降级,不伪装成成功)。
//
// 定位策略(不依赖易变的 class):Target 为候选容器选择器,Match 为凭证块文本正则,
// Text 为复制按钮选择器。按「文本最短」排序取最小容器,避免 Client ID / Client
// Secret 同处一个 <tr> 时误点 Client ID 的复制按钮。找不到就 selector_miss 降级,
// 绝不猜按钮、绝不重试。
// clickAtGeoJS 按 CSS 选择器(可选可见文本正则)定位元素,滚入视口后返回中心坐标。
// 全部在页面里同步完成,不触发 rod 的 WaitInteractable / WaitStableRAF。
const clickAtGeoJS = `({sel, match, index}) => {
	const els = [...document.querySelectorAll(sel)];
	const re = match ? new RegExp(match) : null;
	// 无 Match 时按 Index 取(与既有 pick() 的 0-based 语义一致):
	// 企微模式 radio 的 label 自身没有文本,文本在旁边的卡片标题里,
	// 只能靠「第 2 个 label = API 模式」定位(2026-11-12 真机)。
	const el = re ? els.find(e => re.test((e.innerText || e.textContent || "").trim())) : els[index];
	if (!el) return "not-found:" + sel;
	el.scrollIntoView({block: "center", inline: "center"});
	const r = el.getBoundingClientRect();
	return JSON.stringify({x: r.left + r.width/2, y: r.top + r.height/2,
		w: r.width, h: r.height,
		inViewport: r.top >= 0 && r.top < innerHeight && r.left >= 0 && r.left < innerWidth});
}`

// fillByJSJS 用原生 value setter + input/change 事件写值(React 受控组件标准填法)。
const fillByJSJS = `({sel, val}) => {
	const el = document.querySelector(sel);
	if (!el) return "__not_found__";
	const proto = el.tagName === "TEXTAREA" ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype;
	const setter = Object.getOwnPropertyDescriptor(proto, "value").set;
	setter.call(el, val);
	el.dispatchEvent(new Event("input", {bubbles: true}));
	el.dispatchEvent(new Event("change", {bubbles: true}));
	return el.value;
}`

// clickByCoords 用 CDP 原生鼠标事件按坐标点击,不依赖标签可见性。
//
// 2026-11-12 真机:rod 的 Element.Click → WaitInteractable → ScrollIntoView →
// WaitStableRAF 在 hidden 标签上永久挂住(rAF 被 Chrome 暂停),一直耗到 25s
// 会话上限。copyToCred 已用坐标点击绕过,这里把同一模式抽成声明式 Op,
// 供企微创建表单在后台标签上执行。只改新增 kind,不影响既有 click 链路。
func (bx *BrowserExecutor) clickByCoords(op Op) error {
	if op.Index < 0 {
		return fmt.Errorf("clickAt 的 Index 不能为负数(%d)", op.Index)
	}
	return bx.safe(func() {
		// 2026-11-12 真机:创建页停在 hidden 标签时,坐标/落点/disabled 全部
		// 正确(elementFromPoint 命中按钮内部),但 CDP 鼠标事件不触发 React 的
		// onClick,「保存」静默无效;Activate 后 visibilityState=visible +
		// hasFocus=true 即可正常提交。激活是只读 UI 操作,不改变页面内容。
		_, _ = bx.page.Activate()
		geo, err := bx.page.Timeout(browserOpTimeout).Eval(clickAtGeoJS,
			map[string]any{"sel": op.Target, "match": op.Match, "index": op.Index})
		if err != nil {
			panic(err)
		}
		raw := geo.Value.Str()
		if strings.HasPrefix(raw, "not-found:") {
			// 幂等:某些一次性动作(如「点击获取」生成 Secret)完成后按钮会
			// 从 DOM 消失,重跑同一套 Ops 时不应报错,更不该当成 selector_miss。
			if op.Optional {
				return
			}
			panic(fmt.Errorf("clickAt 未找到可点击元素(%s / %s)", op.Target, op.Match))
		}
		var pt struct {
			X, Y, W, H float64
			InViewport bool
		}
		if err := json.Unmarshal([]byte(raw), &pt); err != nil {
			panic(fmt.Errorf("解析 clickAt 坐标失败: %w (%s)", err, raw))
		}
		if pt.W <= 0 || pt.H <= 0 || !pt.InViewport {
			panic(fmt.Errorf("clickAt 元素不在可点击视口(w=%.0f h=%.0f inViewport=%v),不盲点", pt.W, pt.H, pt.InViewport))
		}
		if err := bx.page.Mouse.MoveTo(proto.Point{X: pt.X, Y: pt.Y}); err != nil {
			panic(err)
		}
		if err := bx.page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
			panic(err)
		}
	})
}

// fillByJS 纯 JS 填值 + 读回校验,避免 MustInput 的 WaitInteractable 在 hidden
// 标签上挂死;读回不一致立即报错,绝不带着空名称去提交创建表单。
func (bx *BrowserExecutor) fillByJS(op Op, creds map[string]string) error {
	text := op.Text
	if v, ok := strings.CutPrefix(text, "$env:"); ok {
		text = creds[v]
	}
	return bx.safe(func() {
		res, err := bx.page.Timeout(browserOpTimeout).Eval(fillByJSJS,
			map[string]string{"sel": op.Target, "val": text})
		if err != nil {
			panic(err)
		}
		if res.Value.Str() != text {
			panic(fmt.Errorf("fillJS 读回不一致: 期望 %q,实际 %q", text, res.Value.Str()))
		}
	})
}

func (bx *BrowserExecutor) copyToCred(op Op) error {
	if op.ExtractKey == "" {
		return fmt.Errorf("copyToCred 需要 ExtractKey")
	}
	if op.Match == "" {
		return fmt.Errorf("copyToCred 需要 Match(凭证块文本正则)")
	}
	rowSel := op.Target
	if rowSel == "" {
		rowSel = "td, .app-info-item, li, div"
	}
	btnSel := op.Text
	if btnSel == "" {
		btnSel = `[role="button"], button`
	}
	return bx.safe(func() {
		// 1) 注入 hook,必须在点击之前
		if _, err := bx.page.Timeout(browserOpTimeout).Eval(copyHookJS); err != nil {
			panic(fmt.Errorf("注入剪贴板 hook 失败: %w", err))
		}
		// 2) 定位目标凭证块里最小的、含复制按钮的容器,并给按钮打标记
		res, err := bx.page.Timeout(browserOpTimeout).Eval(locateCopyButtonJS, map[string]string{
			"rowSel": rowSel, "match": op.Match, "btnSel": btnSel,
		})
		if err != nil {
			panic(fmt.Errorf("定位复制按钮失败: %w", err))
		}
		if res.Value.Str() != "ok" {
			panic(fmt.Errorf("未找到文本匹配 /%s/ 且带复制按钮的凭证块(页面结构已变?)", op.Match))
		}
		// 3) 真实鼠标点击:React 的 onClick 需要 trusted 事件,不能用 JS el.click()。
		//    2026-11-12 真机分步诊断:el.Click() 内部走 WaitInteractable →
		//    ScrollIntoView → WaitStableRAF,后台/hidden 标签的 rAF 被 Chrome
		//    暂停时会永久挂住,直到会话 ctx(25s)到期,对外是 capture_failed。
		//    改为与 copyInFrame 一致:读元素几何后用 CDP 原生鼠标事件按坐标点击,
		//    不依赖 rAF,也不要求窗口处于前台。
		geo, err := bx.page.Timeout(browserOpTimeout).Eval(`() => {
			const el = document.querySelector('[data-hub-copy="1"]');
			if (!el) return 'not-found';
			const r = el.getBoundingClientRect();
			return JSON.stringify({x: r.left + r.width/2, y: r.top + r.height/2,
				w: r.width, h: r.height,
				inViewport: r.top >= 0 && r.top < innerHeight && r.left >= 0 && r.left < innerWidth});
		}`)
		if err != nil {
			panic(err)
		}
		if geo.Value.Str() == "not-found" {
			panic("定位后复制按钮消失,不盲点")
		}
		var pt struct {
			X, Y, W, H float64
			InViewport bool
		}
		if err := json.Unmarshal([]byte(geo.Value.Str()), &pt); err != nil {
			panic(fmt.Errorf("解析复制按钮坐标失败: %w", err))
		}
		if pt.W <= 0 || pt.H <= 0 || !pt.InViewport {
			panic(fmt.Errorf("复制按钮不在可点击视口内(w=%.0f h=%.0f inViewport=%v),不盲点", pt.W, pt.H, pt.InViewport))
		}
		if err := bx.page.Mouse.MoveTo(proto.Point{X: pt.X, Y: pt.Y}); err != nil {
			panic(err)
		}
		if err := bx.page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
			panic(err)
		}
		// 4) 轮询读 hook 捕获值(复制可能是异步的)
		val := ""
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			r, e := bx.page.Timeout(browserOpTimeout).Eval(`() => {
				const a = window.__hubCopyCaptured || [];
				return a.length ? a[a.length - 1] : '';
			}`)
			if e == nil {
				val = strings.TrimSpace(r.Value.Str())
			}
			if val != "" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		// 5) 降级:hook 未捕获 → 授权后读系统剪贴板(仅此路径会碰系统剪贴板)
		if val == "" {
			_ = proto.BrowserGrantPermissions{
				Permissions: []proto.BrowserPermissionType{
					proto.BrowserPermissionTypeClipboardReadWrite,
					proto.BrowserPermissionTypeClipboardSanitizedWrite,
				},
			}.Call(bx.browser)
			if r, e := bx.page.Timeout(browserOpTimeout).Eval(`() => navigator.clipboard.readText()`); e == nil {
				val = strings.TrimSpace(r.Value.Str())
			}
		}
		if val == "" {
			panic("复制按钮已点击但未读到剪贴板内容;不猜值、不重试,按 selector_miss 降级")
		}
		bx.Collected[op.ExtractKey] = val
	})
}

// copyHookJS 在页面上下文拦截复制动作的写入值。覆盖两类实现:现代
// navigator.clipboard.writeText 与旧式 document.execCommand('copy')。
const copyHookJS = `() => {
	if (window.__hubCopyHook) return 'already';
	window.__hubCopyCaptured = [];
	window.__hubCopyHook = true;
	try {
		const clip = navigator.clipboard;
		if (clip && typeof clip.writeText === 'function') {
			const orig = clip.writeText.bind(clip);
			clip.writeText = function (v) {
				try { window.__hubCopyCaptured.push(String(v)); } catch (e) {}
				return orig(v);
			};
		}
	} catch (e) {}
	try {
		const origExec = document.execCommand.bind(document);
		document.execCommand = function (cmd) {
			if (String(cmd).toLowerCase() === 'copy') {
				try {
					const sel = String(window.getSelection() || '');
					if (sel.trim()) window.__hubCopyCaptured.push(sel);
					else {
						const ae = document.activeElement;
						if (ae && (ae.tagName === 'TEXTAREA' || ae.tagName === 'INPUT') && ae.value)
							window.__hubCopyCaptured.push(ae.value);
					}
				} catch (e) {}
			}
			return origExec.apply(document, arguments);
		};
	} catch (e) {}
	return 'installed';
}`

// locateCopyButtonJS 找「文本命中 Match 且内部有复制按钮」的最小容器。
// 先严格按 aria-label=复制 找,再退到调用方给的 btnSel。
const locateCopyButtonJS = `({rowSel, match, btnSel}) => {
	const re = new RegExp(match, 'i');
	const cand = Array.from(document.querySelectorAll(rowSel))
		.filter(e => re.test(e.innerText || e.textContent || ''));
	cand.sort((a, b) => (a.innerText || '').length - (b.innerText || '').length);
	document.querySelectorAll('[data-hub-copy]').forEach(x => x.removeAttribute('data-hub-copy'));
	const sels = ['[aria-label="复制"], [aria-label="copy"]', btnSel];
	for (const sel of sels) {
		for (const c of cand) {
			const btn = c.querySelector(sel);
			if (!btn) continue;
			btn.setAttribute('data-hub-copy', '1');
			return 'ok';
		}
	}
	return 'not-found';
}`

// ---- 跨同源 iframe 的复制控件(copyInFrame) ----
//
// 背景(2026-11-12 真机实测):钉钉应用「机器人」tab 的内容并不渲染在 /fe/ai
// 外层 SPA,而是嵌在同源 iframe open-dev.dingtalk.com/fe/app?isHideOuterFrame=true
// 里;外层 body 只有左侧菜单。「复制 RobotCode」是
//
//	<p class="robotStatus___…"><span role="img">…</span>复制 RobotCode</p>
//
// 既不是 button 也没有 aria-label,所以 copyToCred 的定位器抓不到。
//
// 与 copyToCred 的差异(刻意做成新 Kind,不动已验证的 Client Secret 链路):
//   - hook 装到主 window + 所有同源 iframe 的 window(跨域 iframe 自动跳过);
//   - 在这些 document 里按「文本最短容器 + Match + btnSel」定位,支持容器自身
//     命中 btnSel(c.matches),以覆盖 <p> 这类文本型复制控件;
//   - 用 iframe 偏移换算出**视口绝对坐标**,通过 CDP 真实鼠标点击。
//     真机实测:JS el.click() 会让钉钉 iframe 里的 handler 卡住 CDP 会话
//     (25s 超时),真实鼠标点击稳定命中,也不伪造 trusted 事件;
//   - 读值仍走 hook 主路径,只有 hook 未捕获才降级读系统剪贴板。
const (
	frameCopyPrepareMax = 10 * time.Second // 轮询等 iframe 渲染的上限(须小于 browserSessionMax)
	frameCopyReadMax    = 2 * time.Second  // 点击后等 hook 捕获的上限
)

// frameCopyPrepareJS 一个 Eval 内完成「收集 window → 装 hook → 轮询定位按钮」。
// 每轮重新收集 iframe:SPA 切 tab 后 iframe 异步挂载、可能换 document,只在
// 首轮收集会漏掉。返回 JSON 字符串(rod 的 gson.JSON 没有 Array() 方法)。
const frameCopyPrepareJS = `async ({rowSel, match, btnSel}) => {
	const collect = () => {
		const docs = [{win: window, doc: document, frameEl: null, idx: -1}];
		const iframes = Array.from(document.querySelectorAll('iframe'));
		for (let i = 0; i < iframes.length; i++) {
			try {
				const f = iframes[i], w = f.contentWindow, d = f.contentDocument;
				if (w && d) docs.push({win: w, doc: d, frameEl: f, idx: i});
			} catch (e) {}
		}
		return docs;
	};
	const install = (docs) => {
		for (const o of docs) {
			try {
				const w = o.win;
				if (w.__hubCopyHook) continue;
				w.__hubCopyCaptured = [];
				w.__hubCopyHook = true;
				const clip = w.navigator.clipboard;
				if (clip && typeof clip.writeText === 'function') {
					const orig = clip.writeText.bind(clip);
					clip.writeText = function (v) {
						try { w.__hubCopyCaptured.push(String(v)); } catch (e) {}
						return orig(v);
					};
				}
				const origExec = w.document.execCommand.bind(w.document);
				w.document.execCommand = function (cmd) {
					if (String(cmd).toLowerCase() === 'copy') {
						try {
							const sel = String(w.getSelection() || '');
							if (sel.trim()) w.__hubCopyCaptured.push(sel);
							else {
								const ae = w.document.activeElement;
								if (ae && (ae.tagName === 'TEXTAREA' || ae.tagName === 'INPUT') && ae.value)
									w.__hubCopyCaptured.push(ae.value);
							}
						} catch (e) {}
					}
					return origExec.apply(w.document, arguments);
				};
			} catch (e) {}
		}
	};
	const re = new RegExp(match, 'i');
	const find = (docs) => {
		for (const o of docs) {
			try {
				const cand = Array.from(o.doc.querySelectorAll(rowSel))
					.filter(e => re.test(e.innerText || e.textContent || '') && (e.innerText || '').length < 120);
				cand.sort((a, b) => (a.innerText || '').length - (b.innerText || '').length);
				for (const c of cand) {
					let btn = null;
					try { if (c.matches && c.matches(btnSel)) btn = c; } catch (e) {}
					if (!btn) { try { btn = c.querySelector(btnSel); } catch (e) {} }
					if (!btn) continue;
					const r = btn.getBoundingClientRect();
					let ox = 0, oy = 0;
					if (o.frameEl) { const fr = o.frameEl.getBoundingClientRect(); ox = fr.left; oy = fr.top; }
					return {btn: btn, x: ox + r.left + r.width / 2, y: oy + r.top + r.height / 2, idx: o.idx};
				}
			} catch (e) {}
		}
		return null;
	};
	let docs = [], hit = null;
	for (let i = 0; i < 40; i++) {
		docs = collect();
		install(docs);
		hit = find(docs);
		if (hit) break;
		await new Promise(r => setTimeout(r, 200));
	}
	if (!hit) return JSON.stringify({found: false, docs: docs.length});
	hit.btn.setAttribute('data-hub-copy', '1');
	return JSON.stringify({found: true, x: hit.x, y: hit.y, idx: hit.idx, docs: docs.length});
}`

const frameCopyClearJS = `() => {
	const wins = [window];
	for (const f of document.querySelectorAll('iframe')) {
		try { if (f.contentWindow) wins.push(f.contentWindow); } catch (e) {}
	}
	for (const w of wins) { try { w.__hubCopyCaptured = []; } catch (e) {} }
	return 'ok';
}`

const frameCopyReadJS = `() => {
	const wins = [window];
	for (const f of document.querySelectorAll('iframe')) {
		try { if (f.contentWindow) wins.push(f.contentWindow); } catch (e) {}
	}
	let last = '';
	for (const w of wins) {
		try { const a = w.__hubCopyCaptured || []; if (a.length) last = a[a.length - 1]; } catch (e) {}
	}
	return String(last || '');
}`

// copyInFrame 点击同源 iframe 里的文本型「复制」控件,把明文存入
// bx.Collected[op.ExtractKey]。找不到/读不到都返回 selector_miss 让编排层降级,
// 绝不猜值、绝不重试提交。
func (bx *BrowserExecutor) copyInFrame(op Op) error {
	if op.ExtractKey == "" {
		return fmt.Errorf("copyInFrame 需要 ExtractKey")
	}
	if op.Match == "" {
		return fmt.Errorf("copyInFrame 需要 Match(复制控件文本正则)")
	}
	rowSel := op.Target
	if rowSel == "" {
		rowSel = "body *"
	}
	btnSel := op.Text
	if btnSel == "" {
		btnSel = `p, [role="img"], a, button, [role="button"]`
	}
	return bx.safe(func() {
		res, err := bx.page.Timeout(frameCopyPrepareMax).Eval(frameCopyPrepareJS, map[string]string{
			"rowSel": rowSel, "match": op.Match, "btnSel": btnSel,
		})
		if err != nil {
			panic(fmt.Errorf("跨 frame 定位复制控件失败: %w", err))
		}
		var prep struct {
			Found bool    `json:"found"`
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
			Idx   int     `json:"idx"`
			Docs  int     `json:"docs"`
		}
		if err := json.Unmarshal([]byte(res.Value.Str()), &prep); err != nil {
			panic(fmt.Errorf("解析跨 frame 定位结果失败: %w", err))
		}
		if !prep.Found {
			panic(fmt.Errorf("未找到文本匹配 /%s/ 的复制控件(iframe 未加载或页面结构已变?)", op.Match))
		}
		// 清掉上一次点击的残留捕获值,避免误把旧值写进本次结果。
		if _, err := bx.page.Timeout(browserOpTimeout).Eval(frameCopyClearJS); err != nil {
			panic(err)
		}
		// 真实鼠标点击:坐标已换算成主 frame 视口绝对坐标。
		if err := bx.page.Mouse.MoveTo(proto.Point{X: prep.X, Y: prep.Y}); err != nil {
			panic(err)
		}
		if err := bx.page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
			panic(err)
		}
		val := ""
		deadline := time.Now().Add(frameCopyReadMax)
		for time.Now().Before(deadline) {
			if r, e := bx.page.Timeout(browserOpTimeout).Eval(frameCopyReadJS); e == nil {
				val = strings.TrimSpace(r.Value.Str())
			}
			if val != "" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if val == "" {
			// 与 copyToCred 一致的降级:授权后读系统剪贴板(仅此路径会碰系统剪贴板)。
			_ = proto.BrowserGrantPermissions{
				Permissions: []proto.BrowserPermissionType{
					proto.BrowserPermissionTypeClipboardReadWrite,
					proto.BrowserPermissionTypeClipboardSanitizedWrite,
				},
			}.Call(bx.browser)
			if r, e := bx.page.Timeout(browserOpTimeout).Eval(`() => navigator.clipboard.readText()`); e == nil {
				val = strings.TrimSpace(r.Value.Str())
			}
		}
		if val == "" {
			panic("复制控件已点击但未读到剪贴板内容;不猜值、不重试,按 selector_miss 降级")
		}
		bx.Collected[op.ExtractKey] = val
	})
}
