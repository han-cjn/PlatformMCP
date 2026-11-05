package onboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	// 新建:键值写入 + 权限 0600
	if err := UpsertEnv(path, map[string]string{"FEISHU_APP_ID": "cli_test123", "FEISHU_APP_SECRET": "s1"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	if !strings.Contains(s, "FEISHU_APP_ID=cli_test123") {
		t.Fatalf("新建写入失败: %q", s)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600,实际 %v", fi.Mode().Perm())
	}

	// 合并:覆盖已存在键,保留注释与未知变量
	if err := os.WriteFile(path, []byte("# 注释\nFEISHU_APP_ID=old\nUNKNOWN_KEEP=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnv(path, map[string]string{"FEISHU_APP_ID": "cli_new", "NEW_KEY": "v"}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	s = string(raw)
	if !strings.Contains(s, "# 注释") || !strings.Contains(s, "UNKNOWN_KEEP=1") {
		t.Fatalf("注释/未知变量被破坏: %q", s)
	}
	if strings.Contains(s, "FEISHU_APP_ID=old") || !strings.Contains(s, "FEISHU_APP_ID=cli_new") || !strings.Contains(s, "NEW_KEY=v") {
		t.Fatalf("覆盖/追加失败: %q", s)
	}

	// 原子性:失败不留 .tmp 残留(目录只读触发 WriteFile 失败前先成功写 tmp 的场景难造,
	// 这里验证成功路径无 tmp 残留)
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("存在 .tmp 残留")
	}
}

func TestPortalDataComplete(t *testing.T) {
	want := []string{"feishu", "dingtalk", "wecom", "tencent-docs"}
	for _, name := range want {
		p, ok := FindPortal(name)
		if !ok {
			t.Fatalf("缺少平台清单 %s", name)
		}
		if len(p.Steps) == 0 || p.Validate == nil || p.URL == "" {
			t.Fatalf("%s 清单不完整: steps/validate/url 为空", name)
		}
		creds := PortalCreds[name]
		if len(creds) == 0 {
			t.Fatalf("%s 缺少凭据收集定义", name)
		}
		// 每个清单至少一个凭据步骤;格式校验器非空
		hasCredStep := false
		for _, s := range p.Steps {
			if s.Cred {
				hasCredStep = true
			}
		}
		if !hasCredStep {
			t.Fatalf("%s 无凭据收集步骤", name)
		}
		for _, c := range creds {
			if c.EnvKey == "" || c.Format == nil {
				t.Fatalf("%s 凭据定义不完整: %+v", name, c)
			}
		}
		// 登录步骤不允许有浏览器动作(合规红线:不自动化登录)
		for _, s := range p.Steps {
			if s.ID == "login" && s.Browser != nil {
				t.Fatalf("%s 的登录步骤配置了浏览器动作,违反合规边界", name)
			}
		}
	}
	// 飞书浏览器脚本存在(试点)
	if _, ok := Portals["feishu"]; ok {
		found := false
		for _, s := range Portals["feishu"].Steps {
			if s.Browser != nil && strings.HasPrefix(s.Browser.Script, "feishu@") {
				found = true
			}
		}
		if !found {
			t.Fatal("飞书缺少带版本标记的浏览器脚本")
		}
	}
}

func TestValidateFeishuMock(t *testing.T) {
	// 凭据错误:code != 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":99991663,"msg":"app secret invalid"}`))
	}))
	old := feishuTokenURL
	feishuTokenURL = srv.URL
	defer func() { feishuTokenURL = old; srv.Close() }()

	err := ValidateFeishu(map[string]string{"FEISHU_APP_ID": "cli_x", "FEISHU_APP_SECRET": "bad"})
	ve, ok := err.(*ValidateError)
	if !ok || ve.Kind != "credentials" {
		t.Fatalf("应为 credentials 错误,实际 %v", err)
	}

	// 成功路径
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t"}`))
	})
	if err := ValidateFeishu(map[string]string{"FEISHU_APP_ID": "cli_x", "FEISHU_APP_SECRET": "good"}); err != nil {
		t.Fatalf("有效凭据不应报错: %v", err)
	}
}

func TestValidateDingTalkMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid authentication"}`))
	}))
	old := dingTokenURL
	dingTokenURL = srv.URL
	defer func() { dingTokenURL = old; srv.Close() }()

	err := ValidateDingTalk(map[string]string{"DINGTALK_Client_ID": "dingx", "DINGTALK_Client_Secret": "bad"})
	ve, ok := err.(*ValidateError)
	if !ok || ve.Kind != "credentials" {
		t.Fatalf("应为 credentials 错误,实际 %v", err)
	}

	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"accessToken":"at","expireIn":7200}`))
	})
	if err := ValidateDingTalk(map[string]string{"DINGTALK_Client_ID": "dingx", "DINGTALK_Client_Secret": "good"}); err != nil {
		t.Fatalf("有效凭据不应报错: %v", err)
	}
}

func TestValidateTencentDocsMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"tencent-docs"}}}`))
	}))
	old := tencentMCPURL
	tencentMCPURL = srv.URL
	defer func() { tencentMCPURL = old; srv.Close() }()

	// 无 Token → 401 → credentials
	err := ValidateTencentDocs(map[string]string{"TENCENT_DOCS_TOKEN": ""})
	ve, ok := err.(*ValidateError)
	if !ok || ve.Kind != "credentials" {
		t.Fatalf("应为 credentials 错误,实际 %v", err)
	}
	// 有效 Token
	if err := ValidateTencentDocs(map[string]string{"TENCENT_DOCS_TOKEN": "tok"}); err != nil {
		t.Fatalf("有效凭据不应报错: %v", err)
	}
}

func TestMask(t *testing.T) {
	if Mask("cli_abcdefgh") != "cli_********(12字符)" {
		t.Fatalf("Mask 输出异常: %s", Mask("cli_abcdefgh"))
	}
	if Mask("ab") != "**" {
		t.Fatalf("短值应全遮蔽: %s", Mask("ab"))
	}
}

func TestBrowserActionUnknownOp(t *testing.T) {
	// 无需真实浏览器:未知 op 应返回明确错误而非 panic
	bx := &BrowserExecutor{}
	err := bx.RunAction(&BrowserAction{
		Script: "test@0",
		Ops:    []Op{{Kind: "no-such-op", Target: "x"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "未知操作类型") {
		t.Fatalf("未知 op 应报错: %v", err)
	}
}
