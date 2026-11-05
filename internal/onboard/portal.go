package onboard

import (
	"errors"
	"regexp"
	"strings"
)

// Step 接入引导的一个步骤,两种执行器共用。
type Step struct {
	ID      string          // 步骤标识,如 "create-app"
	Brief    string          // 终端一行说明
	Detail  []string         // 展开说明:具体点哪里、勾哪些权限
	URLs    []string        // 相关页面,手动模式下尝试 open
	Browser *BrowserAction  // 浏览器执行器动作;nil 表示该步必须人工(如登录/审批)
	Cred    bool            // true 表示此步骤要收集凭据(向导进入输入环节)
}

// Cred 单个凭据的收集与校验定义。
type Cred struct {
	EnvKey   string
	Label    string
	Format   func(string) error // 非空格式校验(前缀/长度)
}

// Portal 一个平台的完整接入清单。
type Portal struct {
	Name      string
	Title     string
	URL       string
	Steps     []Step
	Validate  func(env map[string]string) error // 真连校验(见 validate.go)
}

// BrowserAction 浏览器执行器动作序列;Script 为选择器版本标记(平台改版只改数据)。
type BrowserAction struct {
	Script string
	Ops    []Op
}

// Op 单个浏览器操作。Target 为 CSS 选择器;ExtractKey 表示把该元素文本
// 存入凭据收集结果(envKey)。
type Op struct {
	Kind         string // navigate / click / waitVisible / readText / fill / screenshot
	Target       string // CSS 选择器或 URL(navigate)
	Text         string // fill 的输入内容;支持 "$env:KEY" 引用已收集凭据
	ExtractKey   string // readText 抽取到的值存入该 envKey
}

// ---- 格式校验(纯函数,单测覆盖) ----

var (
	reFeishuAppID     = regexp.MustCompile(`^cli_[A-Za-z0-9]+$`)
	reFeishuSecret    = regexp.MustCompile(`^[A-Za-z0-9]{16,}$`)
	reDingKey         = regexp.MustCompile(`^ding[A-Za-z0-9]+$`)
	reDingSecret      = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	reWeComID         = regexp.MustCompile(`^[A-Za-z0-9_-]{6,}$`)
	reWeComSecret     = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	reTencentToken    = regexp.MustCompile(`^[A-Za-z0-9_.-]{20,}$`)
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
			}, URLs: []string{"https://open.feishu.cn/app"}, Browser: &BrowserAction{
				Script: "feishu@2026-11",
				Ops: []Op{
					{Kind: "navigate", Target: "https://open.feishu.cn/app"},
					{Kind: "waitVisible", Target: "button"},
					{Kind: "screenshot", Target: "feishu-01-portal.png"}, // 落到截图由人工点创建
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
				URLs: []string{"https://open.feishu.cn/app"}},
		},
		Validate: ValidateFeishu,
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
			}},
			{ID: "enable-perms", Brief: "权限:通讯录只读 + 消息推送 + 相关 SmartForm", Detail: []string{
				"历史踩坑:qyapi_addresslist_search 必须勾上,否则收不到群消息",
			}},
			{ID: "collect-creds", Brief: "凭证与基础信息:复制 AppKey / AppSecret", Cred: true,
				URLs: []string{"https://open-dev.dingtalk.com/"}},
		},
		Validate: ValidateDingTalk,
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
			}},
			{ID: "collect-creds", Brief: "复制 Bot ID / Secret", Cred: true},
		},
		Validate: ValidateWeCom,
	},
	"tencent-docs": {
		Name:  "tencent-docs",
		Title: "腾讯文档开放平台(MCP)",
		URL:   "https://docs.qq.com/open/auth/mcp.html",
		Steps: []Step{
			{ID: "login", Brief: "打开授权页并登录(此步只能人工,Token 即登录态)", URLs: []string{"https://docs.qq.com/open/auth/mcp.html"}},
			{ID: "collect-creds", Brief: "复制页面上的 MCP Token", Cred: true},
		},
		Validate: ValidateTencentDocs,
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
