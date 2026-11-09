package onboard

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// defaultPerms 是 Hub 能力矩阵所需的最小权限集(见 README 的 enable-perms 说明)。
var defaultPerms = []string{"im:message", "im:chat", "bitable:app"}

// setupStage 一个可自动化的应用配置阶段。
// Probe 部分的 Ops 只读(点菜单 + 读页面文本),用来做幂等检测;
// Apply 部分才会产生真实副作用(开通权限 / 提交版本),必须经 confirmed 门。
type setupStage struct {
	ID     string
	Action *BrowserAction
	Done   func(pageText string) bool
}

// SetupResult 是 hub.onboard.setup 的返回:把「点进应用 → 创建应用 → 抓凭据 →
// 开权限 → 发布」串成一次调用,每个阶段单独给状态,避免半成品被误报为成功。
type SetupResult struct {
	Platform   string           `json:"platform"`
	Phase      string           `json:"phase"` // need_confirmation / done / partial / browser_missing / waiting_login / unsupported
	AppURL     string           `json:"app_url,omitempty"`
	Plan       []string         `json:"plan"`
	Steps      []SetupStep      `json:"steps"`
	Candidates []string         `json:"candidates,omitempty"`
	Capture    *CaptureResult   `json:"capture,omitempty"`
	PermProbe  *PermProbeResult `json:"perm_probe,omitempty"`
	Next       string           `json:"next"`
}

// SetupStep 单个阶段的执行结果。
type SetupStep struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// InspectResult 是 hub.onboard.inspect 的返回:纯只读的页面元素快照,
// 用于在真实浏览器里校准 portal/setup 的 Ops 选择器(不猜 class)。
type InspectResult struct {
	Platform   string   `json:"platform"`
	URL        string   `json:"url"`
	Candidates []string `json:"candidates"`
	PageText   string   `json:"page_text"`
	Next       string   `json:"next"`
}

