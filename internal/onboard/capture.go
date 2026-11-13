package onboard

import (
	"fmt"
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
	BotName     string   `json:"bot_name,omitempty"`
	Message     string   `json:"message,omitempty"`
	Next        string   `json:"next"`
}

// Capture 在 Hub 进程内完成"抓凭据 → 格式校验 → 真连校验 → 写 .env → 热重载"。
//
// 为什么不复用 Run():向导 Run() 全程 fmt.Printf 到 stdout(stdio 模式下是 MCP
// 协议通道,会打脏协议帧),且含 prompt()/readHidden() 等阻塞读终端的逻辑。
// 这里只执行凭据步骤里纯浏览器的 Ops,零终端交互、零 stdout 输出。
//
// 合规边界:只读取页面上已展示的凭据、点击「复制」图标读回明文,绝不代做登录或表单提交。
func Capture(platform string, reload func(map[string]string) error) (*CaptureResult, error) {
	return CaptureNamed(platform, "", reload)
}

// CaptureNamed 要求企微多机器人必须显式给 botName;单机器人兼容旧 Capture。
func CaptureNamed(platform, botName string, reload func(map[string]string) error) (*CaptureResult, error) {
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
	case platform == "wecom" && botName == "" && w.LoginState != "logged_in":
		// 企微登录过期时旧详情 DOM 可能仍可读,绝不能拿它冒充已核验的唯一机器人。
		res.Phase = "need_bot_selection"
		res.Next = "企微登录态过期或无法确认,不能仅凭残留详情页自动选择机器人;请重新登录并从列表明确选择后带 app_name 重试;本次未读取任何凭据"
		return res, nil
	case !w.InAppDetail && !(platform == "wecom" && botName != ""):
		res.Phase = "waiting_app_detail"
		res.Next = "调用 hub.onboard.setup 可自动打开/创建应用详情页并完成后续配置;" +
			"若只需抓凭据,请让用户在浏览器里打开「凭证与基础信息」页(URL 形如 " + p.URL + "/<appId>/...)后重试 hub.onboard.capture"
		return res, nil
	case !w.InAppDetail && platform == "wecom" && botName != "":
		// Capture 也可独立调用:从列表进入用户明确选择的机器人,不靠残留详情页。
		detail, step := openAppForSetup(p, w, botName)
		if step.Status != "done" && step.Status != "already_open" {
			res.Phase = step.Status
			res.Message = step.Message
			res.Next = "请从候选机器人中明确选择;本次未读取任何凭据"
			return res, nil
		}
		w.DetailURL = detail
	}

	if platform == "wecom" {
		_, identity := wecomOpenDetail(p, w.DetailURL, botName)
		if identity.Status != "already_open" {
			res.Phase = identity.Status
			res.Message = identity.Message
			res.Next = "多机器人时必须由用户明确选择;本次未读取任何凭据"
			return res, nil
		}
	}

	act := credAction(p)
	if act == nil {
		res.Phase = "unsupported"
		res.Message = "该平台未定义(或未校准)浏览器凭据抓取动作"
		res.Next = unsupportedCaptureNext()
		return res, nil
	}

	bx, err := NewBrowserExecutorForPlatform("", p)
	if err != nil {
		res.Phase = "browser_missing"
		res.Message = err.Error()
		res.Next = "先调用 hub.onboard.start(" + platform + ") 打开目标平台页面,再重试;不要在其他平台标签上读取凭据"
		return res, nil
	}
	defer bx.Close()

	// 把执行器切到应用详情页标签。Close() 只关 NewBrowserExecutor 自己开的
	// about:blank,不会碰用户的页面。
	located := false
	if platform == "wecom" {
		if page := findWeComDetailPage(bx, w.DetailURL); page != nil {
			bx.page = page
			located = true
		}
	} else {
		err = bx.safe(func() {
			for _, pg := range bx.browser.MustPages() {
				info, e := pg.Info()
				if e != nil || info == nil {
					continue
				}
				if portalDetailMatch(p, info.URL) {
					bx.page = pg
					located = true
					return
				}
			}
		})
	}
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
		res.Next = captureFailedNext(platform)
		return res, nil
	}
	for k, v := range bx.Collected {
		env[k] = strings.TrimSpace(v)
	}

	// 缺值/格式校验 → 真连校验 → 写 .env → 热重载,与 set_credentials 共用
	// 同一份收尾逻辑,避免两条接入路径的校验规则漂移。
	collected, ce := commitCredentials(platform, env, CredOptions{
		EnvPath: ".env", ConfigPath: "config.json", Validate: p.Validate, Reload: reload, BotName: botName,
	})
	if ce != nil {
		res.Phase = capturePhase(ce.Phase)
		res.Message = ce.Message
		// 只回传「哪些字段已从页面成功读到」的键名,绝不回传值。
		// 钉钉 /baseinfo 的 Client ID 是明文、Client Secret 由 copyToCred 点复制
		// 图标读明文,/bot 的 ROBOT_CODE 由 copyInFrame 跨同源 iframe 点复制读明文;
		// 读到掩码或不合规值时格式校验拒绝,让 Agent 明确知道还差哪一项。
		for _, c := range PortalCreds[platform] {
			v := strings.TrimSpace(env[c.EnvKey])
			if v != "" && (c.Format == nil || c.Format(v) == nil) {
				res.Collected = append(res.Collected, c.EnvKey)
			}
		}
		res.Next = captureNext(res.Phase, platform)
		return res, nil
	}
	res.Collected = collected
	res.BotName = botName
	res.Phase = "done"
	res.Next = "凭据已写入并完成本次接入回调;调用 hub.health 确认宿主已挂载,之后可调用 table.query 等能力"
	if platform == "wecom" && botName != "" {
		res.Next = "机器人「" + botName + "」的凭据已写入 .env,config.json 的 wecom.bots 仅保存变量引用;其他机器人配置未修改"
	}
	return res, nil
}

