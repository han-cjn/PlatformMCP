package onboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClassifyPermResponse 权限不足必须能识别,未知错误不能误判成「需要开通」——
// 误判会让 setup 去点「开通权限」按钮,在权限已开通时产生不可撤销的企业记录。
func TestClassifyPermResponse(t *testing.T) {
	cases := []struct {
		httpStatus, code int
		msg, want        string
	}{
		{200, 0, "", "ok"},
		{200, 99991672, "no permission", "needs_perms"},
		{403, 999, "forbidden", "needs_perms"},
		{401, 999, "unauthorized", "needs_perms"},
		{200, 999, "缺少权限", "needs_perms"},
		{200, 999, "scope not granted", "needs_perms"},
		{200, 999, "internal error", "unknown"},
		{500, 0, "server error", "unknown"},
	}
	for _, c := range cases {
		if got := classifyPermResponse(c.httpStatus, c.code, c.msg); got != c.want {
			t.Fatalf("classifyPermResponse(%d,%d,%q)=%q,期望 %q", c.httpStatus, c.code, c.msg, got, c.want)
		}
	}
}

func TestProbePermsUnsupportedAndNoCreds(t *testing.T) {
	if got := ProbePerms("dingtalk"); got.Status != "unsupported" {
		t.Fatalf("非 feishu 应为 unsupported: %+v", got)
	}
	t.Setenv("FEISHU_APP_ID", "")
	t.Setenv("FEISHU_APP_SECRET", "")
	if got := ProbePerms("feishu"); got.Status != "no_credentials" {
		t.Fatalf("无凭据应为 no_credentials: %+v", got)
	}
}

// withMockFeishuAPI 把 token 与群列表端点指向 httptest.Server,返回清理函数。
func withMockFeishuAPI(t *testing.T, chatsStatus int, chatsBody string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-mock"}`))
			return
		}
		if r.URL.Path == "/chats" {
			if r.Header.Get("Authorization") != "Bearer t-mock" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":999,"msg":"bad token"}`))
				return
			}
			w.WriteHeader(chatsStatus)
			_, _ = w.Write([]byte(chatsBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	oldToken, oldChats := feishuTokenURL, feishuChatsProbeURL
	feishuTokenURL = srv.URL + "/token"
	feishuChatsProbeURL = srv.URL + "/chats"
	t.Cleanup(func() {
		feishuTokenURL, feishuChatsProbeURL = oldToken, oldChats
		srv.Close()
	})
}

// TestProbePermsAPIReady API 通过时只标记真正验证过的部分,不把 im:message/bitable:app
// 一起标成「已开通」。
func TestProbePermsAPIReady(t *testing.T) {
	withMockFeishuAPI(t, 200, `{"code":0,"msg":"success"}`)
	t.Setenv("FEISHU_APP_ID", "cli_test")
	t.Setenv("FEISHU_APP_SECRET", "secret_test")
	got := ProbePerms("feishu")
	if got.Status != "api_ready" {
		t.Fatalf("应为 api_ready: %+v", got)
	}
	if len(got.Checked) != 1 || !strings.Contains(got.Checked[0], "im:chat") {
		t.Fatalf("checked 只应含实际验证过的 scope: %v", got.Checked)
	}
	if len(got.Unverified) != 2 {
		t.Fatalf("im:message 与 bitable:app 必须留在 unverified: %v", got.Unverified)
	}
}

func TestProbePermsNeedsPermsAndUnknown(t *testing.T) {
	withMockFeishuAPI(t, 200, `{"code":99991672,"msg":"no permission"}`)
	t.Setenv("FEISHU_APP_ID", "cli_test")
	t.Setenv("FEISHU_APP_SECRET", "secret_test")
	if got := ProbePerms("feishu"); got.Status != "needs_perms" {
		t.Fatalf("明确权限不足应进 UI 开通流程: %+v", got)
	}
	// 端点坏了不能误判成权限不足
	old := feishuChatsProbeURL
	feishuChatsProbeURL = "http://127.0.0.1:1/chats"
	got := ProbePerms("feishu")
	feishuChatsProbeURL = old
	if got.Status != "unknown" {
		t.Fatalf("网络错误应为 unknown 而非 needs_perms: %+v", got)
	}
}
