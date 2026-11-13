package onboard

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// defaultPerms 是 Hub 能力矩阵所需的最小权限集(见 README 的 enable-perms 说明)。
var defaultPerms = []string{"im:message", "im:chat", "bitable:app"}

// setupStage 一个可自动化的应用配置阶段。
// Probe 部分的 Ops 只读(点菜单 + 读页面文本),用来做幂等检测;
// Apply 部分才会产生真实副作用(开通权限 / 提交版本),必须经 confirmed 门。
type setupStage struct {
	ID        string
	Action    *BrowserAction
	Done      func(pageText string) bool
	ApplyNote string // 非空:probe 后不执行任何 apply,原因如实回传(not_required),避免未校准的不可逆动作
	// PlanLine 是确认门清单里的平台专属动作描述(声明式,避免编排器按平台硬编码)。
	// 为空时 setupPlan 回退到通用文案。
	PlanLine string
	// ApplyWarning 非空:apply 照常执行,但把未端到端验证的后续动作如实回传
	// (status=applied_unverified),不伪装成 done。
	ApplyWarning string
}

// SetupResult 是 hub.onboard.setup 的返回:把「点进应用 → 创建应用 → 抓凭据 →
// 开权限 → 发布」串成一次调用,每个阶段单独给状态,避免半成品被误报为成功。
type SetupResult struct {
	Platform   string           `json:"platform"`
	Phase      string           `json:"phase"` // need_confirmation / done / partial / browser_missing / waiting_login / unsupported
	AppURL     string           `json:"app_url,omitempty"`
	BotName    string           `json:"bot_name,omitempty"`
	Bots       []string         `json:"bots,omitempty"`
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
	// 不再按平台硬编码分流:创建/权限/发布动作由 platformStages(p) 声明式提供,
	// 凭据动作由 Portal.Steps 提供。新增平台只需补清单,不改编排器。
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
	appURL, st := openAppForSetup(p, w, appName)
	res.Steps = append(res.Steps, st)
	if platform == "wecom" && optionalAppName([]string{appName}) != "" {
		res.BotName = optionalAppName([]string{appName})
	}
	if st.Status == "need_bot_selection" || st.Status == "app_not_found" || st.Status == "bot_identity_mismatch" {
		res.Phase = st.Status
		res.Next = st.Message
		if platform == "wecom" {
			res.Bots = listWeComBotNames(p)
		}
		return res, nil
	}
	res.AppURL = appURL
	if st.Status == "browser_missing" || st.Status == "selector_miss" {
		res.Phase = "partial"
		res.Next = "自动打开应用详情页失败:" + st.Message + ";可重跑 hub.onboard.start 后重试"
		return res, nil
	}
	if st.Status == "page_loading" {
		// 列表页还没渲染完时,既不能断定「有应用」也不能断定「没有应用」;
		// 直接返回,不生成含「创建应用」的确认清单,避免用户误点不可撤销动作。
		res.Phase = "page_loading"
		res.Next = st.Message + ";请等待 1~2 秒后重试 hub.onboard.setup(confirmed=false),不要确认创建应用,也不要让用户切终端"
		return res, nil
	}

	// 只有「已确认应用列表页渲染完成且确实没有应用」才允许代建;
	// 另外还要求清单里声明了 create-* 动作,token_page 无需创建。
	needCreate := st.Status == "need_create_app" && createAppAction(p) != nil
	if !confirmed {
		res.Phase = "need_confirmation"
		res.Plan = setupPlan(p, appName, needCreate)
		res.Next = "请用选择框让用户确认以上动作(创建应用、开通权限与发布版本等会在企业里真实产生记录),确认后带 confirmed=true 重新调用 hub.onboard.setup"
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
		// 已选中的机器人名必须贯穿接入收尾,否则又回到未消歧的旧 Capture。
		cap, err := CaptureNamed(platform, optionalAppName([]string{appName}), reload)
		if err != nil {
			return nil, err
		}
		res.Capture = cap
		res.BotName = cap.BotName
		res.Steps = append(res.Steps, SetupStep{ID: "capture", Status: cap.Phase, Message: cap.Next})
	}

	// 阶段 4/5:按平台声明式执行权限与发布(platformStages)。
	// 飞书的权限阶段先走只读 API 探针(A),其余平台走「点菜单 + readPage」幂等检测。
	// 各阶段独立执行、独立记录(C):旧版遇第一个 selector_miss 就整体 return,
	// 导致 enable_perms 失败时 publish 根本没被调用,却被误读成「平台侧已完成」。
	probe := probePerms(platform)
	res.PermProbe = &probe
	for _, stage := range platformStages(p) {
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
			"请调 hub.onboard.inspect 校准后只重试失败阶段,不要重复提交表单。" +
			"若失败的是凭据阶段,可改用 hub.onboard.set_credentials 零 DOM 收尾"
		return res, nil
	}
	res.Phase = "done"
	intro := "应用详情页已自动打开/创建、凭据已写入并热重载;权限与发布阶段均已独立处理。"
	if len(platformStages(p)) == 0 {
		intro = "凭据授权页已自动打开、凭据已写入并热重载;该平台无需创建应用/开权限/发布版本。"
	}
	res.Next = intro +
		"请调 hub.health 确认 credentials=true / mounted=true。already_done=平台侧原本已完成;" +
		"api_verified=权限 API 探针已通过、跳过 UI 开通;skipped=未能自动确认、未做任何变更"
	if len(skipped) > 0 {
		res.Next += "。注意以下阶段未完全自动确认: " + strings.Join(skipped, "、") +
			",详见对应 step.message(not_required=动作未校准、刻意不提交;applied_unverified=表单已提交但发布等后续动作未真机验证,需人工确认),不要用 done 覆盖这些未验证项"
	}
	return res, nil
}