// Setup 端到端完成应用接入。用户只需在浏览器里登录并确认一次动作清单,
// 之后点应用/创建应用/抓凭据由调试 Chrome 代做,权限先用只读 API 探针验证
// (必要时才进 UI 开通),发布版本独立执行。
//
// 安全边界:任何有副作用的动作(创建应用、开通权限、提交版本)都受 confirmed 门
// 保护;权限优先用只读 API 探针验证,单阶段选择器未命中只停该阶段、不阻断后续阶段,
// 并附页面候选元素,绝不静默重试提交。
func Setup(platform, appName string, confirmed bool, reload func(map[string]string) error) (*SetupResult, error) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}
	res := &SetupResult{
		Platform: platform, Plan: []string{}, Steps: []SetupStep{}, Candidates: []string{},
	}
	if platform != "feishu" {
		res.Phase = "unsupported"
		res.Next = "当前仅 feishu 定义了创建/开权限/发布动作;其余平台请人工完成应用配置,再调用 hub.onboard.capture"
		return res, nil
	}

	w, err := Watch(platform)
	if err != nil {
		return nil, err
	}
	if !w.ChromeReady {
		res.Phase = "browser_missing"
		res.Next = "调试 Chrome 未就绪;先调用 hub.onboard.start,再重试 hub.onboard.setup"
		return res, nil
	}
	if w.LoginState == "waiting_login" {
		res.Phase = "waiting_login"
		res.Next = "登录(扫码/验证码)必须人工完成;之后继续轮询 hub.onboard.watch,变成 logged_in 再重试 hub.onboard.setup"
		return res, nil
	}

	// 阶段 1:自动点进应用。优先用已打开的详情页,否则从列表页读出应用链接直接打开,
	// 全程不依赖易变的 DOM 卡片选择器,也不需要用户点击。
	appURL, st := openAppForSetup(p, w)
	res.Steps = append(res.Steps, st)
	res.AppURL = appURL
	if st.Status == "browser_missing" || st.Status == "selector_miss" {
		res.Phase = "partial"
		res.Next = "自动打开应用详情页失败:" + st.Message + ";可重跑 hub.onboard.start 后重试"
		return res, nil
	}

	needCreate := st.Status == "need_create_app"
	if !confirmed {
		res.Phase = "need_confirmation"
		res.Plan = setupPlan(appName, needCreate)
		res.Next = "请用选择框让用户确认以上动作(尤其创建应用与发布版本会在企业里真实产生记录),确认后带 confirmed=true 重新调用 hub.onboard.setup"
		return res, nil
	}

	// 阶段 2:列表为空时代建应用。CreateApp 自带幂等门与确认门,这里已确认故传 true;
	// 它内部会顺带完成一次 capture,因此不再重复抓取。
	if needCreate {
		created, err := CreateApp(platform, appName, true, reload)
		if err != nil {
			return nil, err
		}
		res.Steps = append(res.Steps, SetupStep{ID: "create_app", Status: created.Phase, Message: created.Next})
		res.Capture = created.Capture
		if len(created.Candidates) > 0 {
			res.Candidates = append(res.Candidates, created.Candidates...)
		}
		if created.Phase != "done" {
			res.Phase = "partial"
			res.Next = created.Next
			return res, nil
		}
		res.AppURL = created.CreatedURL
	}

	// 阶段 3:抓凭据 → 校验 → 写 .env → 热重载(create_app 已做过就跳过)。
	if res.Capture == nil {
		cap, err := Capture(platform, reload)
		if err != nil {
			return nil, err
		}
		res.Capture = cap
		res.Steps = append(res.Steps, SetupStep{ID: "capture", Status: cap.Phase, Message: cap.Next})
	}

	// 阶段 4/5:权限与版本发布。
	// 权限先走只读 API 探针(A),只有平台明确报「权限不足」才进 UI 开通流程。
	// 各阶段独立执行、独立记录(C):旧版遇第一个 selector_miss 就整体 return,
	// 导致 enable_perms 失败时 publish 根本没被调用,却被误读成「平台侧已完成」。
	probe := probePerms(platform)
	res.PermProbe = &probe
	for _, stage := range feishuSetupStages() {
		step, candidates := runSetupStage(stage, p, &probe)
		res.Steps = append(res.Steps, step)
		if len(candidates) > 0 {
			res.Candidates = append(res.Candidates, candidates...)
		}
	}
	failed, skipped := setupStageOutcomes(res.Steps)
	if len(failed) > 0 {
		res.Phase = "partial"
		res.Next = "以下阶段未自动完成: " + strings.Join(failed, "、") +
			"。各阶段相互独立,已成功的阶段不会重跑;candidates 含页面候选元素(带标签/role)," +
			"请调 hub.onboard.inspect 校准后只重试失败阶段,不要重复提交表单"
		return res, nil
	}
	res.Phase = "done"
	res.Next = "应用已自动打开/创建、凭据已写入并热重载;权限与版本发布阶段均已独立处理。" +
		"请调 hub.health 确认 credentials=true / mounted=true。already_done=平台侧原本已完成;" +
		"api_verified=权限 API 探针已通过、跳过 UI 开通;skipped=未能自动确认、未做任何变更"
	if len(skipped) > 0 {
		res.Next += "。注意以下阶段为跳过(未自动确认): " + strings.Join(skipped, "、") +
			",详见对应 step.message,不要用 done 覆盖这些未验证项"
	}
	return res, nil
}

// setupPlan 生成确认门里要展示给用户的动作清单。
func setupPlan(appName string, needCreate bool) []string {
	if appName == "" {
		appName = defaultAppName
	}
	plan := []string{}
	if needCreate {
		plan = append(plan, "创建企业自建应用「"+appName+"」(企业内真实产生且不可撤销)")
	}
	return append(plan,
		"自动打开应用详情页(无需用户点击)",
		"读取 App ID / App Secret → 校验 → 写 .env → 热重载挂载",
		"开通权限: "+strings.Join(defaultPerms, ", ")+"(已开通则跳过)",
		"创建版本 1.0.0 并申请发布(检测到已发布则跳过)",
	)
}

