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

// TestCloseNilSafe Close 需对 nil 执行器/已关的初始页幂等,不得 panic。
func TestCloseNilSafe(t *testing.T) {
	var bx *BrowserExecutor
	bx.Close() // 不应 panic

	empty := &BrowserExecutor{}
	empty.Close()
	empty.Close()
}

// TestMatchURLPrefix waitURL 前缀匹配:排除列表页自身与分页/搜索等假命中。
func TestMatchURLPrefix(t *testing.T) {
	const target = "https://open.feishu.cn/app/"
	cases := []struct {
		url      string
		excludes []string
		want     bool
	}{
		{"https://open.feishu.cn/app/", nil, false},                       // 列表页自身(不比 target 长)
		{"https://open.feishu.cn/app", nil, false},                        // 更短
		{"https://open.feishu.cn/app/cli_abc123/baseinfo", nil, true},     // 详情页
		{"https://open.feishu.cn/app/list", []string{"/app/list"}, false}, // 列表页排除
		{"https://open.feishu.cn/app/cli_x/baseinfo", []string{"/app/list"}, true},
		{"https://open.feishu.cn/app?page=2", []string{"/app/list"}, false}, // 不带斜杠,前缀不匹配
		{"https://other.feishu.cn/app/cli_x", nil, false},                   // 域名不同
		{"", nil, false},
	}
	for _, c := range cases {
		if got := matchURLPrefix(c.url, target, c.excludes); got != c.want {
			t.Errorf("matchURLPrefix(%q, excludes=%v) = %v, want %v", c.url, c.excludes, got, c.want)
		}
	}
}

// TestPickRejectsNegativeIndex Index<0 应返回错误而不是负索引 panic。
func TestPickRejectsNegativeIndex(t *testing.T) {
	bx := &BrowserExecutor{}
	if _, err := bx.pick(Op{Target: "div", Index: -1}); err == nil {
		t.Fatal("负 Index 应返回错误")
	}
	// 未初始化页面也要给出明确错误而非 nil deref
	if _, err := bx.pick(Op{Target: "div"}); err == nil {
		t.Fatal("nil page 应返回错误")
	}
}

// TestIsEphemeralProfileDir 临时 profile 判定:命中即拒绝,持久化目录放行。
func TestIsEphemeralProfileDir(t *testing.T) {
	cases := []struct {
		dir  string
		want bool
	}{
		{"/tmp/chrome-profile", true},
		{"/private/tmp/abc", true},
		{"/var/folders/x8/xyz/T/rod", true},
		{"/private/var/folders/x8/xyz/T/rod", true},
		{"/Users/hanyechao/.org.chromium.Chromium.abc123", true},
		{"/Users/hanyechao/.chrome-debug-profile", false},
		{"/Users/hanyechao/Library/Application Support/Google/Chrome", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isEphemeralProfileDir(c.dir); got != c.want {
			t.Errorf("isEphemeralProfileDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

// TestResolveWebSocketURLRejectsEphemeralProfile 临时 profile 应报错并带修复命令,
// 不能让代填脚本跑到一半才发现登录态是空的。
func TestResolveWebSocketURLRejectsEphemeralProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/x","userDataDir":"/var/folders/tmp/rod"}`))
	}))
	defer srv.Close()

	_, err := resolveWebSocketURL(srv.URL)
	if err == nil {
		t.Fatal("临时 profile 应返回错误")
	}
	if !strings.Contains(err.Error(), "临时 profile") || !strings.Contains(err.Error(), "user-data-dir") {
		t.Fatalf("错误信息应含诊断与修复命令,实际: %v", err)
	}
}

// TestResolveWebSocketURLAcceptsPersistentProfile 持久化 profile 正常返回 WS 地址。
func TestResolveWebSocketURLAcceptsPersistentProfile(t *testing.T) {
	const wantWS = "ws://127.0.0.1:9222/devtools/browser/ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"` + wantWS + `","userDataDir":"/Users/x/.chrome-debug-profile"}`))
	}))
	defer srv.Close()

	got, err := resolveWebSocketURL(srv.URL)
	if err != nil {
		t.Fatalf("持久化 profile 不应报错: %v", err)
	}
	if got != wantWS {
		t.Fatalf("WS 地址不符: got %q want %q", got, wantWS)
	}
}

// TestResolveWebSocketURLMissingWS 缺 webSocketDebuggerUrl 时给出明确错误(不再打印 %!w(<nil>))。
func TestResolveWebSocketURLMissingWS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"userDataDir":"/Users/x/.chrome-debug-profile"}`))
	}))
	defer srv.Close()

	_, err := resolveWebSocketURL(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "未返回 webSocketDebuggerUrl") {
		t.Fatalf("应报缺少 WS 地址: %v", err)
	}
	if strings.Contains(err.Error(), "%!w(") {
		t.Fatalf("错误格式化有误: %v", err)
	}
}

// TestResolveWebSocketURLPassesThroughWS 调用方直接给 ws:// 时应原样返回,不做 HTTP 探测。
func TestResolveWebSocketURLPassesThroughWS(t *testing.T) {
	const ws = "ws://127.0.0.1:9222/devtools/browser/direct"
	got, err := resolveWebSocketURL(ws)
	if err != nil || got != ws {
		t.Fatalf("ws 地址应原样透传: got %q err %v", got, err)
	}
}