// setupPlan 按平台声明生成确认门里的动作清单:token_page 没有应用/版本概念,
// app_console 平台按各自的 Perms 与 platformStages 列条目,不再硬编码飞书权限集。
func setupPlan(p *Portal, appName string, needCreate bool) []string {
	if appName == "" {
		appName = defaultAppName
	}
	labels := []string{}
	for _, c := range PortalCreds[p.Name] {
		labels = append(labels, c.Label)
	}
	readLine := "读取 " + strings.Join(labels, " / ") + " → 格式校验 → 真连校验 → 写 .env → 热重载挂载"
	plan := []string{}
	if p.Flow == "token_page" {
		return append(plan, "自动打开凭据授权页(无需用户点击)", readLine)
	}
	if needCreate {
		plan = append(plan, "创建应用「"+appName+"」(企业内真实产生且不可撤销;当前页面未检测到已有应用,请先核对避免重复创建)")
	}
	plan = append(plan, "自动打开应用详情页(无需用户点击)", readLine)
	stages := platformStages(p)
	if len(p.Perms) > 0 {
		// 平台声明了专属清单就用它(钉钉要写清会自动点哪些权限点、会留记录),
		// 否则回退通用文案,避免编排器里散落 if platform == "dingtalk"。
		line := ""
		for _, s := range stages {
			if s.ID == "enable_perms" && s.PlanLine != "" {
				line = s.PlanLine
				break
			}
		}
		if line == "" {
			label := "开通权限: "
			for _, s := range stages {
				if s.ID == "enable_perms" && s.ApplyNote != "" {
					label = "核对权限(只读,不自动开通): "
				}
			}
			line = label + strings.Join(p.Perms, ", ") + "(已开通则跳过)"
		}
		plan = append(plan, line)
	}
	for _, s := range stages {
		if s.ID != "publish" {
			continue
		}
		if s.PlanLine != "" {
			plan = append(plan, s.PlanLine)
			continue
		}
		if s.ApplyNote != "" {
			plan = append(plan, "核对版本发布状态(已发布则跳过;不自动创建版本)")
		} else {
			plan = append(plan, "创建版本 1.0.0 并申请发布(检测到已发布则跳过)")
		}
	}
	return plan
}

