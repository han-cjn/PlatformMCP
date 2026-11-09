package onboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// PermProbeResult 权限 API 探针的结果。
//
// 飞书没有公开的「查询应用已开通 scope 列表」接口,所以这里只做能力级只读探针
// (群列表),并如实区分 Checked / Unverified:不把「一个 API 调通了」等同于
// 「三个 scope 全部已开通」。gate 状态只标记已验证的部分。
type PermProbeResult struct {
	Status     string   `json:"status"` // api_ready / needs_perms / unknown / no_credentials / unsupported
	Checked    []string `json:"checked"`
	Unverified []string `json:"unverified"`
	Detail     string   `json:"detail,omitempty"`
}

// 只读探针端点做成包级变量,单测可替换为 httptest.Server。
var feishuChatsProbeURL = "https://open.feishu.cn/open-apis/im/v1/chats?page_size=1"

// probePerms 可注入,单测替换为 stub 避免真实网络调用。
var probePerms = ProbePerms

func allPermsUnverified() []string {
	return append([]string(nil), defaultPerms...)
}

// ProbePerms 用平台 API 判断权限是否可用:零副作用、不打开浏览器、不点任何按钮。
//
// 设计取舍:只有 needs_perms 才允许进入 UI 开通流程。api_ready 表示能力已可用,
// 跳过 UI 避免重复开通;unknown/no_credentials 无法确认,同样跳过 UI——
// 在权限可能已开通时盲点「开通权限」会产生不可撤销的企业记录。
func ProbePerms(platform string) PermProbeResult {
	if platform != "feishu" {
		return PermProbeResult{Status: "unsupported", Unverified: allPermsUnverified(),
			Detail: "当前仅 feishu 定义了只读权限探针"}
	}
	id := strings.TrimSpace(os.Getenv("FEISHU_APP_ID"))
	secret := strings.TrimSpace(os.Getenv("FEISHU_APP_SECRET"))
	if id == "" || secret == "" {
		return PermProbeResult{Status: "no_credentials", Unverified: allPermsUnverified(),
			Detail: "进程内尚无飞书凭据,先完成 capture 或重启 Hub 加载 .env"}
	}
	token, err := feishuTenantToken(id, secret)
	if err != nil {
		return PermProbeResult{Status: "unknown", Unverified: allPermsUnverified(),
			Detail: "换取 tenant_access_token 失败: " + err.Error()}
	}
	req, err := http.NewRequest(http.MethodGet, feishuChatsProbeURL, nil)
	if err != nil {
		return PermProbeResult{Status: "unknown", Unverified: allPermsUnverified(), Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return PermProbeResult{Status: "unknown", Unverified: allPermsUnverified(),
			Detail: "权限探针请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	detail := fmt.Sprintf("GET /open-apis/im/v1/chats: http=%d code=%d msg=%s", resp.StatusCode, out.Code, out.Msg)
	switch classifyPermResponse(resp.StatusCode, out.Code, out.Msg) {
	case "ok":
		return PermProbeResult{
			Status:     "api_ready",
			Checked:    []string{"im:chat(群列表只读可用)"},
			Unverified: []string{"im:message(发消息)", "bitable:app(多维表格)"},
			Detail:     detail,
		}
	case "needs_perms":
		return PermProbeResult{Status: "needs_perms", Unverified: allPermsUnverified(), Detail: detail}
	default:
		return PermProbeResult{Status: "unknown", Unverified: allPermsUnverified(),
			Detail: detail + ";无法据此判断权限状态,不进入 UI 开通流程"}
	}
}

// feishuTenantToken 用 app_id/secret 换 tenant_access_token(复用 validate.go 的端点变量)。
func feishuTenantToken(id, secret string) (string, error) {
	body, _ := json.Marshal(map[string]string{"app_id": id, "app_secret": secret})
	resp, err := httpClient.Post(feishuTokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Token string `json:"tenant_access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Code != 0 || out.Token == "" {
		return "", fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Token, nil
}

// classifyPermResponse 把响应归为 ok / needs_perms / unknown。
// 平台权限错误码会变,因此以可读 msg 关键词为主、HTTP 状态为辅,不硬编码数字码。
func classifyPermResponse(httpStatus, code int, msg string) string {
	if httpStatus >= 200 && httpStatus < 300 && code == 0 {
		return "ok"
	}
	low := strings.ToLower(msg)
	if httpStatus == http.StatusForbidden || httpStatus == http.StatusUnauthorized ||
		strings.Contains(msg, "权限") || strings.Contains(low, "permission") ||
		strings.Contains(low, "scope") || strings.Contains(low, "no access") ||
		strings.Contains(low, "forbidden") {
		return "needs_perms"
	}
	return "unknown"
}
