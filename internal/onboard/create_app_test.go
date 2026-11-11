package onboard

import (
	"strings"
	"testing"
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