// openAppForSetup 只读地定位应用详情页;列表为空时返回 need_create_app 交给上层代建。
func openAppForSetup(p *Portal, w *WatchResult, appName ...string) (string, SetupStep) {
	// token_page 平台(腾讯文档)的授权页本身就是凭据页:直接确保它已打开,
	// 不去列应用、不触发创建,也不需要用户点击任何按钮。
	if p.Flow == "token_page" {
		if w.InAppDetail && w.DetailURL != "" {
			return w.DetailURL, SetupStep{ID: "open_app", Status: "already_open"}
		}
		if !openDebugTab(p.URL) {
			return "", SetupStep{ID: "open_app", Status: "browser_missing",
				Message: "CDP 未能打开凭据授权页 " + p.URL}
		}
		return p.URL, SetupStep{ID: "open_app", Status: "done"}
	}
	if w.InAppDetail && w.DetailURL != "" {
		if p.Name == "wecom" {
			return wecomOpenDetail(p, w.DetailURL, optionalAppName(appName))
		}
		return w.DetailURL, SetupStep{ID: "open_app", Status: "already_open"}
	}
	apps, listVerified, err := listExistingApps(p)
	if err != nil {
		return "", SetupStep{ID: "open_app", Status: "browser_missing", Message: err.Error()}
	}
	if len(apps) == 0 {
		if listVerified {
			return "", SetupStep{ID: "open_app", Status: "need_create_app",
				Message: "已自动进入应用列表页并确认页面渲染完成,未发现已有应用"}
		}
		// 入口页/加载中的列表页读不到应用,不等于企业里没有应用。
		// 这是硬闸门:未确认列表页渲染完成就绝不进入创建流程(创建不可撤销)。
		return "", SetupStep{ID: "open_app", Status: "page_loading",
			Message: "已自动进入应用列表页,但页面尚未渲染完成(未在页面上读到列表特征);此时不能判断企业内是否已有应用"}
	}
	// 列表未确认渲染完成时,即便读到了条目也可能是脏数据(2026-11-12 真机:
	// 企微官网营销页的「工作台管理」被当成已有机器人)。此时绝不能点进去,
	// 否则确认清单会漏掉「创建应用」。飞书未声明 markers,不启用该门。
	if !listVerified && len(p.AppListMarkers) > 0 {
		return "", SetupStep{ID: "open_app", Status: "page_loading",
			Message: "已自动进入应用列表页,但页面尚未确认渲染完成(未读全列表特征);此时读到的应用条目不可信,不能据此判断企业内是否已有应用"}
	}
	targetName := defaultAppName
	if len(appName) > 0 && strings.TrimSpace(appName[0]) != "" {
		targetName = strings.TrimSpace(appName[0])
	}
	selected := selectAppByName(apps, targetName)
	if p.Name == "wecom" {
		requested := optionalAppName(appName)
		var names []string
		var found bool
		selected, names, found = resolveWeComApp(apps, requested)
		if !found {
			status := "need_bot_selection"
			message := "检测到多个企业微信机器人,请询问用户使用哪个机器人,并带 app_name 重试;本次未打开或读取任何机器人凭据"
			if requested != "" {
				status = "app_not_found"
				message = "未找到名称与 " + requested + " 完全匹配的企业微信机器人;请从现有机器人中明确选择,本次未自动改选"
			}
			if len(names) == 0 && requested != "" {
				message = "未能从已有详情页确认机器人 " + requested + " 的身份;请从列表中明确选择,本次未读取任何机器人凭据"
			}
			return "", SetupStep{ID: "open_app", Status: status, Message: message + ";候选: " + strings.Join(names, "、")}
		}
		targetName = selected.Name
	}
	if selected.NeedsClick {
		// 点击已明确选择的应用行只是导航,不提交任何表单、不创建任何东西。
		if detail, ok := clickAppRow(p, targetName); ok {
			return detail, SetupStep{ID: "open_app", Status: "done",
				Message: "已自动进入应用列表页,并点击进入已有应用「" + selected.Name + "」的详情页(未新建应用)"}
		}
		return "", SetupStep{ID: "open_app", Status: "selector_miss",
			Message: "已自动进入应用列表页并检测到已有应用「" + selected.Name + "」,但点击进入详情失败(行选择器未命中);请在浏览器点进该应用后重试,不要创建重复应用"}
	}
	target := absoluteAppURL(p, selected.URL)
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

// optionalAppName 区分「用户未选」与默认值。默认值只能用于创建,不能用于在多个
// 已有机器人中自动选择。
func optionalAppName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimSpace(names[0])
}

// listWeComBotNames 只读返回候选,失败时返回空清单,不把失败伪装成没有机器人。
func listWeComBotNames(p *Portal) []string {
	apps, _, err := listExistingApps(p)
	if err != nil {
		return nil
	}
	_, names, _ := resolveWeComApp(apps, "")
	return names
}

