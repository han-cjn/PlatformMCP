package onboard

import (
	"errors"
	"regexp"
	"strings"
)

// Step 接入引导的一个步骤,两种执行器共用。
type Step struct {
	ID      string         // 步骤标识,如 "create-app"
	Brief   string         // 终端一行说明
	Detail  []string       // 展开说明:具体点哪里、勾哪些权限
	URLs    []string       // 相关页面,手动模式下尝试 open
	Browser *BrowserAction // 浏览器执行器动作;nil 表示该步必须人工(如登录/审批)
	Cred    bool           // true 表示此步骤要收集凭据(向导进入输入环节)
}

// Cred 单个凭据的收集与校验定义。
type Cred struct {
	EnvKey string
	Label  string
	Format func(string) error // 非空格式校验(前缀/长度)
}

// Portal 一个平台的完整接入清单。
type Portal struct {
	Name     string
	Title    string
	URL      string
	Steps    []Step
	Validate func(env map[string]string) error // 真连校验(见 validate.go)

	// ---- 平台差异(声明式,避免各工具里散落 if platform == "feishu") ----
	// Flow: "app_console" = 应用控制台(有列表/详情/创建/权限/发布);
	//       "token_page"  = 凭据直接展示在授权页(无应用概念,如腾讯文档 MCP Token)。
	Flow       string         // app_console / token_page
	CredPages  []string       // 详情页/凭据页 URL 前缀,任一命中即视为 in_app_detail
	LoginHosts []string       // 登录页 host 片段,补充默认的 accounts.* 与 /login
	AppIDRe    *regexp.Regexp // 从 URL 抽取应用/机器人 ID;nil 用默认 /app/<id>
	Perms      []string       // 该平台需开通的权限(展示在 plan,也用于页面文本幂等检测)
	// ApplyPerms 是 apply 阶段真正要去点的权限点。Perms 包含「默认已开通」
	// 的项(如钉钉 snsapi_base),那些行没有「立即开通」按钮,不能拿来点。
	// 为空表示该平台没有可安全自动开通的权限点。
	ApplyPerms []string

	// ---- 应用列表页(2026-11-11 钉钉真机实测) ----
	// 入口页不渲染应用列表,Agent 若停在入口页就会把「读不到应用链接」误判成
	// 「企业里没有应用」,进而在确认框里提出创建不可撤销的新应用。因此把
	// 「如何自动进入应用列表页」也做成声明式配置,由 start/setup 统一调用。
	// AppListURL 必须是实测可直达、且登录态在同一持久化 profile 内保留的 URL;
	// 为空表示该平台没有独立列表页(或尚未实测),不做任何自动导航。
	AppListURL         string   // 应用列表页 URL
	AppListMarkers     []string // 列表页可见文本特征,用于确认页面已渲染
	AppListRowSelector string   // 列表页已有应用的可点击元素(每行一个),读到即证明已有应用

	// ---- 应用详情内的凭据 tab(2026-11-11 钉钉真机实测) ----
	// 钉钉后台是单页多 tab:/baseinfo(凭证) /permission(权限) /version(版本)。
	// 点进应用默认落在 /permission,不切 tab 就读不到 Client ID / Client Secret,
	// 旧版 collect-creds 直接 readPage 必然 extract 失败。
	CredTab         string // 左侧菜单文本,用于自动切到凭据页
	CredTabFragment string // 凭据页 URL hash 片段,用于确认切页是否成功
}

// BrowserAction 浏览器执行器动作序列;Script 为选择器版本标记(平台改版只改数据)。
type BrowserAction struct {
	Script string
	Ops    []Op
}

