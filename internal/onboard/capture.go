package onboard

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// CaptureResult 是 hub.onboard.capture 的返回:phase 明确区分"等待人工"与
// 真正失败,Agent 据此决定是继续轮询还是转述给用户,不需要用户敲任何命令。
type CaptureResult struct {
	Platform    string   `json:"platform"`
	Phase       string   `json:"phase"` // browser_missing / waiting_login / page_loading / waiting_app_detail / unsupported / capture_failed / incomplete / verify_failed / write_failed / reload_failed / done
	ChromeReady bool     `json:"chrome_ready"`
	LoginState  string   `json:"login_state"`
	Collected   []string `json:"collected"`
	Message     string   `json:"message,omitempty"`
	Next        string   `json:"next"`
}

// Capture 在 Hub 进程内完成"抓凭据 → 格式校验 → 真连校验 → 写 .env → 热重载"。
//
// 为什么不复用 Run():向导 Run() 全程 fmt.Printf 到 stdout(stdio 模式下是 MCP
// 协议通道,会打脏协议帧),且含 prompt()/readHidden() 等阻塞读终端的逻辑。
// 这里只执行凭据步骤里纯浏览器的 Ops,零终端交互、零 stdout 输出。
//
// 合规边界:只读取页面上已展示的 App ID / Secret,绝不代做登录或表单提交。
func Capture(platform string, reload func(map[string]string) error) (*CaptureResult, error) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}
	res := &CaptureResult{Platform: platform, Collected: []string{}}

	w, err := Watch(platform)
	if err != nil {
		return nil, err
	}
	res.ChromeReady = w.ChromeReady
	res.LoginState = w.LoginState

	switch {
	case !w.ChromeReady:
		res.Phase = "browser_missing"
		res.Next = "调试端点暂时不可达;先重试 hub.onboard.watch,仍失败再调用 hub.onboard.start"
		return res, nil
	case w.LoginState == "waiting_login":
		res.Phase = "waiting_login"
		res.Next = "请用户完成扫码登录;继续轮询 hub.onboard.watch,变成 logged_in 后重新调用 hub.onboard.capture"
		return res, nil
	case w.LoginState == "unknown":
		res.Phase = "page_loading"
		res.Next = "浏览器页面尚未稳定(未识别到登录页或开放平台页);继续轮询 hub.onboard.watch,状态明确后再重试 hub.onboard.capture"
		return res, nil
	case !w.InAppDetail:
		res.Phase = "waiting_app_detail"
		res.Next = "请提示用户在浏览器里创建应用并进入「凭证与基础信息」页(URL 形如 " + p.URL + "/<appId>/...);完成后重新调用 hub.onboard.capture"
		return res, nil
	}

	act := credAction(p)
	if act == nil {
		res.Phase = "unsupported"
		res.Message = "该平台未定义浏览器凭据抓取动作"
		res.Next = "该平台仍需在终端执行 go run ./cmd/hub -config config.json -onboard " + platform
		return res, nil
	}

	bx, err := NewBrowserExecutor("")
	if err != nil {
		res.Phase = "browser_missing"
		res.Message = err.Error()
		res.Next = "确认 Chrome 以持久化 profile + 9222 调试端口启动(可重跑 hub.onboard.start)"
		return res, nil
	}
	defer bx.Close()

	// 把执行器切到应用详情页标签。Close() 只关 NewBrowserExecutor 自己开的
	// about:blank,不会碰用户的页面。
	located := false
	err = bx.safe(func() {
		for _, pg := range bx.browser.MustPages() {
			info, e := pg.Info()
			if e != nil || info == nil {
				continue
			}
			if matchURLPrefix(info.URL, p.URL, []string{"/app/list", "/app/create", "/app/search", "/login"}) {
				bx.page = pg
				located = true
				return
			}
		}
	})
	if err != nil || !located {
		res.Phase = "waiting_app_detail"
		res.Message = "未能在调试 Chrome 中定位到应用详情页标签"
		res.Next = "请在浏览器里打开应用的「凭证与基础信息」页后重试 hub.onboard.capture"
		return res, nil
	}

	// 用户已经停在详情页了,只执行读取凭据的 Ops(见 captureOps)
	env := map[string]string{}
	if err := bx.RunAction(&BrowserAction{Script: act.Script, Ops: captureOps(act)}, env); err != nil {
		res.Phase = "capture_failed"
		res.Message = err.Error()
		res.Next = "请确认停留在「凭证与基础信息」页,App ID 与 Secret 均可读;然后重试 hub.onboard.capture"
		return res, nil
	}
	for k, v := range bx.Collected {
		env[k] = strings.TrimSpace(v)
	}

	// 必需凭据齐全 + 格式校验通过才允许写盘
	for _, c := range PortalCreds[platform] {
		v := strings.TrimSpace(env[c.EnvKey])
		if v == "" {
			res.Phase = "incomplete"
			res.Message = "未能读取 " + c.EnvKey
			res.Next = "请在详情页展开该凭据后重试 hub.onboard.capture"
			return res, nil
		}
		if c.Format != nil {
			if err := c.Format(v); err != nil {
				res.Phase = "incomplete"
				res.Message = c.EnvKey + " 格式不符: " + err.Error()
				res.Next = "页面读取到的值疑似掩码或占位文本,请刷新详情页后重试 hub.onboard.capture"
				return res, nil
			}
		}
		env[c.EnvKey] = v
		res.Collected = append(res.Collected, c.EnvKey)
	}
	sort.Strings(res.Collected)

	if err := p.Validate(env); err != nil {
		res.Phase = "verify_failed"
		res.Message = err.Error()
		res.Next = "凭据被平台拒绝或平台不可达,未写入 .env;请核对后重试 hub.onboard.capture"
		return res, nil
	}

	if err := UpsertEnv(".env", env); err != nil {
		res.Phase = "write_failed"
		res.Message = err.Error()
		return res, nil
	}
	for k, v := range env {
		os.Setenv(k, v) // 让本进程后续逻辑与热重载直接用新值
	}
	if reload != nil {
		if err := reload(env); err != nil {
			res.Phase = "reload_failed"
			res.Message = err.Error()
			res.Next = "凭据已写入 .env,但热重载失败;在连接器页面禁用/启用 mcp-hub 即可生效"
			return res, nil
		}
	}
	res.Phase = "done"
	res.Next = "凭据已写入 .env 并完成热重载;请确认已在开放平台开通所需权限并发布应用,之后即可调用 table.query 等能力"
	return res, nil
}

// captureOps 过滤掉会导走页面/长时间等待/落盘的 Ops。
// Capture 的前置条件已保证用户停留在应用详情页,此时再 navigate 会把页面
// 导回列表,waitURL 会白等 60s,screenshot 会往工作目录写文件——都不该执行。
func captureOps(act *BrowserAction) []Op {
	ops := make([]Op, 0, len(act.Ops))
	for _, op := range act.Ops {
		switch op.Kind {
		case "navigate", "waitURL", "screenshot":
			continue
		}
		ops = append(ops, op)
	}
	return ops
}

// credAction 取清单中「收集凭据」步骤的浏览器动作定义。
func credAction(p *Portal) *BrowserAction {
	for i := range p.Steps {
		if p.Steps[i].Cred && p.Steps[i].Browser != nil {
			return p.Steps[i].Browser
		}
	}
	return nil
}
