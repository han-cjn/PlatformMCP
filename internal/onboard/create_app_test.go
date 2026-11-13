package onboard

import (
	"strings"
	"testing"
	"time"
)

// TestCreateAppActionPerPlatform L2/L3 之后:应用控制台类平台都声明了各自的
// 创建动作,脚本标记必须带平台前缀(否则会误跑飞书的 DOM 选择器);
// token_page 类平台(腾讯文档)没有应用概念,不应有创建动作。
func TestCreateAppActionPerPlatform(t *testing.T) {
	for _, id := range []string{"feishu", "dingtalk", "wecom"} {
		p, ok := FindPortal(id)
		if !ok {
			t.Fatalf("找不到 %s portal", id)
		}
		act := createAppAction(p)
		if act == nil {
			t.Fatalf("%s 应定义创建应用/机器人动作", id)
		}
		if !strings.HasPrefix(act.Script, id+"@") {
			t.Fatalf("%s 的创建脚本缺平台前缀,有误跑跨平台选择器风险: %s", id, act.Script)
		}
	}
	q, _ := FindPortal("tencent-docs")
	if createAppAction(q) != nil {
		t.Fatal("token_page 平台不应定义创建动作")
	}
}

// TestCreateAppUnsupportedPlatform unsupported 分支在 Watch 之前返回,
// 不依赖 CDP,可在无 Chrome 的 CI 里稳定验证「确认门之前不碰浏览器」。
func TestCreateAppUnsupportedPlatform(t *testing.T) {
	res, err := CreateApp("tencent-docs", "whatever", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != "unsupported" {
		t.Fatalf("期望 unsupported,实际 %q", res.Phase)
	}
	if !strings.Contains(res.Next, "set_credentials") || strings.Contains(res.Next, "go run") {
		t.Fatalf("降级指引应指向 set_credentials 且不能让用户切终端: %q", res.Next)
	}
	if len(res.ExistingApps) != 0 {
		t.Fatal("unsupported 不应返回已有应用")
	}
}

func TestCreateAppRejectUnknownPlatform(t *testing.T) {
	if _, err := CreateApp("not-a-platform", "", false, nil); err == nil {
		t.Fatal("未知平台应返回错误")
	}
}

// TestCreateAppOps 创建动作必须:替换应用名、去掉会白等 60s 的 waitURL
// 与会往工作目录落盘的 screenshot、保留按文本匹配的 Match。
func TestCreateAppOps(t *testing.T) {
	p, _ := FindPortal("feishu")
	act := createAppAction(p)
	ops := createAppOps(act, "my-hub-app")

	fillFound, matchFound := false, false
	for _, op := range ops {
		switch op.Kind {
		case "waitURL", "screenshot":
			t.Fatalf("createAppOps 不应保留 %s", op.Kind)
		case "fill":
			fillFound = true
			if op.Text != "my-hub-app" {
				t.Fatalf("$app_name 未替换: %q", op.Text)
			}
		}
		if op.Match != "" {
			matchFound = true
		}
		if strings.Contains(op.Text, "$app_name") {
			t.Fatal("替换后有残留占位符")
		}
	}
	if !fillFound || !matchFound {
		t.Fatalf("缺少 fill(%v) 或文本匹配(%v)", fillFound, matchFound)
	}
	if len(ops) >= len(act.Ops) {
		t.Fatal("未过滤掉 waitURL/screenshot")
	}
}

func TestAppIDFromURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://open.feishu.cn/app/cli_a1b2c3d4/baseinfo", "cli_a1b2c3d4"},
		{"/app/cli_abc123/baseinfo?from=list", "cli_abc123"},
		{"https://open.feishu.cn/app", ""},
		{"https://open.feishu.cn/app/create", ""}, // 功能页不是应用
		{"https://open.feishu.cn/app/search", ""}, // 同上
		{"/app/create?type=self", ""},
	}
	for _, c := range cases {
		if got := appIDFromURL(c.in); got != c.want {
			t.Errorf("appIDFromURL(%q) = %q,期望 %q", c.in, got, c.want)
		}
	}
}