// Op 单个浏览器操作。Target 为 CSS 选择器;ExtractKey 表示把读到的值
// 存入凭据收集结果(envKey)。Index 表示多匹配元素取第几个(0 起):飞书
// Secret 的「显示」按钮是同 class 的第 2 个(第 1 个是复制)。
type Op struct {
	Kind        string   // navigate / hover / click / waitVisible / waitURL / readText / readPage / copyToCred / extract / fill / screenshot / clickInRow
	Target      string   // CSS 选择器(支持逗号分隔的多候选)或 URL 前缀(navigate/waitURL);clickInRow / copyToCred 时为「行/容器」选择器
	Text        string   // fill 的输入内容(支持 "$env:KEY");clickInRow 时为行内要点的按钮文本;copyToCred 时为复制按钮选择器
	ExtractKey  string   // readText 抽取到的值存入该 envKey
	Index       int      // 多匹配时取第几个;0 取首个
	MaskRune    rune     // readText 识别的掩码字符(0 表示用 defaultMaskRune)
	URLExcludes []string // waitURL 命中需排除的 URL 片段(过滤分页/搜索等假命中)
	Match       string   // JS 正则:按元素可见文本匹配(click/hover/fill),不依赖易变的 class
	SkipIfMatch string   // clickInRow 专用:行文本命中该正则视为已达成,幂等跳过点击(如钉钉「已开通」)
	Scope       string   // 适用阶段:"list" 仅应用列表页;"probe" 只读探测;"apply" 有副作用;空/detail 表示详情页可用
}

// defaultMaskRune 飞书/钉钉类平台凭据掩码的默认字符(U+2217 ASTERISK OPERATOR)。
const defaultMaskRune = '\u2217'

// menuCandidateSelector 是「菜单/按钮」元素的统一候选选择器。
//
// probe/click 与 collectCandidates 必须共用同一份标签集合,否则会出现
// 「inspect 候选里明明有『权限管理』、ElementR 却遍历不到该元素」的假超时
// (2026-11-09 实测踩坑:probe 用 a/li/div/span,候选采集用 button/a/[role=button])。
const menuCandidateSelector = "a, li, div, span, button, [role=menuitem], [role=button], [role=tab]"

// rowActionSelector 是 clickInRow 在「行」内部查找可点元素的候选标签。
// 刻意不含 div:钉钉 /permission 行的「立即开通」外层也有一个 div.premium_apply_wrap,
// 文本同样是「立即开通」,但 React 事件绑在内层 span 上,点 div 不会触发开通。
const rowActionSelector = "span, button, [role=button], a"

// ---- 格式校验(纯函数,单测覆盖) ----

var (
	reFeishuAppID  = regexp.MustCompile(`^cli_[A-Za-z0-9]+$`)
	reFeishuSecret = regexp.MustCompile(`^[A-Za-z0-9]{16,}$`)
	reDingKey      = regexp.MustCompile(`^ding[A-Za-z0-9]+$`)
	reDingSecret   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	reWeComID      = regexp.MustCompile(`^[A-Za-z0-9_-]{6,}$`)
	reWeComSecret  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	reTencentToken = regexp.MustCompile(`^[A-Za-z0-9_.-]{20,}$`)
)

func nonEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("不能为空")
	}
	return nil
}

func match(re *regexp.Regexp, hint string) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return errors.New("不能为空")
		}
		if !re.MatchString(s) {
			return errors.New(hint)
		}
		return nil
	}
}

