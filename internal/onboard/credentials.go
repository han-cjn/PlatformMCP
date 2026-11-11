package onboard

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// CredField 描述一个待收集的凭据字段。Agent 用它渲染表单/选择框,
// 字段名必须与 PortalCreds 一致;这里绝不返回值,只返回键与中文标签。
type CredField struct {
	EnvKey string `json:"env_key"`
	Label  string `json:"label"`
}

// CredentialFields 返回某平台必须收集的凭据字段清单。
func CredentialFields(platform string) []CredField {
	defs := PortalCreds[platform]
	out := make([]CredField, 0, len(defs))
	for _, c := range defs {
		out = append(out, CredField{EnvKey: c.EnvKey, Label: c.Label})
	}
	return out
}

// CredError 把凭据收尾失败分成可机器判断的阶段,供 Capture / SetCredentials
// 映射到各自的 phase,而不是只给一句人读的错误文本。
type CredError struct {
	Phase   string
	Message string
}

func (e *CredError) Error() string { return e.Phase + ": " + e.Message }

// CredOptions 注入真连校验、写盘路径与热重载回调。
// Validate/Reload 允许单测替换,SkipSetEnv 让单测不污染进程环境。
type CredOptions struct {
	EnvPath    string
	Validate   func(map[string]string) error
	Reload     func(map[string]string) error
	SkipSetEnv bool
}

// commitCredentials 是所有接入路径共用的收尾:
// 缺值/格式校验 → 平台真连校验 → 写 .env → 更新进程环境 → 热重载挂载。
//
// 安全边界:真连校验失败一律不写盘(避免把错误凭据固化),写盘成功后
// 即使热重载失败也如实返回 reload_failed,不假装整体完成。
func commitCredentials(platform string, values map[string]string, opts CredOptions) ([]string, *CredError) {
	p, ok := FindPortal(platform)
	if !ok {
		return nil, &CredError{"unsupported", "未知平台 " + platform}
	}
	env := map[string]string{}
	collected := make([]string, 0, len(PortalCreds[platform]))
	for _, c := range PortalCreds[platform] {
		v := strings.TrimSpace(values[c.EnvKey])
		if v == "" {
			return nil, &CredError{"incomplete", "缺少 " + c.EnvKey + "(" + c.Label + ")"}
		}
		if c.Format != nil {
			if err := c.Format(v); err != nil {
				return nil, &CredError{"invalid_format", c.EnvKey + " 格式不符: " + err.Error()}
			}
		}
		env[c.EnvKey] = v
		collected = append(collected, c.EnvKey)
	}
	sort.Strings(collected)

	validate := opts.Validate
	if validate == nil {
		validate = p.Validate
	}
	if validate == nil {
		return nil, &CredError{"verify_failed", "该平台未定义真连校验,拒绝写入未经校验的凭据"}
	}
	if err := validate(env); err != nil {
		return nil, &CredError{"verify_failed", err.Error() + ";未写入 .env"}
	}

	path := opts.EnvPath
	if path == "" {
		path = ".env"
	}
	if err := UpsertEnv(path, env); err != nil {
		return nil, &CredError{"write_failed", err.Error()}
	}
	if !opts.SkipSetEnv {
		for k, v := range env {
			os.Setenv(k, v) // 让本进程后续逻辑与热重载直接用新值
		}
	}
	if opts.Reload != nil {
		if err := opts.Reload(env); err != nil {
			return nil, &CredError{"reload_failed", err.Error()}
		}
	}
	return collected, nil
}

// SetCredentialsResult 是 hub.onboard.set_credentials 的返回:零浏览器依赖,
// 四平台通吃;Required 只在缺值时返回,供 Agent 生成表单。
type SetCredentialsResult struct {
	Platform  string      `json:"platform"`
	Phase     string      `json:"phase"` // done / incomplete / invalid_format / verify_failed / write_failed / reload_failed / unsupported
	Collected []string    `json:"collected"`
	Required  []CredField `json:"required,omitempty"`
	Message   string      `json:"message,omitempty"`
	Next      string      `json:"next"`
}

// SetCredentials 让 Agent 把用户从平台后台复制的凭据直接交给 Hub 完成接入。
//
// 与 Capture 的区别:Capture 从浏览器页面读值(需要 DOM 选择器且未实测的平台
// 会降级),SetCredentials 由 Agent 用表单收集值,不需要任何浏览器动作,
// 因此对所有定义了 PortalCreds + Validate 的平台都可靠可用。
func SetCredentials(platform string, values map[string]string, opts CredOptions) (*SetCredentialsResult, error) {
	if _, ok := FindPortal(platform); !ok {
		return nil, fmt.Errorf("未知平台 %q,可选: %s", platform, strings.Join(PlatformOrder(), " / "))
	}
	res := &SetCredentialsResult{
		Platform:  platform,
		Collected: []string{},
		Required:  CredentialFields(platform),
	}
	collected, ce := commitCredentials(platform, values, opts)
	if ce != nil {
		res.Phase = ce.Phase
		res.Message = ce.Message
		res.Next = credentialNext(ce.Phase)
		return res, nil
	}
	res.Phase = "done"
	res.Collected = collected
	res.Required = nil
	res.Next = "凭据已写入 .env 并完成热重载;调用 hub.health 确认 credentials=true / mounted=true。" +
		"若该平台还有权限/发布环节,可继续调用 hub.onboard.setup;全程无需在终端执行任何命令"
	return res, nil
}

func credentialNext(phase string) string {
	switch phase {
	case "incomplete":
		return "按 required 逐项收集凭据值(可用表单/选择框让用户粘贴),然后带 values 重新调用 hub.onboard.set_credentials;不要在回复里回显凭据内容"
	case "invalid_format":
		return "凭据格式不符合平台要求,请让用户重新复制完整值后重试;未写入 .env"
	case "verify_failed":
		return "真连校验未通过,凭据未写入 .env;核对平台、凭据是否属于同一应用/企业,或平台是否可达后重试。也可改走 hub.onboard.setup 自动抓取"
	case "write_failed":
		return "写 .env 失败,检查工作目录写权限后重试;未产生热重载"
	case "reload_failed":
		return "凭据已写入 .env,但热重载失败;在连接器页面禁用/启用 mcp-hub 即可生效"
	default:
		return "按 message 修正后重试,或调用 hub.onboard.setup 由浏览器自动抓取凭据"
	}
}