// openAppForSetup 只读地定位应用详情页;列表为空时返回 need_create_app 交给上层代建。
func openAppForSetup(p *Portal, w *WatchResult) (string, SetupStep) {
	if w.InAppDetail && w.DetailURL != "" {
		return w.DetailURL, SetupStep{ID: "open_app", Status: "already_open"}
	}
	apps, err := listExistingApps(p)
	if err != nil {
		return "", SetupStep{ID: "open_app", Status: "browser_missing", Message: err.Error()}
	}
	if len(apps) == 0 {
		return "", SetupStep{ID: "open_app", Status: "need_create_app", Message: "应用列表为空,需创建应用"}
	}
	target := absoluteAppURL(p, apps[0].URL)
	if !openDebugTab(target) {
		return "", SetupStep{ID: "open_app", Status: "selector_miss", Message: "CDP 未能打开应用详情页 " + target}
	}
	// 新标签出现需要一点时间,短轮询确认后再继续,避免后面 capture 误判等待人工。
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if w2, err := Watch(p.Name); err == nil && w2.InAppDetail {
			return w2.DetailURL, SetupStep{ID: "open_app", Status: "done"}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return target, SetupStep{ID: "open_app", Status: "done", Message: "已发起打开,页面仍在加载"}
}

// absoluteAppURL 列表页的 href 常是相对路径,补成可直接打开的绝对 URL。
func absoluteAppURL(p *Portal, raw string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return u.Scheme + "://" + u.Host + raw
	}
	return u.Scheme + "://" + u.Host + "/" + raw
}