// Portals 四平台接入清单;历史踩坑(钉钉大小写、飞书长连接等)已固化进说明。
var Portals = map[string]*Portal{
	"feishu": {
		Name:  "feishu",
		Title: "飞书开放平台(企业自建应用)",
		URL:   "https://open.feishu.cn/app",
		Steps: []Step{
			{ID: "login", Brief: "打开开放平台并登录(扫码/手机号)", Detail: []string{
				"访问 https://open.feishu.cn/app",
				"登录方式:扫码或手机号+验证码(此步必须人工,自动化不碰登录)",
			}, URLs: []string{"https://open.feishu.cn/app"}},
			{ID: "create-app", Brief: "创建企业自建应用:填名称/描述/图标", Detail: []string{
				"点击「创建企业自建应用」,填写应用名称(如 mcp-hub)、描述、上传图标",
				"Agent 可通过 hub.onboard.create_app 代填并提交(需显式确认),图标沿用平台默认",
			}, URLs: []string{"https://open.feishu.cn/app"}, Browser: &BrowserAction{
				Script: "feishu@2026-11-create",
				// 选择器策略:按钮按可见文本匹配,输入框用 CSS 多候选(placeholder/role/class)。
				// 飞书开放平台是 React SPA,class 随改版变化;纯 data-test 钩子不可靠,
				// 因此不绑定单一选择器。任一步未命中即降级人工,绝不静默重试提交。
				Ops: []Op{
					{Kind: "navigate", Target: "https://open.feishu.cn/app"},
					{Kind: "waitVisible", Target: ".data-test__create-app-button, button, a, [role=button]"},
					{Kind: "click", Target: ".data-test__create-app-button, button, a, [role=button]",
						Match: "创建企业自建应用|创建应用"},
					{Kind: "waitVisible", Target: "input[placeholder*='名称'], [role=dialog] input, [class*=modal] input, [class*=dialog] input"},
					{Kind: "fill", Target: "input[placeholder*='名称'], [role=dialog] input[type=text], [class*=modal] input[type=text], [class*=dialog] input[type=text]",
						Text: "$app_name"},
					// 提交按钮必须整串匹配,避免误命中列表页的「创建企业自建应用」入口
					{Kind: "click", Target: "[role=dialog] button, [class*=modal] button, [class*=dialog] button, button",
						Match: "^\\s*(确定|确认|提交|立即创建|创建应用|创建)\\s*$"},
					{Kind: "waitURL", Target: "https://open.feishu.cn/app/",
						URLExcludes: []string{"/app/list", "/app/create", "/app/search"}},
				},
			}},
			{ID: "enable-perms", Brief: "开权限(与 Hub 功能一一对应)", Detail: []string{
				"权限管理 开通(按需):",
				"  im:message            → 发消息/回执(M1/M2 必需)",
				"  im:chat                → 群列表(P0-3 hub.chats)",
				"  bitable:app            → #记录 写入多维表格(M2 必需)",
				"事件与回调 → 事件配置 → 订阅方式选「使用长连接接收」(历史踩坑:别选 webhook)",
			}},
			{ID: "publish", Brief: "创建版本并发布(企业自建通常即时通过)", Detail: []string{
				"应用发布 → 版本管理与发布 → 创建版本 → 申请发布",
				"若提示需管理员审批:找企业管理员在后台通过(此步无法自动化)",
			}},
			{ID: "collect-creds", Brief: "复制凭证基础信息中的 App ID / App Secret", Cred: true,
				URLs: []string{"https://open.feishu.cn/app"},
				// 2026-11 实测校准:详情页 baseinfo 的 App ID 明文在 .auth-info__appid .secret-code__code;
				// Secret 按钮组 hover 才展开,点同 class 第 2 个按钮(第 1 个是复制)切换明文,抓完再点一次收回;
				// readText 自带掩码重试。应用入口不自动点(用户可能多应用):截图后等用户点,waitURL 切到详情标签。
				Browser: &BrowserAction{
					Script: "feishu@2026-11",
					Ops: []Op{
						// 下面四步只在「应用列表页等用户点进应用」的向导流程里执行;
						// capture 在详情页调用时必须跳过,否则 waitVisible(app-card) 永远等不到。
						{Kind: "navigate", Target: "https://open.feishu.cn/app", Scope: "list"},
						{Kind: "waitVisible", Target: "[class*='app-card']", Scope: "list"},
						{Kind: "screenshot", Target: "feishu-02-list.png", Scope: "list"},
						{Kind: "waitURL", Target: "https://open.feishu.cn/app/", Scope: "list",
							// 只认 /app/<appId>/... 详情标签:排除列表页/创建页/分页参数等假命中
							URLExcludes: []string{"/app/list", "/app/create", "/app/search"}},
						{Kind: "waitVisible", Target: ".auth-info__appid .secret-code__code"},
						{Kind: "readText", Target: ".auth-info__appid .secret-code__code", ExtractKey: "FEISHU_APP_ID"},
						{Kind: "hover", Target: ".auth-info__secret"},                             // 按钮组 hover 展开
						{Kind: "click", Target: ".auth-info__secret .secret-code__btn", Index: 1}, // 第 2 个=显示
						{Kind: "readText", Target: ".auth-info__secret .secret-code__code", ExtractKey: "FEISHU_APP_SECRET"},
						{Kind: "click", Target: ".auth-info__secret .secret-code__btn", Index: 1}, // 抓完收回隐藏
						{Kind: "screenshot", Target: "feishu-03-done.png"},
					},
				}},
		},
		Flow:       "app_console",
		CredPages:  []string{"/app/"},
		LoginHosts: []string{"accounts.feishu.cn"},
		Perms:      []string{"im:message", "im:chat", "bitable:app"},
		Validate:   ValidateFeishu,
	},
	"dingtalk": {
		Name:  "dingtalk",
		Title: "钉钉开放平台(企业内部应用)",
		URL:   "https://open-dev.dingtalk.com/",
		Steps: []Step{
			{ID: "login", Brief: "打开开放平台并登录(扫码)", URLs: []string{"https://open-dev.dingtalk.com/"}},
			{ID: "create-app", Brief: "应用开发 → 创建企业内部应用(H5/小程序均可)", Detail: []string{
				"进入「应用开发」→「企业内部开发」→ 创建应用",
				"左侧「机器人配置」:消息接收模式选 Stream 模式(免公网)",
			}, Browser: &BrowserAction{
				Script: "dingtalk@2026-11-create",
				// 2026-11-11 真机实测:列表页 /fe/app?...#/corp/app 的按钮文本是
				// 「创建应用」(BUTTON);入口页的「创建快捷应用」是另一种智能体应用,
				// 宣传卡片的「立即创建」也不是企业内部应用,两者都不能误点。
				// 下面的填表/提交 Ops 尚未真机校准(创建表单未打开过),命中失败即
				// selector_miss 降级,绝不重试提交。
				Ops: []Op{
					{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*创建应用\s*$`},
					{Kind: "click", Target: menuCandidateSelector, Match: `^\s*创建应用\s*$`},
					{Kind: "fill", Target: "input[placeholder*='名称'], [role=dialog] input[type=text], input[type=text]", Text: "$app_name"},
					{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(确定|确认|提交|创建)\\s*$"},
				},
			}},
			{ID: "enable-perms", Brief: "权限管理:按权限点 code 只读核对开通状态", Detail: []string{
				"真机实测:页面按 Contact.User.mobile / Calendar.Event.Write 等英文 code 列权限,状态为「未开通 / 已开通 / 立即开通」",
				"Hub 的钉钉能力只依赖 Client ID/Secret + Stream 长连接,不依赖这些 API 权限;不自动逐个开通",
			}},
			// 2026-11-11 真机实测 /baseinfo 页:
			//   "应用凭证 App ID : <uuid> 原企业内部应用AgentId <num>
			//    Client ID (原 AppKey 和 SuiteKey) <ding...>
			//    Client Secret (原 AppSecret 和 SuiteSecret) 当前使用:xxxx****xxxx"
			// Client ID 明文可读;Client Secret **渲染成掩码**,但同一行右侧有
			// aria-label=复制 的图标。2026-11-12 用户手工点该图标确认写入剪贴板的是
			// **完整明文** —— 旧注释/文案把「渲染成掩码」误写成「平台不提供明文」,
			// 且从没人点过复制按钮。现由 copyToCred 自动点复制并读回明文;
			// 该自动路径尚未真机端到端验证,失败按 selector_miss 降级 set_credentials。
			{ID: "collect-creds", Brief: "凭证与基础信息:读 Client ID / 复制 Client Secret 明文", Cred: true,
				URLs: []string{"https://open-dev.dingtalk.com/"},
				Browser: &BrowserAction{
					Script: "dingtalk@2026-11-12-creds",
					Ops: []Op{
						// 点进应用默认停在 /permission;先切到 /baseinfo 再读。
						{Kind: "waitVisible", Target: menuCandidateSelector, Match: `^\s*凭证与基础信息\s*$`},
						{Kind: "click", Target: menuCandidateSelector, Match: `^\s*凭证与基础信息\s*$`},
						{Kind: "waitVisible", Target: "body", Match: `应用凭证|Client\s*ID`},
						{Kind: "readPage"},
						{Kind: "extract", Target: `(?is)Client\s*ID[\s\S]{0,200}?\b(ding[A-Za-z0-9]{6,})\b`, ExtractKey: "DINGTALK_Client_ID"},
						// 页面渲染的是掩码,复制图标给出的才是明文。Target 用 td/.app-info-item
						// 缩小范围,Match 只匹配 Client Secret 块,避免误点 Client ID 的复制按钮。
						{Kind: "copyToCred", Target: "td, .app-info-item, div", Match: `Client\s*Secret`,
							Text: `[role="button"], button`, ExtractKey: "DINGTALK_Client_Secret"},
					},
				}},
		},
		Flow: "app_console",
		// 2026-11-11 真机实测的三段 URL:
		//   入口页 https://open-dev.dingtalk.com/?hash=%23%2F#/
		//   列表页 https://open-dev.dingtalk.com/fe/app?hash=%23%2Fcorp%2Fapp#/corp/app
		//   详情页 https://open-dev.dingtalk.com/fe/ai?...#/app/<uuid>/dev
		// 旧的 CredPages 里宽泛的 "/app/" 会把别的平台详情页串进来,改用实测
		// 的 /fe/ai 前缀 + #/app/<id> 由 AppIDRe 精确识别。
		CredPages:  []string{"/fe/ai", "appId=", "/detail", "/baseinfo"},
		LoginHosts: []string{"login.dingtalk.com", "accounts.dingtalk.com", "oapi.dingtalk.com"},
		AppIDRe:    regexp.MustCompile(`(?:#/app/|(?:appId|app_id|clientId)=)([A-Za-z0-9_-]{6,})`),
		// 真机 /permission 页的权限点是英文 code,不是飞书式中文标签。
		// Perms 用于只读幂等比对;ApplyPerms 才是 apply 阶段会点的「立即开通」行:
		// 真机 dump 里 snsapi_base 是「无需审批 已开通 默认开通」,该行没有
		// 「立即开通」按钮,若把它放进 ApplyPerms 必然 selector_miss。
		Perms:      []string{"Contact.User.Read", "Calendar.Event.Read", "snsapi_base"},
		ApplyPerms: []string{"Contact.User.Read", "Calendar.Event.Read"},
		// 列表页文本特征:「创建应用」按钮 +「全部应用」筛选 +「全部分组」。
		// 已有应用渲染为 <tr data-row-key><h5 title="应用名">,不是 a[href]。
		AppListURL:         "https://open-dev.dingtalk.com/fe/app?hash=%23%2Fcorp%2Fapp#/corp/app",
		AppListMarkers:     []string{"创建应用", "全部应用", "全部分组"},
		AppListRowSelector: "tr[data-row-key] h5[title]",
		// 详情页三个 tab 实测 hash:/baseinfo(凭证) /permission(权限) /version(版本)。
		CredTab:         "凭证与基础信息",
		CredTabFragment: "/baseinfo",
		Validate:        ValidateDingTalk,
	},
	"wecom": {
		Name:  "wecom",
		Title: "企业微信(智能机器人)",
		URL:   "https://work.weixin.qq.com/",
		Steps: []Step{
			{ID: "login", Brief: "管理员登录企业微信后台", URLs: []string{"https://work.weixin.qq.com/"}},
			{ID: "create-bot", Brief: "智能专区 → 智能机器人 → 创建(收发一体,长连接)", Detail: []string{
				"创建后把机器人拉进目标群(此步人工)",
				"备选:群机器人 webhook 只能发不能收,不推荐",
			}, Browser: &BrowserAction{
				Script: "wecom@2026-11-create",
				Ops: []Op{
					{Kind: "waitVisible", Target: menuCandidateSelector, Match: "^\\s*(创建机器人|创建智能机器人|新建机器人)\\s*$"},
					{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(创建机器人|创建智能机器人|新建机器人)\\s*$"},
					{Kind: "fill", Target: "input[placeholder*='名称'], [role=dialog] input[type=text], input[type=text]", Text: "$app_name"},
					{Kind: "click", Target: menuCandidateSelector, Match: "^\\s*(确定|确认|提交|创建)\\s*$"},
				},
			}},
			{ID: "collect-creds", Brief: "复制 Bot ID / Secret", Cred: true,
				Browser: &BrowserAction{
					Script: "wecom@2026-11-creds",
					Ops: []Op{
						{Kind: "readPage"},
						{Kind: "extract", Target: `(?i)(?:Bot\s*ID|BotID|机器人\s*ID)[^\n]{0,80}?([A-Za-z0-9_-]{6,})`, ExtractKey: "WECOM_BOT_ID"},
						{Kind: "extract", Target: `(?i)(?:Secret|密钥)[^\n]{0,80}?([A-Za-z0-9_-]{16,})`, ExtractKey: "WECOM_BOT_SECRET"},
					},
				}},
		},
		Flow:       "app_console",
		CredPages:  []string{"/robot", "botid=", "/detail", "/baseinfo"},
		LoginHosts: []string{"login.work.weixin.qq.com", "account.weixin.qq.com"},
		AppIDRe:    regexp.MustCompile(`(?:botid|robot_id|robotid)=([A-Za-z0-9_-]{6,})`),
		Perms:      []string{"收发消息"},
		Validate:   ValidateWeCom,
	},
	"tencent-docs": {
		Name:  "tencent-docs",
		Title: "腾讯文档开放平台(MCP)",
		URL:   "https://docs.qq.com/open/auth/mcp.html",
		Steps: []Step{
			{ID: "login", Brief: "打开授权页并登录(此步只能人工,Token 即登录态)", URLs: []string{"https://docs.qq.com/open/auth/mcp.html"}},
			// 腾讯文档没有「应用/版本」概念,Token 直接展示在授权页;
			// 因此该平台从登录到写盘可以完全无人工点击(除登录本身)。
			{ID: "collect-creds", Brief: "复制页面上的 MCP Token", Cred: true,
				Browser: &BrowserAction{
					Script: "tencent-docs@2026-11-creds",
					Ops: []Op{
						{Kind: "readPage"},
						{Kind: "extract", Target: `(?i)(?:Token|令牌|密钥)[^\n]{0,80}?([A-Za-z0-9_.-]{20,})`, ExtractKey: "TENCENT_DOCS_TOKEN"},
					},
				}},
		},
		Flow:       "token_page",
		CredPages:  []string{"https://docs.qq.com/open/auth/mcp.html"},
		LoginHosts: []string{"login.docs.qq.com", "ssl.ptlogin2.qq.com", "accounts.qq.com"},
		Perms:      []string{},
		Validate:   ValidateTencentDocs,
	},
}

// PortalCreds 每个平台要收集的凭据列表(顺序即向导输入顺序)。
var PortalCreds = map[string][]Cred{
	"feishu": {
		{EnvKey: "FEISHU_APP_ID", Label: "App ID", Format: match(reFeishuAppID, "飞书 App ID 形如 cli_xxxxxxxx")},
		{EnvKey: "FEISHU_APP_SECRET", Label: "App Secret", Format: match(reFeishuSecret, "Secret 为 16 位以上字母数字")},
	},
	"dingtalk": {
		{EnvKey: "DINGTALK_Client_ID", Label: "AppKey (注意:此变量名大小写敏感,官方 MCP 要求)", Format: match(reDingKey, "AppKey 形如 dingxxxxxxxx")},
		{EnvKey: "DINGTALK_Client_Secret", Label: "AppSecret", Format: match(reDingSecret, "Secret 为 16 位以上")},
	},
	"wecom": {
		{EnvKey: "WECOM_BOT_ID", Label: "Bot ID", Format: match(reWeComID, "Bot ID 为 6 位以上")},
		{EnvKey: "WECOM_BOT_SECRET", Label: "Secret", Format: match(reWeComSecret, "Secret 为 16 位以上")},
	},
	"tencent-docs": {
		{EnvKey: "TENCENT_DOCS_TOKEN", Label: "MCP Token", Format: match(reTencentToken, "Token 为 20 位以上")},
	},
}

// FindPortal 按名称取清单。
func FindPortal(name string) (*Portal, bool) {
	p, ok := Portals[name]
	return p, ok
}
