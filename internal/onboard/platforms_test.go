package onboard

import (
	"strings"
	"testing"
)

// TestSupportedPlatformsFullList status 必须返回项目支持的全部平台(不只 missing),
// 能力清单从语义层注入后排序稳定,元信息不缺项。
func TestSupportedPlatformsFullList(t *testing.T) {
	caps := map[string][]string{
		"feishu":   {"table.query", "im.send_text"},
		"dingtalk": {"im.send_text"},
	}
	got := SupportedPlatforms(caps)
	order := PlatformOrder()
	if len(got) != len(order) {
		t.Fatalf("应返回项目支持的全部 %d 个平台,实际 %d", len(order), len(got))
	}
	for i, p := range got {
		if p.ID != order[i] || p.Name == "" || p.Title == "" || p.Hint == "" {
			t.Fatalf("第 %d 项元信息不完整: %+v", i, p)
		}
		if p.Credentials != "missing" {
			t.Fatalf("无环境变量时应为 missing: %+v", p)
		}
	}
	if strings.Join(got[0].Capabilities, ",") != "im.send_text,table.query" {
		t.Fatalf("capabilities 应排序: %v", got[0].Capabilities)
	}
	if !got[0].SetupSupported || !got[0].BrowserCapture {
		t.Fatalf("feishu 应支持 setup 与浏览器抓凭据: %+v", got[0])
	}
	for _, p := range got[1:] {
		if p.SetupSupported || p.BrowserCapture {
			t.Fatalf("%s 不应被标为自动接入/浏览器抓凭据: %+v", p.ID, p)
		}
	}
}

// TestSupportedPlatformsCredentialsState 凭据状态随环境变量变化,且不篡改调用方入参。
func TestSupportedPlatformsCredentialsState(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "cli_test")
	t.Setenv("FEISHU_APP_SECRET", "secret_test_value")
	got := SupportedPlatforms(nil)
	if got[0].Credentials != "configured" {
		t.Fatalf("凭据齐全应为 configured: %+v", got[0])
	}
	for _, p := range got[1:] {
		if p.Credentials != "missing" {
			t.Fatalf("%s 应保持 missing: %+v", p.ID, p)
		}
	}
	caps := map[string][]string{"feishu": {"table.query", "im.send_text"}}
	_ = SupportedPlatforms(caps)
	if strings.Join(caps["feishu"], ",") != "table.query,im.send_text" {
		t.Fatalf("不应修改调用方传入的 map: %v", caps["feishu"])
	}
}
