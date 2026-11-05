package onboard

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-rod/rod"
)

// ErrSelectorMiss 浏览器执行器某步失败(选择器失效/页面结构变化/元素超时),
// 编排器收到后当场把该步骤降级为手动说明,流程不中断。
var ErrSelectorMiss = errors.New("浏览器步骤未命中(选择器失效或页面超时)")

// BrowserExecutor 直连用户已登录的 Chrome 调试端口执行清单步骤。
// 合规边界:只做表单代填与凭据抄写,绝不碰登录(扫码/短信/验证码永远人工)。
type BrowserExecutor struct {
	browser   *rod.Browser
	page      *rod.Page
	Collected map[string]string // readText 抽取的值(如 App ID),供向导回显确认
}

// NewBrowserExecutor 连接调试端口(默认 http://127.0.0.1:9222)。
// 失败即返回错误,编排器降级为纯手动模式。
func NewBrowserExecutor(debugURL string) (*BrowserExecutor, error) {
	if debugURL == "" {
		debugURL = "http://127.0.0.1:9222"
	}
	b := rod.New().ControlURL(debugURL)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("连接 Chrome 调试端口失败(需以 --remote-debugging-port=9222 启动并登录平台): %w", err)
	}
	bx := &BrowserExecutor{browser: b, Collected: map[string]string{}}
	// rod 以 Must* API 为主,统一 recover 转 ErrSelectorMiss
	var p *rod.Page
	if err := bx.safe(func() { p = b.MustPage("about:blank") }); err != nil {
		b.Close()
		return nil, err
	}
	bx.page = p
	return bx, nil
}

// Close 关闭页面与连接(不关浏览器本身,不碰用户其他标签页)。
func (bx *BrowserExecutor) Close() {
	if bx.page != nil {
		_ = bx.page.Close()
	}
	if bx.browser != nil {
		_ = bx.browser.Close()
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
		return bx.safe(func() { bx.page.MustNavigate(op.Target) })
	case "waitVisible":
		return bx.safe(func() { bx.page.MustElement(op.Target) })
	case "click":
		return bx.safe(func() { bx.page.MustElement(op.Target).MustClick() })
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
			text := strings.TrimSpace(bx.page.MustElement(op.Target).MustText())
			if op.ExtractKey != "" {
				bx.Collected[op.ExtractKey] = text
			}
		})
	case "screenshot": // 辅助人工核对当前页面状态
		return bx.safe(func() { bx.page.MustScreenshot(op.Target) })
	default:
		return fmt.Errorf("未知操作类型 %q", op.Kind)
	}
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