// captureFailedNext 给 capture_failed 阶段按平台定制的降级指引。
func captureFailedNext(platform string) string {
	switch platform {
	case "wecom":
		return "请确认停留在企微机器人详情页(/aiHelper/detail?aibotid=...),Bot ID 与 Secret 行右侧的复制图标均可读;" +
			"然后重试 hub.onboard.capture;若复制图标定位持续失效,改用 hub.onboard.set_credentials 补齐 WECOM_BOT_ID / WECOM_BOT_SECRET"
	case "dingtalk":
		return "请确认停留在应用详情页、「凭证与基础信息」的 Client ID 明文与 Client Secret 复制图标、" +
			"以及「机器人」tab 的「复制 RobotCode」均可读(机器人内容在同源 iframe 里,加载稍慢);" +
			"然后重试 hub.onboard.capture;若 iframe 或复制按钮定位持续失效,改用 hub.onboard.set_credentials 一次性补齐三项"
	default:
		return "请确认停留在应用详情页、凭据字段与复制图标均可读,然后重试 hub.onboard.capture;" +
			"若定位持续失效,改用 hub.onboard.set_credentials 补齐凭据"
	}
}

// unsupportedCaptureNext 是浏览器抓取动作缺失时唯一的降级指引。
// 旧版这里让用户「去终端跑 -onboard」,在 stdio 宿主下是死路:该向导需要 TTY。
func unsupportedCaptureNext() string {
	return "不要让用户切到终端。请调用 hub.onboard.status 取 platforms[].credential_fields," +
		"用表单收集用户从平台后台复制的凭据,再调用 hub.onboard.set_credentials 完成格式校验→真连校验→写 .env→热重载(四平台通用、零 DOM 依赖)"
}

// capturePhase 把公共收尾的阶段映射到 CaptureResult 的历史 phase 枚举,
// 保持 Agent 已有的分支处理逻辑不变。
func capturePhase(phase string) string {
	switch phase {
	case "invalid_format":
		return "incomplete"
	default:
		return phase
	}
}

// captureNext 的 platform 为可选参数:历史调用点只传 phase,新增的
// 钉钉专属降级说明通过第二个参数启用。
func captureNext(phase string, platform ...string) string {
	p := ""
	if len(platform) > 0 {
		p = platform[0]
	}
	switch phase {
	case "incomplete":
		if p == "dingtalk" {
			return "钉钉接入需要三项:DINGTALK_Client_ID(凭证页明文)、DINGTALK_Client_Secret(凭证页渲染成掩码," +
				"复制图标给出完整明文)、ROBOT_CODE(「机器人」tab 的「复制 RobotCode」,内容在同源 iframe)。" +
				"collect-creds 已自动点这两处复制按钮读明文;本次仍缺值、读到掩码或格式不符,已拒绝写入 .env" +
				"(见 collected 判断已拿到哪几项)。请用表单只向用户索取缺失项,把三项一起交给 hub.onboard.set_credentials" +
				"完成格式校验→真连校验→写 .env→热重载;不要在回复里回显任何凭据值"
		}
		return "页面读取到的值缺失、疑似掩码或格式不符;" +
			"或改用 hub.onboard.set_credentials 由 Agent 收集凭据(零 DOM 依赖,四平台通用)"
	case "verify_failed":
		return "凭据被平台拒绝或平台不可达,未写入 .env;核对后重试 hub.onboard.capture,或改用 hub.onboard.set_credentials"
	case "write_failed":
		return "写入或校验本地凭据配置失败;请按 message 修正文件权限、既有配置引用或环境变量后重试"
	case "reload_failed":
		return "凭据已写入,但本次接入回调失败;在连接器页面禁用/启用 mcp-hub 后重试"
	default:
		return "按 message 修正后重试,或改用 hub.onboard.set_credentials"
	}
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
		// 列表页专用步骤(如等 app-card、等用户点开应用)在详情页必然等不到,
		// 旧版把它们留在这里,配合无超时的 MustElement 会让 capture 永久卡死。
		if op.Scope == "list" {
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