// wecomOpenDetail 详情页也需要身份门:SPA 停在「测试」时,不能因为 URL 有
// aibotid 就把它的凭据写入另一个机器人配置。
func wecomOpenDetail(p *Portal, detailURL, requested string) (string, SetupStep) {
	bx, err := NewBrowserExecutorForPlatform("", p)
	if err != nil {
		return "", SetupStep{ID: "open_app", Status: "browser_missing", Message: err.Error()}
	}
	defer bx.Close()
	name := ""
	if page := findWeComDetailPage(bx, detailURL); page != nil {
		name = weComDetailBotName(page)
	}
	if name == "" {
		return "", SetupStep{ID: "open_app", Status: "bot_identity_mismatch",
			Message: "当前企微详情页未能确认机器人名称;请从列表明确选择机器人,本次未读取凭据"}
	}
	if requested != "" && !strings.EqualFold(name, requested) {
		return "", SetupStep{ID: "open_app", Status: "bot_identity_mismatch",
			Message: "当前详情页是机器人「" + name + "」,不是指定的「" + requested + "」;请从列表明确选择,本次未读取凭据"}
	}
	if requested == "" {
		// 此函数没有 WatchResult,先只读检查登录态;已过期时不要再打开列表页。
		// 企微过期跳转与自动导航会互相抢焦点并重复开登录标签。保守停在选择门。
		if current, watchErr := Watch("wecom"); watchErr == nil {
			for _, pageURL := range current.Pages {
				if portalIsLoginPage(p, pageURL) && portalPageBelongs(p, pageURL) {
					return "", SetupStep{ID: "open_app", Status: "need_bot_selection",
						Message: "企微列表登录态已过期;请完成扫码登录后重新调用并从列表明确选择机器人,本次未读取凭据"}
				}
			}
		}
		apps, verified, e := listExistingApps(p)
		if e != nil || !verified {
			// 详情页仍在而列表登录态过期时,不能拿残留页面证明「只有一个机器人」。
			// 宁可要求明确选择,也不能在未核对完整列表时读取/覆盖凭据。
			return "", SetupStep{ID: "open_app", Status: "need_bot_selection",
				Message: "当前详情页为「" + name + "」,但完整机器人列表暂不可确认(可能登录态过期);请从列表明确选择并带 app_name 重试,本次未读取凭据"}
		}
		_, _, found := resolveWeComApp(apps, name)
		if len(apps) > 1 && found {
			return "", SetupStep{ID: "open_app", Status: "need_bot_selection",
				Message: "检测到多个企业微信机器人,当前详情页为「" + name + "」;请询问用户并带 app_name 重试,本次未读取凭据"}
		}
	}
	return detailURL, SetupStep{ID: "open_app", Status: "already_open",
		Message: "已确认当前详情页机器人身份为「" + name + "」"}
}

func weComDetailBotName(page *rod.Page) string {
	if page == nil {
		return ""
	}
	result, err := page.Timeout(browserOpTimeout).Eval(`() => document.querySelector('span.name')?.textContent?.trim() || ''`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Value.Str())
}

func findWeComDetailPage(bx *BrowserExecutor, detailURL string) *rod.Page {
	var found *rod.Page
	_ = bx.safe(func() {
		for _, page := range bx.browser.MustPages() {
			if info, err := page.Info(); err == nil && info != nil && info.URL == detailURL {
				found = page
				return
			}
		}
	})
	return found
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

// 各平台的声明式「权限 / 发布」阶段见 stages.go 的 platformStages(p)。

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
		// 凭据与创建阶段是接入的硬前提:任何非 done 状态都不能被后续阶段的
		// 成功「洗」成整体 done(旧版汇总只认 selector_miss/browser_missing 的漏洞)。
		if s.ID == "capture" || s.ID == "create_app" {
			if s.Status != "done" {
				failed = append(failed, s.ID)
			}
			continue
		}
		switch s.Status {
		case "selector_miss", "browser_missing", "waiting_app_detail", "waiting_login", "page_loading":
			failed = append(failed, s.ID)
		case "skipped", "unsupported", "not_required", "applied_unverified":
			skipped = append(skipped, s.ID)
		}
	}
	return failed, skipped
}

