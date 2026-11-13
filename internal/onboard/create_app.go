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
	defaultAppName = "mcp-hub"
	// 2026-11-12 真机:企微 API 模式点「保存」后创建请求较慢,详情页跳转
	// 可能超过 15s。旧上限会误报 create_failed,而 Next 又让用户「手动完成
	// 创建」——实际已创建,极可能诱导重复提交不可撤销的创建表单。
	createAppURLWaitMax = 30 * time.Second
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
// NeedsClick=true 表示它来自应用列表页的行元素(不是 a[href]),URL 是列表页
// 地址,需要点击该行才会进入详情页;调用方据此走 clickAppRow,不要把它当成
// 详情页直接打开,也不要误判成「企业里没有应用」。
type AppRef struct {
	AppID      string `json:"app_id,omitempty"`
	Name       string `json:"name,omitempty"`
	URL        string `json:"url"`
	NeedsClick bool   `json:"needs_click,omitempty"`
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
		res.Next = "该平台未定义自动创建应用的浏览器动作;若已有应用,直接用 hub.onboard.status 取 platforms[].credential_fields 收集凭据并调用 hub.onboard.set_credentials 完成接入;" +
			"若还没有应用,只需请用户在平台后台人工创建一次,再回到 set_credentials。浏览器抓取动作同样未定义,也不要让用户切终端"
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
	case w.InAppDetail && platform != "wecom":
		// 企微可能同时存在多个详情页,不能凭当前页就认定是目标机器人;
		// 统一走下面的列表名称匹配与多机器人消歧。
		// 幂等门 1:其他平台沿用已打开详情页,避免重复创建。
		// 直接帮他收尾,这正是「人已登录并点进凭证页」场景下最该做的事。
		res.Phase = "already_in_detail"
		res.ExistingApps = []AppRef{{AppID: portalAppID(p, w.DetailURL), URL: w.DetailURL}}
		res.Capture, err = Capture(platform, reload)
		if err != nil {
			return nil, err
		}
		res.Next = "已检测到你停在上一个应用的详情页,为避免重复创建未新建应用;已自动尝试抓取凭据。" +
			"若 capture.phase=done 则接入完成,否则按 capture.next 继续"
		return res, nil
	}

	// 幂等门 2:列表页已经有应用卡片 → 复用,不新建。
	// listVerified 为 false 时,「读到的条目」和「空列表」都不可信:既不能据此
	// 复用,也不能据此创建不可撤销的新应用(与 setup.go openAppForSetup 的
	// 硬闸门对齐)。只对声明了 AppListMarkers 的平台(钉钉/企微)启用该门;
	// 未声明的平台(飞书)保持原有行为,不波及已真机验证的链路。
	apps, listVerified, err := listExistingApps(p)
	if err != nil {
		res.Phase = "browser_missing"
		res.Message = err.Error()
		res.Next = "读取应用列表失败;确认调试 Chrome 仍在运行后重试"
		return res, nil
	}
	strictListGate := len(p.AppListMarkers) > 0
	if len(apps) > 0 && (listVerified || !strictListGate) {
		res.Phase = "existing_apps"
		res.ExistingApps = apps
		if platform == "wecom" {
			requested := appName
			if appName == defaultAppName {
				// 区分「默认值」与「用户真的点名 mcp-hub」:只有多机器人存在时
				// 才要求重新明确选择;单机器人继续兼容旧默认名。
				requested = ""
			}
			selected, _, found := resolveWeComApp(apps, requested)
			if !found {
				res.Phase = "need_bot_selection"
				res.Next = "检测到多个企业微信机器人或未找到指定名称;请询问用户明确选择 app_name 后重试;本次未打开或读取任何机器人凭据"
				return res, nil
			}
			res.AppName = selected.Name
		}
		res.Next = "检测到已有应用,为避免在企业里重复创建不新建;" +
			"调用 hub.onboard.setup 会自动打开该应用详情页并完成抓凭据与后续配置(无需用户点击、无需登录、无需终端命令);" +
			"只需抓凭据时才让用户点进「凭证与基础信息」页再调 hub.onboard.capture"
		return res, nil
	}
	if strictListGate && !listVerified {
		res.Phase = "page_loading"
		res.Next = "应用列表页尚未确认渲染完成(未读全列表特征),此时读到的应用条目与空列表都不可信;" +
			"为避免重复创建不可撤销的应用本次不新建。请重跑 hub.onboard.start 让列表页渲染完成后再调 hub.onboard.create_app,不要重复提交创建表单"
		return res, nil
	}

	// 确认门:不显式确认就不碰任何按钮。
	if !confirmed {
		res.Phase = "need_confirmation"
		res.Next = "创建应用会在企业里真实产生一个应用且不可撤销;当前页面未检测到已有应用链接(可能停在入口页),请先确认企业内没有可复用的应用。" +
			"请用选择框让用户确认应用名(默认 \"" + appName + "\"),确认后带 confirmed=true 重新调用 hub.onboard.create_app。" +
			"图标沿用平台默认;后续阶段由 hub.onboard.setup 按各平台声明继续自动完成(部分平台无版本/发布概念)"
		return res, nil
	}

	before := pageSet()
	bx, err := NewBrowserExecutorForPlatform("", p)
	if err != nil {
		res.Phase = "browser_missing"
		res.Message = err.Error()
		res.Next = "先调用 hub.onboard.start(" + platform + ") 打开目标平台页面并完成登录,再重试;不要在其他平台页面上执行创建动作,也不要重复提交创建表单"
		return res, nil
	}
	defer bx.Close()

	// 只找到登录页标签时,表单必然未命中,别浪费一次有副作用的操作。
	if cur := bx.CurrentURL(); portalIsLoginPage(p, cur) {
		res.Phase = "waiting_login"
		res.Message = "当前只找到 " + p.Title + " 的登录页标签"
		res.Next = "登录(扫码/验证码)必须人工完成;完成后轮询 hub.onboard.watch,变成 logged_in 再重试。不要自动重试提交表单"
		return res, nil
	}

	if err := bx.RunAction(&BrowserAction{Script: act.Script, Ops: createAppOps(act, appName)}, nil); err != nil {
		res.Phase = "selector_miss"
		res.Message = err.Error() + ";执行器绑定页面: " + bx.CurrentURL()
		res.Candidates = collectCandidates(bx)
		res.Next = "创建表单的选择器未命中(" + p.Title + " 页面改版或 Ops 未校准),已降级,**不要再自动重试提交**。" +
			"candidates 来自本平台页面,可用于校准 Ops;也可让用户在浏览器里手动创建应用并进入凭证页后调用 hub.onboard.capture,或直接降级 hub.onboard.set_credentials"
		return res, nil
	}

	// 提交后必须验证真的产生了新应用详情页,不能只凭「点到了按钮」就算成功。
	created := waitForNewDetail(p, before)
	listed := false
	if created == "" {
		// 企微 API 模式真机:保存成功后也可能不跳详情页,只在列表页新增一行。
		// 先只读确认「应用确实已创建」,再走既有 openAppForSetup 链路进详情,
		// 绝不因为没等到 URL 变化就报「创建失败」诱导重复提交。
		if listed = createdAppInList(p, appName); listed {
			if w2, e := Watch(platform); e == nil {
				// 刚创建的机器人已由用户明确命名,只跟随这个新详情 URL;不让
				// 列表中其他机器人抢占,也不把创建流程变成自由选择流程。
				if detail, step := openAppForSetup(p, w2, appName); detail != "" && step.Status != "need_create_app" {
					created = detail
					res.Message = "保存后未立即跳转详情页;已在列表页确认新应用「" + appName +
						"」存在并自动进入其详情页(未重复创建)"
				}
			}
		}
	}
	if created == "" {
		res.Phase = "create_failed"
		res.Candidates = collectCandidates(bx)
		if listed {
			res.Message = "新应用「" + appName + "」已在列表中出现,但未能自动进入详情页"
			res.Next = "应用已创建,**不要重复提交创建表单**;请在浏览器里点进该应用详情页后调用 hub.onboard.capture 抓凭据"
		} else {
			res.Message = "提交后 " + createAppURLWaitMax.String() + " 内未出现新的应用详情页,列表页也未检测到新应用"
			res.Next = "可能触发了平台风控/二次确认,或表单未通过校验;请让用户在浏览器里核对并手动完成创建," +
				"然后调用 hub.onboard.set_credentials 收尾(或 hub.onboard.capture 自动抓取)。不要重复提交创建表单"
		}
		return res, nil
	}
	res.CreatedURL = created
	res.ExistingApps = []AppRef{{AppID: portalAppID(p, created), URL: created}}

	// 新应用已落到详情页,抓凭据 → 校验 → 写配置 → 热重载。
	res.Capture, err = CaptureNamed(platform, appName, reload)
	if err != nil {
		return nil, err
	}
	if res.Capture.Phase != "done" {
		res.Phase = "create_failed"
		res.Message = "应用已创建,但自动收尾未完成(capture.phase=" + res.Capture.Phase + ")"
		res.Next = "按 capture.next 继续;后续阶段可调用 hub.onboard.setup 按平台声明处理,无需用户手动操作(部分平台无版本/发布概念)"
		return res, nil
	}
	res.Phase = "done"
	res.Next = "应用已创建且凭据已写入 .env 并完成热重载;请调 hub.health 确认 credentials=true / mounted=true," +
		"后续阶段可调用 hub.onboard.setup 按平台声明处理(企微智能机器人无版本/发布概念)"
	return res, nil
}

