package onboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ValidateError 真连校验失败分类:
//   - credentials:凭据错误,重试无意义,需要重输
//   - network:平台不可达/故障,稍后重试可能恢复
type ValidateError struct {
	Kind string
	Msg  string
}

func (e *ValidateError) Error() string { return e.Kind + ": " + e.Msg }

// 校验端点做成包级变量,测试可替换为 mock。
var (
	feishuTokenURL = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	dingTokenURL   = "https://api.dingtalk.com/v1.0/oauth2/accessToken"
	tencentMCPURL  = "https://docs.qq.com/openapi/mcp"
	httpClient     = &http.Client{Timeout: 10 * time.Second}

	// 企微无公开 REST 校验 API:拉起适配器子进程,观察 WS 认证日志。
	// WecomAdapterPath 是相对路径,真连验证进程的工作目录决定能否找到适配器;
	// ValidateWeCom 显式把 cmd.Dir 设到模块根,不依赖调用方的工作目录
	// (测试从 internal/onboard 跑时,相对路径会解析到包内而找不到 server.js)。
	WecomAdapterCmd  = "node"
	WecomAdapterPath = "adapters/wecom-aibot-mcp/server.js"
)

// moduleRoot 返回 go.mod 所在目录(模块根),供相对路径资源定位。
// 从当前源文件向上回溯;测试与生产(仓库根)都能正确定位。
func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return "."
}

// ValidateFeishu 用 app_id/secret 换 tenant_access_token,code==0 即凭据有效。
func ValidateFeishu(env map[string]string) error {
	body, _ := json.Marshal(map[string]string{
		"app_id":     env["FEISHU_APP_ID"],
		"app_secret": env["FEISHU_APP_SECRET"],
	})
	resp, err := httpClient.Post(feishuTokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return &ValidateError{"network", "飞书平台不可达: " + err.Error()}
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Code != 0 {
		return &ValidateError{"credentials", fmt.Sprintf("飞书拒绝凭据 code=%d: %s", out.Code, out.Msg)}
	}
	return nil
}

// ValidateDingTalk appKey/appSecret 换 accessToken;非 200 视为凭据错误。
func ValidateDingTalk(env map[string]string) error {
	body, _ := json.Marshal(map[string]string{
		"appKey":    env["DINGTALK_Client_ID"],
		"appSecret": env["DINGTALK_Client_Secret"],
	})
	resp, err := httpClient.Post(dingTokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return &ValidateError{"network", "钉钉平台不可达: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &ValidateError{"credentials", fmt.Sprintf("钉钉拒绝凭据 http=%d: %s", resp.StatusCode, snippet(raw))}
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.AccessToken == "" {
		return &ValidateError{"credentials", "钉钉响应缺少 accessToken"}
	}
	return nil
}

// ValidateWeCom 拉起适配器子进程,10s 内 stderr 出现「长连接认证成功」即通过。
// 无官方校验 API,这是最接近真实可用性的验证(连不上≈凭据错误或平台不可达)。
func ValidateWeCom(env map[string]string) error {
	cmd := exec.Command(WecomAdapterCmd, WecomAdapterPath)
	// 适配器是相对路径:把子进程工作目录固定到模块根,与调用方工作目录解耦。
	cmd.Dir = moduleRoot()
	cmd.Env = append(os.Environ(),
		"WECOM_BOT_ID="+env["WECOM_BOT_ID"],
		"WECOM_BOT_SECRET="+env["WECOM_BOT_SECRET"],
		"HUB_EVENT_URL=http://127.0.0.1:1/events", // 失效地址,防止校验期间真发事件
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return &ValidateError{"network", "企微适配器启动失败: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return &ValidateError{"network", "企微适配器启动失败(检查 node 与路径): " + err.Error()}
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), "长连接认证成功") {
					done <- nil
					return
				}
			}
			if err != nil {
				done <- &ValidateError{"credentials", "企微 WS 未认证(适配器已退出): " + snippet([]byte(acc.String()))}
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		return &ValidateError{"credentials", "企微 WS 15s 内未完成认证(凭据错误或平台不可达)"}
	}
}

// ValidateTencentDocs 对远程 MCP 端点发 initialize,200 且含 result 即 Token 有效。
func ValidateTencentDocs(env map[string]string) error {
	initBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "hub-onboard", "version": "0"},
		},
	})
	req, err := http.NewRequest(http.MethodPost, tencentMCPURL, bytes.NewReader(initBody))
	if err != nil {
		return &ValidateError{"network", err.Error()}
	}
	req.Header.Set("Authorization", env["TENCENT_DOCS_TOKEN"])
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return &ValidateError{"network", "腾讯文档 MCP 不可达: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &ValidateError{"credentials", fmt.Sprintf("Token 被拒绝 http=%d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return &ValidateError{"network", fmt.Sprintf("http=%d: %s", resp.StatusCode, snippet(raw))}
	}
	if !strings.Contains(string(raw), "result") {
		return &ValidateError{"credentials", "MCP initialize 未返回 result(Token 可能失效)"}
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