// TestManualStepsExcludesCreateApp create-app 现在有浏览器动作,不应再被
// 列为「必须人工」;状态感知修复后 start 才不会让已登录用户再去建应用。
func TestManualStepsExcludesCreateApp(t *testing.T) {
	p, _ := FindPortal("feishu")
	steps := manualSteps(p)
	if len(steps) == 0 {
		t.Fatal("manualSteps 不应为空")
	}
	for _, s := range steps {
		if strings.Contains(s, "创建企业自建应用") {
			t.Fatalf("create-app 已有浏览器动作,不应列为纯人工步骤: %q", s)
		}
	}
}

// TestPageInAdminAreaRejectsWecomMarketingPage 2026-11-12 真机缺陷:
// 企微官网营销页与后台同 host,listExistingApps 的 a[href] 兜底扫描把
// 「工作台管理」(#/app/workbenchEditor/index)经 appIDFromURL 抽成
// workbenchEditor,当成已有机器人 → CreateApp 报 existing_apps、
// Setup 确认清单漏掉「创建应用」,机器人删除后永远不触发代建。
func TestPageInAdminAreaRejectsWecomMarketingPage(t *testing.T) {
	wecom, _ := FindPortal("wecom")
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"官网营销页(同 host,必须排除)", "https://work.weixin.qq.com/#/app/workbenchEditor/index", false},
		{"营销页另一 hash", "https://work.weixin.qq.com/#/app/workbenchEditor/index?from=manage_tools", false},
		{"后台列表页", "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/list?from=manage_tools&tab=manage", true},
		{"后台详情页", "https://work.weixin.qq.com/wework_admin/frame#/aiHelper/detail?aibotid=33776999461281670", true},
		{"后台根 frame", "https://work.weixin.qq.com/wework_admin/frame#/", true},
		{"跨平台残留标签", "https://open.feishu.cn/app/cli_xxx/baseinfo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pageInAdminArea(wecom, c.url); got != c.want {
				t.Fatalf("pageInAdminArea(%q)=%v,期望 %v", c.url, got, c.want)
			}
		})
	}
	// 未声明 AppListURL 的平台(飞书)必须保持全域通过,不能波及已真机验证链路。
	feishu, _ := FindPortal("feishu")
	if feishu.AppListURL != "" {
		t.Fatal("前提变更:飞书现在声明了 AppListURL,请重新评估该门对其他平台的影响")
	}
	if !pageInAdminArea(feishu, "https://open.feishu.cn/app/cli_xxx/baseinfo") {
		t.Fatal("飞书未声明 AppListURL,pageInAdminArea 应保持 true")
	}
}

// TestStrictListGateOnlyForMarkeredPlatforms 固化 CreateApp/openAppForSetup
// 新加的 listVerified 硬闸门只作用于声明了列表特征平台的边界。
func TestStrictListGateOnlyForMarkeredPlatforms(t *testing.T) {
	for _, id := range []string{"wecom", "dingtalk"} {
		p, _ := FindPortal(id)
		if len(p.AppListMarkers) == 0 {
			t.Errorf("%s 应声明 AppListMarkers,否则未渲染列表会被误判为「企业内没有应用」", id)
		}
	}
	feishu, _ := FindPortal("feishu")
	if len(feishu.AppListMarkers) > 0 {
		t.Fatal("飞书一旦声明 AppListMarkers,create_app 的新门会改变已真机验证的行为,需重新评估")
	}
}

// TestAppLinkIsPlatformApp 固化 a[href] 兜底扫描的窄匹配边界。
// 关键点:portalAppID 对企微会回落 appIDFromURL,把后台导航链接
// 「工作台管理」误抽成应用 ID;声明了 AppIDRe 的平台不允许这个回落。
func TestAppLinkIsPlatformApp(t *testing.T) {
	wecom, _ := FindPortal("wecom")
	dingtalk, _ := FindPortal("dingtalk")
	feishu, _ := FindPortal("feishu")

	marketing := "#/app/workbenchEditor/index"
	if got := portalAppID(wecom, marketing); got != "workbenchEditor" {
		t.Fatalf("前提变更:宽匹配不再误抽该链接(%q),请复核本修复是否仍需保留", got)
	}
	cases := []struct {
		name string
		p    *Portal
		href string
		want bool
	}{
		{"企微后台导航链接必须排除", wecom, marketing, false},
		{"企微真机器人详情链接", wecom, "#/aiHelper/detail?aibotid=33776999461281670", true},
		{"钉钉真应用详情链接", dingtalk, "#/app/1f2e47b9-a105-48f6-9cdc-45b32563a88e/dev", true},
		{"飞书保留 /app/<appId> 宽匹配", feishu, "/app/cli_aabbccdd1122/baseinfo", true},
		{"空链接", wecom, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appLinkIsPlatformApp(c.p, c.href); got != c.want {
				t.Fatalf("appLinkIsPlatformApp(%s,%q)=%v,期望 %v", c.p.Name, c.href, got, c.want)
			}
		})
	}
}