// createAppAction 取清单中「创建应用」步骤的浏览器动作定义。
func createAppAction(p *Portal) *BrowserAction {
	for i := range p.Steps {
		// create-app / create-bot 都是「代建应用」步骤,按前缀匹配让新平台
		// 只需在清单里声明动作,不必改这里。
		if strings.HasPrefix(p.Steps[i].ID, "create-") && p.Steps[i].Browser != nil {
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
		// fillJS 与 fill 都支持 $app_name 占位符(2026-11-12 企微创建页校准:
		// hidden 标签上必须用纯 JS 填值,漏替换会带着字面量去提交表单)。
		if op.Kind == "fill" || op.Kind == "fillJS" {
			op.Text = strings.ReplaceAll(op.Text, "$app_name", appName)
		}
		ops = append(ops, op)
	}
	return ops
}

// markersRendered 判断列表页文本是否命中整组渲染特征。
//
// 必须整组命中:首页左侧菜单也含「智能机器人」「Intelligent robot」,单个词
// 命中会把「停在首页」误判成「列表页已渲染」,进而误创建不可撤销的应用。
// 2026-11-12 真机:企微后台会按标签语言渲染中文或英文 UI(CDP /json/new
// 打开的列表页是英文),Portal 因此可声明一组备用语言 markers,两组任一组
// 全中即视为已渲染;两组都空/都不中时返回 false,由调用方走 page_loading 门。
func markersRendered(markers []string, text string) bool {
	if len(markers) == 0 {
		return false
	}
	// SPA 每个菜单项独占一行,innerText 里的换行会把「Intelligent Bot Create」
	// 拆成「Intelligent Bot\nCreate」;2026-11-12 真机因此出现「压缩空白后
	// 看起来全中、实际 strings.Contains 全不中」。两侧都归一化再比较。
	normalized := strings.Join(strings.Fields(text), " ")
	for _, marker := range markers {
		want := strings.Join(strings.Fields(marker), " ")
		if want == "" || !strings.Contains(normalized, want) {
			return false
		}
	}
	return true
}

// listExistingApps 只读地收集已存在的应用,用于幂等判断。
//
// listVerified 表示「已确认停在应用列表页且页面文本特征全部渲染」——只有它为
// true 且结果为 0,才能如实说「企业里没有应用」。停在入口页/页面加载中时返回
// false,调用方必须按「未检测到」处理,不能据此创建不可撤销的新应用。
//
// 两类证据:详情页 URL(飞书 /app/<appId>)与列表页行元素(钉钉 tr[data-row-key]
// h5[title],不是链接)。后者由 Portal 声明式提供,不散落平台 if。
func listExistingApps(p *Portal) (apps []AppRef, listVerified bool, err error) {
	// token_page 平台没有「应用列表」概念,Setup 会直接打开授权页读凭据。
	if p.Flow == "token_page" {
		return []AppRef{}, false, nil
	}
	// 自动进入应用列表页(幂等):入口页不渲染应用列表,不进列表页就会误判。
	if p.AppListURL != "" {
		ensureAppListTab(p)
	}
	bx, err := NewBrowserExecutorForPlatform("", p)
	if err != nil {
		return nil, false, err
	}
	defer bx.Close()

	out := []AppRef{}
	seen := map[string]bool{}
	add := func(raw, name string) {
		id := portalAppID(p, raw)
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
			if portalDetailMatch(p, info.URL) {
				add(info.URL, "")
			}
			// 企微详情页先按页面名称去重;它与列表行指向同一机器人时只保留一条,
			// 避免重复条目把「单机器人」误判成「多机器人」。
			if portalDetailMatch(p, info.URL) && p.Name == "wecom" {
				detailName := strings.TrimSpace(weComDetailBotName(pg))
				duplicate := false
				for i := range out {
					if detailName != "" && strings.EqualFold(out[i].Name, detailName) {
						out[i].URL = info.URL
						out[i].NeedsClick = false
						duplicate = true
						break
					}
				}
				if !duplicate {
					add(info.URL, detailName)
				}
			}
			if hostOf(info.URL) != hostOf(p.URL) {
				continue
			}
			// 应用列表页:确认 markers 渲染后读出行元素(钉钉实测)。
			// SPA 刚打开时只有「加载中」,此时读不到行,不能当成「没有应用」;
			// 短轮询等它渲染完(连续 start→setup 的实测场景)。
			if isAppListPage(p, info.URL) {
				rendered := false
				for attempt := 0; attempt < 5; attempt++ {
					var text string
					if el, e := pg.Timeout(browserOpTimeout).Element("body"); e == nil {
						text, _ = el.Timeout(browserOpTimeout).Text()
					}
					rendered = markersRendered(p.AppListMarkers, text)
					if !rendered && len(p.AppListMarkersAlt) > 0 {
						rendered = markersRendered(p.AppListMarkersAlt, text)
					}
					if rendered {
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
				if rendered {
					listVerified = true
				}
				// 企微列表可能比详情页先读到:按名称覆盖占位详情,消除重复候选。
				if rendered && p.AppListRowSelector != "" {
					els, e := pg.Elements(p.AppListRowSelector)
					if e == nil {
						for i, el := range els {
							name := ""
							if t, e := el.Text(); e == nil {
								name = strings.TrimSpace(t)
							}
							if name == "" {
								continue
							}
							key := "row:" + name
							if p.Name == "wecom" {
								key = "row:" + strings.ToLower(name)
							}
							if seen[key] {
								if p.Name == "wecom" {
									for i := range out {
										if strings.EqualFold(out[i].Name, name) {
											out[i].URL = info.URL
											out[i].NeedsClick = true
											break
										}
									}
								}
								continue
							}
							seen[key] = true
							out = append(out, AppRef{
								AppID:      fmt.Sprintf("row-%d", i+1),
								Name:       name,
								URL:        info.URL,
								NeedsClick: true,
							})
						}
					}
				}
			}
			// a[href] 兜底扫描只在本平台管理区域的标签上跑(纵深防御:官网营销页
			// 与后台同 host,不能让它的链接参与判断)。
			if !pageInAdminArea(p, info.URL) {
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
				// 2026-11-12 真机根因见 appLinkIsPlatformApp 的注释:
				// 「工作台管理」这类后台导航链接不能被当成已有机器人。
				if !appLinkIsPlatformApp(p, *href) {
					continue
				}
				add(*href, el.MustText())
			}
		}
	})
	if err != nil {
		return nil, false, err
	}
	return out, listVerified, nil
}

// appLinkIsPlatformApp 判断 a[href] 是否指向该平台的真实应用/机器人。
//
// 2026-11-12 真机根因:企微后台首页的「工作台管理」链接是
// `#/app/workbenchEditor/index`,不含 aibotid=,却会被 portalAppID 回落的
// appIDFromURL 宽匹配抽出 workbenchEditor,当成已有机器人 → CreateApp 报
// existing_apps、Setup 确认清单漏掉「创建应用」,机器人已删除却永远不触发代建。
// 声明了 AppIDRe 的平台(钉钉/企微)只认该平台真实 ID 格式,不回落宽匹配;
// 未声明的(飞书)保留 /app/<appId> 宽匹配,不影响已真机验证的链路。
func appLinkIsPlatformApp(p *Portal, href string) bool {
	if p == nil || href == "" {
		return false
	}
	if p.AppIDRe != nil {
		return p.AppIDRe.MatchString(href)
	}
	return portalAppID(p, href) != ""
}

// collectCandidates 失败时 dump 页面上可见的按钮文本与输入框 placeholder,
// 供 Agent 转述、也便于日后校准选择器。只读,不改 DOM。
// candidatePerGroup 限制每组读取的元素数:div/span 这类泛标签可达上千个,
// 逐个 MustText 都是一次 CDP 往返,不限流会拖到会话超时。
const candidatePerGroup = 20

// collectCandidates 按选择器分组采集,输出形如 "a,[role=button]: 权限管理",
// 直接告诉调用方该文本命中了哪些标签/语义角色,便于现场校准 probe 的 Match。
// 分组从 menuCandidateSelector 派生,保证与 probe/click 的集合永不漂移。
func collectCandidates(bx *BrowserExecutor) []string {
	var out []string
	_ = bx.safe(func() {
		hits := map[string][]string{}
		order := []string{}
		for _, sel := range strings.Split(menuCandidateSelector, ", ") {
			els, e := bx.page.Elements(sel)
			if e != nil {
				continue
			}
			limit := candidatePerGroup
			if sel == "div" || sel == "span" {
				limit = 5
			}
			for i, el := range els {
				if i >= limit {
					break
				}
				t := strings.TrimSpace(el.MustText())
				if t == "" || len(t) > 40 {
					continue
				}
				if _, ok := hits[t]; !ok {
					order = append(order, t)
				}
				if !containsStrLocal(hits[t], sel) {
					hits[t] = append(hits[t], sel)
				}
			}
		}
		for i, t := range order {
			if i >= 30 {
				break
			}
			out = append(out, strings.Join(hits[t], ",")+": "+t)
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

func containsStrLocal(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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

// appNameMatches 判断列表页读到的应用里是否有目标名称(去空白、忽略大小写)。
func appNameMatches(apps []AppRef, appName string) bool {
	want := strings.ToLower(strings.TrimSpace(appName))
	if want == "" {
		return false
	}
	for _, a := range apps {
		if strings.ToLower(strings.TrimSpace(a.Name)) == want {
			return true
		}
	}
	return false
}

// selectAppByName 仅做名称精确匹配(去空白、忽略大小写)。
// 找不到时返回零值;多机器人绝不能回退第一项。单一机器人且无指定名称时,
// 由调用方显式处理旧版兼容,避免把回退藏进选择函数。
func selectAppByName(apps []AppRef, name string) AppRef {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, app := range apps {
		if want != "" && strings.ToLower(strings.TrimSpace(app.Name)) == want {
			return app
		}
	}
	return AppRef{}
}

// resolveWeComApp 在导航前消歧。defaultName 仅用于列表为空时创建默认机器人,
// 不得在多个已有机器人中自动选择。详情页优先时必须另行核对页面名称。
func resolveWeComApp(apps []AppRef, requestedName string) (AppRef, []string, bool) {
	names := make([]string, 0, len(apps))
	for _, app := range apps {
		if app.Name != "" {
			names = append(names, app.Name)
		}
	}
	if name := strings.TrimSpace(requestedName); name != "" {
		selected := selectAppByName(apps, name)
		return selected, names, selected.Name != ""
	}
	if len(apps) == 1 {
		return apps[0], names, true
	}
	return AppRef{}, names, false
}

// createdAppInList 只读确认「应用是否已在列表页出现」:只有列表渲染校验通过
// 才可信,避免把加载中/脏数据当成创建成功。
func createdAppInList(p *Portal, appName string) bool {
	apps, verified, err := listExistingApps(p)
	return err == nil && verified && appNameMatches(apps, appName)
}

func waitForNewDetail(p *Portal, before map[string]bool) string {
	deadline := time.Now().Add(createAppURLWaitMax)
	for time.Now().Before(deadline) {
		pages, err := debugPages()
		if err == nil {
			for _, u := range pages {
				if !before[u] && portalDetailMatch(p, u) {
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
