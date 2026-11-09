package onboard

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// hub.onboard.create_app 的后端实现:Agent 代用户创建企业自建应用并自动收尾。
//
// 合规边界:登录(扫码/验证码)与管理员审批永远人工;创建应用属于「以用户身份
// 执行写操作」,因此必须满足两道门才允许提交:
//   1) 幂等门 —— 已有应用 / 用户已停在应用详情页时一律不创建,直接复用;
//   2) 确认门 —— confirmed=false 只返回 need_confirmation,不碰任何按钮。
// 表单选择器未命中时返回 selector_miss 并附上页面候选元素,降级人工,绝不静默重试提交。
//
// 选择器策略见 portal.go 的 create-app 步骤:按钮按可见文本匹配、输入框用 CSS
// 多候选。飞书开放平台是 React SPA,class 随改版变化,不绑定单一选择器。

const (
	defaultAppName      = "mcp-hub"
	createAppURLWaitMax = 15 * time.Second
	createAppPollStep   = 500 * time.Millisecond
)

// reAppPath 从详情页 URL 抽取应用标识(/app/<appId>/...)。飞书是 cli_ 前缀,
// 其余平台允许字母数字与下划线连字符;用于幂等检查与创建结果校验。
var reAppPath = regexp.MustCompile(`/app/([A-Za-z0-9_-]{6,})`)

// appPathExcludes 是开放平台的功能页路径,不是应用 ID。列表页上「创建应用」
// 之类的链接常写成 /app/create,若当成已有应用会让幂等门误判、拒绝真正创建。
var appPathExcludes = map[string]bool{
	"list": true, "create": true, "search": true, "new": true,
	"detail": true, "category": true, "market": true,
}

// AppRef 描述一个已存在的应用。
type AppRef struct {
	AppID string `json:"app_id,omitempty"`
	Name  string `json:"name,omitempty"`
	URL   string `json:"url"`
}

// CreateAppResult 是 hub.onboard.create_app 的返回。
type CreateAppResult struct {
	Platform     string         `json:"platform"`
	Phase        string         `json:"phase"` // need_confirmation / existing_apps / already_in_detail / browser_missing / waiting_login / page_loading / unsupported / selector_miss / create_failed / done
	AppName      string         `json:"app_name"`
	ExistingApps []AppRef       `json:"existing_apps"`
	Candidates   []string       `json:"candidates,omitempty"`
	CreatedURL   string         `json:"created_url,omitempty"`
	Capture      *CaptureResult `json:"capture,omitempty"`
	Message      string         `json:"message,omitempty"`
	Next         string         `json:"next"`
}