// TestWecomCreateBotOpsAreCalibratedAgainstRealDOM 锁死 2026-11-12 真机校准:
// 企微创建是独立 SPA 页(#/aiHelper/create)、必须切 API 模式、提交按钮是
// 「保存」;创建页可能是 hidden 标签,只能用不依赖 rAF 的 clickAt/fillJS。
func TestWecomCreateBotOpsAreCalibratedAgainstRealDOM(t *testing.T) {
	p, _ := FindPortal("wecom")
	act := createAppAction(p)
	if act == nil {
		t.Fatal("企微 create-bot 动作缺失")
	}
	if act.Script != "wecom@2026-11-12-create-api-mode" {
		t.Fatalf("创建脚本版本未升级到真机校准版: %s", act.Script)
	}
	ops := createAppOps(act, "mcp-hub")
	kinds := make([]string, 0, len(ops))
	for _, op := range ops {
		kinds = append(kinds, op.Kind)
		if op.Kind == "click" || op.Kind == "fill" {
			t.Fatalf("hidden 标签上 %s 会挂 WaitStableRAF,创建链路不得使用: %+v", op.Kind, op)
		}
	}
	if got, want := strings.Join(kinds, ","), "navigate,waitVisible,clickAt,fillJS,fillJS,clickAt,clickAt"; got != want {
		t.Fatalf("Ops 序列与真机校准不符: %s,期望 %s", got, want)
	}
	if !strings.Contains(ops[0].Target, "#/aiHelper/create") {
		t.Fatalf("首步应导航到独立创建页: %q", ops[0].Target)
	}
	if ops[2].Target != "label.config_mode_card_radio" || ops[2].Index != 1 || ops[2].Match != "" {
		t.Fatalf("必须按索引切到 API 模式(radio label 无文本,真机第 2 个): %+v", ops[2])
	}
	if ops[3].Text != "mcp-hub" || !strings.Contains(ops[3].Target, "输入智能机器人名称") {
		t.Fatalf("名称未填入真机名称框: %+v", ops[3])
	}
	if ops[4].Text != "mcp-hub 集成机器人(API 模式,长连接)" || !strings.Contains(ops[4].Target, "textarea") {
		t.Fatalf("必填简介未补(真机保存曾报「请输入简介」): %+v", ops[4])
	}
	if ops[5].Target != "span.sdk-get-secret" || !ops[5].Optional {
		t.Fatalf("保存前必须生成 Secret 且幂等(生成后按钮消失): %+v", ops[5])
	}
	if !strings.Contains(ops[6].Match, "保存") {
		t.Fatalf("提交按钮应匹配「保存」而非推测的「确定/创建」: %+v", ops[6])
	}
}

// 2026-11-12 真机:企微 API 模式保存后跳转 >15s,旧上限误报 create_failed,
// 而失败文案「请手动完成创建」会诱导重复提交已存在的应用。
func TestCreateAppWaitWindowAndListedFallback(t *testing.T) {
	if createAppURLWaitMax < 30*time.Second {
		t.Fatalf("详情页等待窗口 %s 不足以覆盖企微慢跳转,会误报创建失败", createAppURLWaitMax)
	}
	cases := []struct {
		name string
		apps []AppRef
		want string
		got  bool
	}{
		{"精确命中", []AppRef{{Name: "mcp-hub"}}, "mcp-hub", true},
		{"带空白/大小写", []AppRef{{Name: "  MCP-Hub "}}, "mcp-hub", true},
		{"同名不同应用", []AppRef{{Name: "mcp-hub-prod"}}, "mcp-hub", false},
		{"空名称不算命中", []AppRef{{Name: ""}}, "mcp-hub", false},
		{"空列表", nil, "mcp-hub", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appNameMatches(c.apps, c.want); got != c.got {
				t.Fatalf("appNameMatches=%v,期望 %v", got, c.got)
			}
		})
	}
	if appNameMatches([]AppRef{{Name: "anything"}}, "") {
		t.Fatal("目标名为空时不得判定为已创建")
	}
}