// apiProbeApplies 只有飞书的 enable_perms 阶段有只读 API 探针可用。
// 抽成纯函数便于单测锁定「其他平台不会被 unsupported 探针短路」这一边界:
// 它们没有权限校验 API,必须靠页面文本做幂等检测,不能一律 skipped。
func apiProbeApplies(stageID, platform string, probe *PermProbeResult) bool {
	return stageID == "enable_perms" && platform == "feishu" && probe != nil
}

// runSetupStage 先跑只读 probe 做幂等检测,未达成才跑有副作用的 apply。
// 任一步失败立即停止并返回页面候选元素,绝不重试提交表单。
//
// enable_perms 阶段优先依据 API 探针短路(A):权限已可用或无法确认时不打开浏览器、
// 不点任何按钮;只有 needs_perms(平台明确报权限不足)才进入 UI 开通流程。
func runSetupStage(stage setupStage, p *Portal, probe *PermProbeResult) (SetupStep, []string) {
	// 只有飞书实现了只读 API 权限探针。其他平台没有可调用的权限校验 API,
	// 不能因 probe.status=unsupported 就整体 skipped(那样 UI 开通永远不执行),
	// 改为走下面「点菜单 + readPage」的只读幂等检测,由 Done(text) 判断是否已开通。
	if apiProbeApplies(stage.ID, p.Name, probe) {
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
	bx, err := NewBrowserExecutorForPlatform("", p)
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

	// 阶段未达成时,若平台明确声明「不做自动 apply」(钉钉权限/版本),
	// 如实返回 not_required + 原因,绝不用未校准的选择器去点不可逆按钮。
	if stage.ApplyNote != "" {
		return SetupStep{ID: stage.ID, Status: "not_required", Message: stage.ApplyNote}, nil
	}
	apply := opsForScope(stage.Action, "apply")
	if len(apply) == 0 {
		return SetupStep{ID: stage.ID, Status: "unsupported"}, nil
	}
	if err := bx.RunAction(&BrowserAction{Script: stage.Action.Script, Ops: apply}, nil); err != nil {
		return SetupStep{ID: stage.ID, Status: "selector_miss", Message: err.Error()}, collectCandidates(bx)
	}
	// apply 执行完但仍有未真机验证的后续动作(如钉钉「保存」后的「发布」):
	// 如实标 applied_unverified,绝不用 done 掩盖这条边界。
	if stage.ApplyWarning != "" {
		return SetupStep{ID: stage.ID, Status: "applied_unverified", Message: stage.ApplyWarning}, nil
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
			if portalDetailMatch(p, info.URL) {
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
	bx, err := NewBrowserExecutorForPlatform("", p)
	if err != nil {
		res.Next = "未找到 " + p.Title + " 的标签(当前标签可能属于其他平台): " + err.Error() +
			";先调用 hub.onboard.start 打开目标平台页面,再重试 inspect"
		return res, nil
	}
	defer bx.Close()

	// 旧版忽略 focusDetailPage 的返回值,详情页不存在时会把「第一个真实标签」
	// (常常是上一次接入留下的飞书页)当成目标平台页面 dump 出来,误导 Agent。
	inDetail := focusDetailPage(bx, p)
	if !inDetail && !bx.FocusPlatform(p) {
		res.Next = "未找到 " + p.Title + " 的标签;先调用 hub.onboard.start 打开目标平台页面"
		return res, nil
	}
	res.URL = bx.CurrentURL()
	res.Candidates = collectCandidates(bx)
	_ = bx.safe(func() {
		el := bx.page.Timeout(browserOpTimeout).MustElement("body")
		res.PageText = el.Timeout(browserOpTimeout).MustText()
	})
	if len(res.PageText) > 3000 {
		res.PageText = res.PageText[:3000] + "...(已截断)"
	}
	if inDetail {
		res.Next = "用 candidates 里的按钮文本 / 输入框 placeholder 校准 setup 阶段 Ops;此工具只读,不改变页面"
	} else {
		res.Next = "当前停在 " + p.Title + " 的入口/列表页(非应用详情页);candidates 来自本平台页面,不是其他平台标签。" +
			"如需校准详情页 Ops,先让 setup 打开应用详情页再重试;此工具只读,不改变页面"
	}
	return res, nil
}
