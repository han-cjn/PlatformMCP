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
	// L2 之后四平台都挂了各自的浏览器凭据动作(browser_capture=true),
	// 且每项都要给出 credential_fields 供 set_credentials 渲染表单。
	for _, p := range got {
		if !p.BrowserCapture {
			t.Fatalf("%s 应支持浏览器抓凭据: %+v", p.ID, p)
		}
		if len(p.CredentialKeys) == 0 {
			t.Fatalf("%s 缺少 credential_fields: %+v", p.ID, p)
		}
	}
	// L3 后 setup 链路四平台全量可用;dom_verified 只对真机校准过的飞书、钉钉为真
	// (钉钉 2026-11-12 第十二轮跑通版本表单+发布按钮采集后置 true)。
	for _, p := range got {
		if !p.SetupSupported {
			t.Fatalf("%s 的 setup 链路应可用: %+v", p.ID, p)
		}
		want := p.ID == "feishu" || p.ID == "dingtalk"
		if p.DomVerified != want {
			t.Fatalf("%s 的 dom_verified 不实: %+v", p.ID, p)
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