// feishuSetupStages 权限与发布的声明式动作。选择器一律按可见文本匹配,
// 不绑定易变的 class;未命中时由上层降级人工并回传候选元素。
func feishuSetupStages() []setupStage {
	return []setupStage{
		{
			ID: "enable_perms",
			Action: &BrowserAction{Script: "feishu@2026-11-perms", Ops: []Op{
				// 先等菜单文本渲染出来(SPA 异步),再点:旧版直接 click 会在页面刚加载时 6s 超时
				{Kind: "waitVisible", Target: menuCandidateSelector, Match: "^\\s*权限管理(?:\\s*\\d+)?\\s*$", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*权限管理(?:\\s*\\d+)?\\s*$", Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(开通权限|添加权限|申请权限|批量导入权限)\\s*$", Scope: "apply"},
				{Kind: "fill", Target: "input[placeholder*='搜索'], input[type=text], textarea",
					Text: strings.Join(defaultPerms, ", "), Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(确定|确认|提交|开通)\\s*$", Scope: "apply"},
			}},
			Done: func(text string) bool {
				for _, k := range defaultPerms {
					if !strings.Contains(text, k) {
						return false
					}
				}
				return true
			},
		},
		{
			ID: "publish",
			Action: &BrowserAction{Script: "feishu@2026-11-publish", Ops: []Op{
				{Kind: "waitVisible", Target: menuCandidateSelector,
					Match: "^\\s*(版本管理与发布|应用发布|版本管理)(?:\\s*\\d+)?\\s*$", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector,
					Match: "^\\s*(版本管理与发布|应用发布|版本管理)(?:\\s*\\d+)?\\s*$", Scope: "probe"},
				{Kind: "readPage", Scope: "probe"},
				{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(创建版本|新建版本)\\s*$", Scope: "apply"},
				{Kind: "fill", Target: "input[placeholder*='版本'], input[type=text], textarea", Text: "1.0.0", Scope: "apply"},
				{Kind: "fill", Target: "textarea, input[placeholder*='说明'], input[placeholder*='描述']",
					Text: "mcp-hub 接入自动发布", Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(保存|确定|提交)\\s*$", Scope: "apply"},
				{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(申请发布|发布|提交发布)\\s*$", Scope: "apply"},
			}},
			Done: func(text string) bool {
				// 飞书详情页/版本页在无需变更时会显示「当前修改均已发布」(见实测截图)
				return strings.Contains(text, "当前修改均已发布") || strings.Contains(text, "已发布且生效")
			},
		},
	}
}

func opsForScope(act *BrowserAction, scope string) []Op {
	out := []Op{}
	for _, op := range act.Ops {
		if op.Scope == scope {
			out = append(out, op)
		}
	}
	return out
}

// setupStageOutcomes 汇总各阶段结果:单阶段失败只记它自己的 ID,
// 不会掩盖其他阶段的成功,也不会中断后续阶段(C)。抽成纯函数便于单测覆盖。
func setupStageOutcomes(steps []SetupStep) (failed, skipped []string) {
	for _, s := range steps {
		switch s.Status {
		case "selector_miss", "browser_missing":
			failed = append(failed, s.ID)
		case "skipped", "unsupported":
			skipped = append(skipped, s.ID)
		}
	}
	return failed, skipped
}

// runSetupStage 先跑只读 probe 做幂等检测,未达成才跑有副作用的 apply。
// 任一步失败立即停止并返回页面候选元素,绝不重试提交表单。
//
// enable_perms 阶段优先依据 API 探针短路(A):权限已可用或无法确认时不打开浏览器、
// 不点任何按钮;只有 needs_perms(平台明确报权限不足)才进入 UI 开通流程。
func runSetupStage(stage setupStage, p *Portal, probe *PermProbeResult) (SetupStep, []string) {
	if stage.ID == "enable_perms" && probe != nil {
		switch probe.Status {
		case "api_ready":
			return SetupStep{ID: stage.ID, Status: "api_verified",
				Message: "权限 API 探针通过(" + strings.Join(probe.Checked, "、") +
					"),跳过 UI 开通流程;未单独验证: " + strings.Join(probe.Unverified, "、")}, nil
		case "no_credentials", "unknown", "unsupported":
			return SetupStep{ID: stage.ID, Status: "skipped",
				Message: "权限 API 探针未能确认(" + probe.Status + "): " + probe.Detail +
					";为避免重复提交,不进入 UI 开通流程"}, nil
		case "needs_perms":
			// 平台明确报权限不足,继续走 UI 开通流程
		}
	}
	bx, err := NewBrowserExecutor("")
	if err != nil {
		return SetupStep{ID: stage.ID, Status: "browser_missing", Message: err.Error()}, nil
	}
	defer bx.Close()

	if !focusDetailPage(bx, p) {
		return SetupStep{ID: stage.ID, Status: "browser_missing", Message: "未定位到应用详情页标签"}, collectCandidates(bx)
	}
	if err := bx.RunAction(&BrowserAction{Script: stage.Action.Script, Ops: opsForScope(stage.Action, "probe")}, nil); err != nil {
		return SetupStep{ID: stage.ID, Status: "selector_miss", Message: err.Error()}, collectCandidates(bx)
	}
	if text := bx.Collected["__page_text__"]; stage.Done != nil && stage.Done(text) {
		return SetupStep{ID: stage.ID, Status: "already_done"}, nil
	}

	apply := opsForScope(stage.Action, "apply")
	if len(apply) == 0 {
		return SetupStep{ID: stage.ID, Status: "unsupported"}, nil
	}
	if err := bx.RunAction(&BrowserAction{Script: stage.Action.Script, Ops: apply}, nil); err != nil {
		return SetupStep{ID: stage.ID, Status: "selector_miss", Message: err.Error()}, collectCandidates(bx)
	}
	return SetupStep{ID: stage.ID, Status: "done"}, nil
}

// focusDetailPage 把执行器切到应用详情页标签,避免误操作用户的其他标签页。
func focusDetailPage(bx *BrowserExecutor, p *Portal) bool {
	found := false
	_ = bx.safe(func() {
		for _, pg := range bx.browser.MustPages() {
			info, err := pg.Info()
			if err != nil || info == nil {
				continue
			}
			if matchURLPrefix(info.URL, p.URL, []string{"/app/list", "/app/create", "/app/search", "/login"}) {
				bx.page = pg
				found = true
				return
			}
		}
	})
	return found
}

// Inspect 只读地 dump 当前应用详情页上的可点击元素与输入框,以及页面可见文本。
// 用于真实浏览器里校准声明式 Ops:不猜 class,直接拿按钮文本/placeholder。
func Inspect(platform string) (*InspectResult, error) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}
	res := &InspectResult{Platform: platform, Candidates: []string{}}
	w, err := Watch(platform)
	if err != nil {
		return nil, err
	}
	if !w.ChromeReady {
		res.Next = "调试 Chrome 未就绪;先调用 hub.onboard.start"
		return res, nil
	}
	bx, err := NewBrowserExecutor("")
	if err != nil {
		res.Next = "连接调试 Chrome 失败: " + err.Error()
		return res, nil
	}
	defer bx.Close()

	focusDetailPage(bx, p)
	_ = bx.safe(func() {
		if info, e := bx.page.Info(); e == nil && info != nil {
			res.URL = info.URL
		}
	})
	res.Candidates = collectCandidates(bx)
	_ = bx.safe(func() {
		el := bx.page.Timeout(browserOpTimeout).MustElement("body")
		res.PageText = el.Timeout(browserOpTimeout).MustText()
	})
	if len(res.PageText) > 3000 {
		res.PageText = res.PageText[:3000] + "...(已截断)"
	}
	res.Next = "用 candidates 里的按钮文本 / 输入框 placeholder 校准 setup 阶段 Ops;此工具只读,不改变页面"
	return res, nil
}
