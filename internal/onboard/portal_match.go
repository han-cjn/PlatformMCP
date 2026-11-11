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

// portalDetailMatch 统一判断某个标签 URL 是否是该平台的「凭据/应用详情页」。
//
// 判定优先级:
//  1. token_page(腾讯文档):授权页本身就是凭据页,按 CredPages 前缀命中;
//  2. 带 AppIDRe 的平台(钉钉/企微):URL 里出现 appId=/botid= 即为详情;
//  3. 所有平台:CredPages 里的 URL 片段(如 /app/、/robot)命中即为详情;
//  4. 仅无 AppIDRe 的平台(飞书)保留实测的 /app/<appId>/... host 级兜底。
//
// 真实后台 URL 未实测的平台一律走 2/3 的窄匹配,宽匹配只会让 capture 白读
// 一次并降级 set_credentials,不会误提交任何表单。
func portalDetailMatch(p *Portal, raw string) bool {
	if p == nil || raw == "" || containsExcluded(raw) {
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