// CreateApp 检查前置条件、代填并提交创建应用表单,随后自动调用 Capture 完成接入。
// reload 由调用方(main.go)注入,与终端向导/ capture 共用同一热重载路径。
func CreateApp(platform, appName string, confirmed bool, reload func(map[string]string) error) (*CreateAppResult, error) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}
	if strings.TrimSpace(appName) == "" {
		appName = defaultAppName
	}
	res := &CreateAppResult{
		Platform:     platform,
		AppName:      appName,
		ExistingApps: []AppRef{},
	}

	act := createAppAction(p)
	if act == nil {
		res.Phase = "unsupported"
		res.Next = "该平台未定义自动创建应用的浏览器动作;请在浏览器里人工创建应用并进入「凭证与基础信息」页,之后调用 hub.onboard.capture"
		return res, nil
	}

	w, err := Watch(platform)
	if err != nil {
		return nil, err
	}
	switch {
	case !w.ChromeReady:
		res.Phase = "browser_missing"
		res.Next = "调试 Chrome 未就绪;先重试 hub.onboard.watch 2~3 次,仍失败再调用 hub.onboard.start"
		return res, nil
	case w.LoginState == "waiting_login":
		res.Phase = "waiting_login"
		res.Next = "尚未登录,登录必须人工完成;扫码后继续轮询 hub.onboard.watch,变成 logged_in 再重试 hub.onboard.create_app"
		return res, nil
	case w.LoginState == "unknown":
		res.Phase = "page_loading"
		res.Next = "页面尚未稳定;继续轮询 hub.onboard.watch,状态明确后重试 hub.onboard.create_app"
		return res, nil
	case w.InAppDetail:
		// 幂等门 1:用户已经停在某个应用的详情页了,再建一个纯属浪费。
		// 直接帮他收尾,这正是「人已登录并点进凭证页」场景下最该做的事。
		res.Phase = "already_in_detail"
		res.ExistingApps = []AppRef{{AppID: appIDFromURL(w.DetailURL), URL: w.DetailURL}}
		res.Capture, err = Capture(platform, reload)
		if err != nil {
			return nil, err
		}
		res.Next = "已检测到你停在上一个应用的详情页,为避免重复创建未新建应用;已自动尝试抓取凭据。" +
			"若 capture.phase=done 则接入完成,否则按 capture.next 继续"
		return res, nil
	}

	// 幂等门 2:列表页已经有应用卡片 → 复用,不新建。
	apps, err := listExistingApps(p)
	if err != nil {
		res.Phase = "browser_missing"
		res.Message = err.Error()
		res.Next = "读取应用列表失败;确认调试 Chrome 仍在运行后重试"
		return res, nil
	}
	if len(apps) > 0 {
		res.Phase = "existing_apps"
		res.ExistingApps = apps
		res.Next = "检测到已有应用,为避免在企业里重复创建不新建;" +
			"调用 hub.onboard.setup 会自动打开该应用详情页并完成抓凭据与后续配置(无需用户点击、无需登录、无需终端命令);" +
			"只需抓凭据时才让用户点进「凭证与基础信息」页再调 hub.onboard.capture"
		return res, nil
	}

	// 确认门:不显式确认就不碰任何按钮。
	if !confirmed {
		res.Phase = "need_confirmation"
		res.Next = "创建应用会在企业里真实产生一个应用且不可撤销。请用选择框让用户确认应用名(默认 \"" +
			appName + "\"),确认后带 confirmed=true 重新调用 hub.onboard.create_app。" +
			"图标沿用平台默认;权限开通与版本发布仍需人工"
		return res, nil
	}

	before := pageSet()
	bx, err := NewBrowserExecutor("")
	if err != nil {
		res.Phase = "browser_missing"
		res.Message = err.Error()
		res.Next = "确认 Chrome 以持久化 profile + 9222 调试端口启动(可重跑 hub.onboard.start)"
		return res, nil
	}
	defer bx.Close()

	if err := bx.RunAction(&BrowserAction{Script: act.Script, Ops: createAppOps(act, appName)}, nil); err != nil {
		res.Phase = "selector_miss"
		res.Message = err.Error()
		res.Candidates = collectCandidates(bx)
		res.Next = "创建表单的选择器未命中(飞书改版?),已降级为人工,**不要再自动重试提交**。" +
			"请把 candidates 转述给用户,让他在浏览器里手动创建应用并进入「凭证与基础信息」页,之后调用 hub.onboard.capture"
		return res, nil
	}

	// 提交后必须验证真的产生了新应用详情页,不能只凭「点到了按钮」就算成功。
	created := waitForNewDetail(p, before)
	if created == "" {
		res.Phase = "create_failed"
		res.Message = "提交后 " + createAppURLWaitMax.String() + " 内未出现新的应用详情页"
		res.Candidates = collectCandidates(bx)
		res.Next = "可能触发了平台风控/二次确认,或表单未通过校验;请让用户在浏览器里核对并手动完成创建," +
			"然后调用 hub.onboard.capture。不要重复提交创建表单"
		return res, nil
	}
	res.CreatedURL = created
	res.ExistingApps = []AppRef{{AppID: appIDFromURL(created), URL: created}}

	// 新应用已落到详情页,直接走既有收尾链路:抓凭据 → 校验 → 写 .env → 热重载。
	res.Capture, err = Capture(platform, reload)
	if err != nil {
		return nil, err
	}
	if res.Capture.Phase != "done" {
		res.Phase = "create_failed"
		res.Message = "应用已创建,但自动收尾未完成(capture.phase=" + res.Capture.Phase + ")"
		res.Next = "按 capture.next 继续;权限开通与版本发布可调用 hub.onboard.setup 自动完成,无需用户手动操作"
		return res, nil
	}
	res.Phase = "done"
	res.Next = "应用已创建且凭据已写入 .env 并完成热重载;请调 hub.health 确认 credentials=true / mounted=true," +
		"权限开通与版本发布可继续调用 hub.onboard.setup 自动完成"
	return res, nil
}

