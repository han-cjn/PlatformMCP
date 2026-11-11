package onboard

import "strings"

// portalExcludes 是开放平台的功能页/登录页路径,不能当成应用详情页。
// 旧版把这段数组散落在 watch/capture/create_app/focusDetailPage 四处,
// 新增平台时漏一处就会误判,统一收敛到这里。
var portalExcludes = []string{"/app/list", "/app/create", "/app/search", "/login"}

func containsExcluded(raw string) bool {
	for _, ex := range portalExcludes {
		if strings.Contains(raw, ex) {
			return true
		}
	}
	return false
}

// platformHostAllowlist 是各平台允许参与 URL 判定的域后缀白名单。
// 与 portalExcludes 同属「平台 URL 判定」的内部规则,集中放在这里。
// 必须写主域(feishu.cn / dingtalk.com / weixin.qq.com / docs.qq.com),
// 不能写 qq.com 这类公共后缀——否则企微与腾讯文档会互相串台。
// Portal.URL / LoginHosts / CredPages 会在 portalPageBelongs 里一并合并。
var platformHostAllowlist = map[string][]string{
	"feishu":       {"feishu.cn"},
	"dingtalk":     {"dingtalk.com"},
	"wecom":        {"weixin.qq.com"},
	"tencent-docs": {"docs.qq.com"},
}

func hostInAllowlist(host, allow string) bool {
	if host == "" || allow == "" {
		return false
	}
	host, allow = strings.ToLower(host), strings.ToLower(allow)
	return host == allow || strings.HasSuffix(host, "."+allow)
}

// portalPageBelongs 判断标签 URL 是否属于目标平台的域生态。
//
// 背景(2026-11-11 实测):调试 Chrome 的持久化 profile 会长期留下上一次接入
// 的标签。飞书详情页 https://open.feishu.cn/app/cli_xxx/baseinfo 同时含
// "/app/" 与 "/baseinfo",正好命中钉钉 CredPages,于是 Watch("dingtalk")
// 把它当成钉钉详情页 → start 提前 ready_to_capture 且不开钉钉页面 →
// setup 的 app_url 指向飞书,后续 Ops 全在飞书页面上按钉钉声明执行。
// 因此所有 URL 判定必须先过域隔离,不能只看路径片段。
func portalPageBelongs(p *Portal, raw string) bool {
	if p == nil || raw == "" {
		return false
	}
	h := hostOf(raw)
	if h == "" {
		return false
	}
	if hostInAllowlist(h, hostOf(p.URL)) {
		return true
	}
	for _, allow := range platformHostAllowlist[p.Name] {
		if hostInAllowlist(h, allow) {
			return true
		}
	}
	for _, lh := range p.LoginHosts {
		if hostInAllowlist(h, lh) {
			return true
		}
	}
	for _, frag := range p.CredPages {
		if strings.HasPrefix(frag, "http") && hostInAllowlist(h, hostOf(frag)) {
			return true
		}
	}
	return false
}

// portalDetailMatch 统一判断某个标签 URL 是否是该平台的「凭据/应用详情页」。
//
// 判定优先级:
//  0. 域隔离:不属于该平台生态的标签一律不算(防止跨平台残留标签串页);
//  1. token_page(腾讯文档):授权页本身就是凭据页,按 CredPages 前缀命中;
//  2. 带 AppIDRe 的平台(钉钉/企微):URL 里出现 appId=/botid= 即为详情;
//  3. 所有平台:CredPages 里的 URL 片段(如 /app/、/robot)命中即为详情;
//  4. 仅无 AppIDRe 的平台(飞书)保留实测的 /app/<appId>/... host 级兜底。
//
// 真实后台 URL 未实测的平台一律走 2/3 的窄匹配,宽匹配只会让 capture 白读
// 一次并降级 set_credentials,不会误提交任何表单。
func portalDetailMatch(p *Portal, raw string) bool {
	if p == nil || raw == "" || containsExcluded(raw) || !portalPageBelongs(p, raw) {
		return false
	}
	if p.Flow == "token_page" {
		for _, pre := range p.CredPages {
			if strings.HasPrefix(raw, pre) {
				return true
			}
		}
		return strings.HasPrefix(raw, p.URL)
	}
	if p.AppIDRe != nil {
		if p.AppIDRe.MatchString(raw) {
			return true
		}
		for _, frag := range p.CredPages {
			if strings.Contains(raw, frag) {
				return true
			}
		}
		return false
	}
	for _, frag := range p.CredPages {
		if strings.Contains(raw, frag) {
			return true
		}
	}
	if !matchURLPrefix(raw, p.URL, portalExcludes) {
		return false
	}
	return strings.Contains(strings.TrimPrefix(raw, p.URL), "/")
}

// portalAppID 抽取 URL 里的应用/机器人 ID,用于幂等判断与列表探测。
func portalAppID(p *Portal, raw string) string {
	if p != nil && p.AppIDRe != nil {
		if m := p.AppIDRe.FindStringSubmatch(raw); len(m) > 1 {
			return m[1]
		}
	}
	return appIDFromURL(raw)
}

// portalIsLoginPage 在默认 accounts.* / 路径含 login 之外,补充各平台自己的
// 登录域(钉钉 login.dingtalk.com、企微 login.work.weixin.qq.com、QQ 登录等)。
func portalIsLoginPage(p *Portal, raw string) bool {
	host := hostOf(raw)
	if p != nil {
		for _, h := range p.LoginHosts {
			if h != "" && strings.Contains(host, h) {
				return true
			}
		}
	}
	return isLoginPage(raw)
}