// createAppAction 取清单中「创建应用」步骤的浏览器动作定义。
func createAppAction(p *Portal) *BrowserAction {
	for i := range p.Steps {
		if p.Steps[i].ID == "create-app" && p.Steps[i].Browser != nil {
			return p.Steps[i].Browser
		}
	}
	return nil
}

// createAppOps 把声明式动作转成可执行的序列:
//   - 替换 $app_name 为确认后的应用名
//   - 去掉 waitURL(默认会白等 60s)与 screenshot(往工作目录落盘),
//     跳转改由 waitForNewDetail 用短超时验证
func createAppOps(act *BrowserAction, appName string) []Op {
	ops := make([]Op, 0, len(act.Ops))
	for _, op := range act.Ops {
		if op.Kind == "waitURL" || op.Kind == "screenshot" {
			continue
		}
		if op.Kind == "fill" {
			op.Text = strings.ReplaceAll(op.Text, "$app_name", appName)
		}
		ops = append(ops, op)
	}
	return ops
}

// listExistingApps 只读地收集列表页上已存在的应用链接,用于幂等判断。
// 依据是详情页 URL 中的 /app/<appId> 模式(与 watch.classifyPages 同源),
// 不依赖任何易变的 class。
func listExistingApps(p *Portal) ([]AppRef, error) {
	bx, err := NewBrowserExecutor("")
	if err != nil {
		return nil, err
	}
	defer bx.Close()

	var out []AppRef
	seen := map[string]bool{}
	add := func(raw, name string) {
		id := appIDFromURL(raw)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, AppRef{AppID: id, Name: strings.TrimSpace(name), URL: raw})
	}

	err = bx.safe(func() {
		for _, pg := range bx.browser.MustPages() {
			info, e := pg.Info()
			if e != nil || info == nil {
				continue
			}
			// 已打开的详情页本身就是「已有应用」的铁证
			if matchURLPrefix(info.URL, p.URL, []string{"/app/list", "/app/create", "/app/search", "/login"}) {
				add(info.URL, "")
			}
			if hostOf(info.URL) != hostOf(p.URL) {
				continue
			}
			els, e := pg.Elements("a[href]")
			if e != nil {
				continue
			}
			for _, el := range els {
				href := el.MustAttribute("href")
				if href == nil || *href == "" {
					continue
				}
				if appIDFromURL(*href) == "" {
					continue
				}
				add(*href, el.MustText())
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// collectCandidates 失败时 dump 页面上可见的按钮文本与输入框 placeholder,
// 供 Agent 转述、也便于日后校准选择器。只读,不改 DOM。
func collectCandidates(bx *BrowserExecutor) []string {
	var out []string
	_ = bx.safe(func() {
		if els, e := bx.page.Elements("button, a, [role=button]"); e == nil {
			for i, el := range els {
				if i >= 20 {
					break
				}
				t := strings.TrimSpace(el.MustText())
				if t != "" && len(t) <= 40 {
					out = append(out, "button: "+t)
				}
			}
		}
		if ins, e := bx.page.Elements("input, textarea"); e == nil {
			for i, el := range ins {
				if i >= 12 {
					break
				}
				if a := el.MustAttribute("placeholder"); a != nil && *a != "" {
					out = append(out, "input[placeholder]: "+*a)
				}
			}
		}
	})
	return out
}

// pageSet 记录调用前的标签 URL 集合,用于识别「创建后新出现的详情页」。
func pageSet() map[string]bool {
	out := map[string]bool{}
	pages, err := debugPages()
	if err != nil {
		return out
	}
	for _, u := range pages {
		out[u] = true
	}
	return out
}

func waitForNewDetail(p *Portal, before map[string]bool) string {
	deadline := time.Now().Add(createAppURLWaitMax)
	for time.Now().Before(deadline) {
		pages, err := debugPages()
		if err == nil {
			for _, u := range pages {
				if !before[u] && matchURLPrefix(u, p.URL, []string{"/app/list", "/app/create", "/app/search", "/login"}) {
					return u
				}
			}
		}
		time.Sleep(createAppPollStep)
	}
	return ""
}

func appIDFromURL(raw string) string {
	m := reAppPath.FindStringSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	id := m[1]
	if appPathExcludes[strings.ToLower(id)] {
		return ""
	}
	return id
}
