package onboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 2026-11-11 钉钉开放平台真机 dump 的脱敏样本:结构与可见文本逐字一致,
// 凭据值全部替换为假值。用于把「按可见文本声明的 Ops」锁在真实 DOM 上,
// 防止再出现「按飞书模板推测按钮名」造成的 selector_miss。
const (
	dingBaseinfoText = `个人调试 已上线
应用凭证
App ID : 1f2e47b9-a105-48f6-9cdc-45b32563a88e
原企业内部应用AgentId 5110249287
Client ID (原 AppKey 和 SuiteKey)
dingabcdefghijklmnop
Client Secret (原 AppSecret 和 SuiteSecret)
当前使用:-JxJlo7zXy****pY7Ah3PcyG
应用信息`

	dingPermissionText = `权限管理
选择开通状态 批量申请
全部（0） 个人权限（23）
权限信息 接口 权限点code 权限审批 全部状态 操作
Contact.User.mobile 无需审批 未开通 立即开通
snsapi_base 无需审批 已开通 默认开通
请输入权限或接口的名称进行搜索`

	dingVersionText = `版本管理与发布
创建新版本
版本号 版本状态 发布人 发布时间 操作
1.0.0 已上线 韩业超 2026-10-30 16:41:48 查看`
)

// collect-creds 必须先切到 /baseinfo,再按真实标签抽取;Secret 只有掩码。
func TestDingtalkCredOpsSwitchToBaseinfoTab(t *testing.T) {
	act := credAction(Portals["dingtalk"])
	if act == nil {
		t.Fatal("钉钉缺少浏览器凭据动作")
	}
	if act.Script != "dingtalk@2026-11-11-creds" {
		t.Fatalf("脚本标记未更新: %s", act.Script)
	}
	if len(act.Ops) != 6 {
		t.Fatalf("凭据 Ops 应为 6 步,实际 %d: %+v", len(act.Ops), act.Ops)
	}
	if act.Ops[0].Kind != "waitVisible" || act.Ops[0].Target != menuCandidateSelector ||
		!strings.Contains(act.Ops[0].Match, "凭证与基础信息") {
		t.Fatalf("第 1 步应等「凭证与基础信息」菜单: %+v", act.Ops[0])
	}
	if act.Ops[1].Kind != "click" || !strings.Contains(act.Ops[1].Match, "凭证与基础信息") {
		t.Fatalf("第 2 步应点「凭证与基础信息」菜单: %+v", act.Ops[1])
	}
	if act.Ops[2].Kind != "waitVisible" || act.Ops[2].Target != "body" {
		t.Fatalf("第 3 步应等凭据页渲染: %+v", act.Ops[2])
	}
	if act.Ops[3].Kind != "readPage" {
		t.Fatalf("第 4 步应 readPage: %+v", act.Ops[3])
	}

	reID := regexp.MustCompile(act.Ops[4].Target)
	if got := reID.FindStringSubmatch(dingBaseinfoText); len(got) < 2 || got[1] != "dingabcdefghijklmnop" {
		t.Fatalf("Client ID 正则未从真机文本抽到明文: %v /%s/", got, act.Ops[4].Target)
	}
	reSecret := regexp.MustCompile(act.Ops[5].Target)
	got := reSecret.FindStringSubmatch(dingBaseinfoText)
	if len(got) < 2 || !strings.Contains(got[1], "****") {
		t.Fatalf("Client Secret 正则应命中掩码串(交给格式校验拒绝): %v /%s/", got, act.Ops[5].Target)
	}
}

// 掩码绝不能被写进 .env;降级文案必须点明「只差 Secret」。
func TestDingtalkMaskedSecretNeverWritten(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	_, ce := commitCredentials("dingtalk", map[string]string{
		"DINGTALK_Client_ID":     "dingabcdefghijklmnop",
		"DINGTALK_Client_Secret": "-JxJlo7zXy****pY7Ah3PcyG",
	}, CredOptions{EnvPath: envPath, SkipSetEnv: true})
	if ce == nil || ce.Phase != "invalid_format" {
		t.Fatalf("掩码应被格式校验拒绝,实际 %+v", ce)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("掩码凭据不得写入 .env")
	}
	next := captureNext(capturePhase(ce.Phase), "dingtalk")
	for _, want := range []string{"掩码", "set_credentials", "Client Secret", "不要"} {
		if !strings.Contains(next, want) {
			t.Fatalf("降级指引缺 %q: %s", want, next)
		}
	}
}

// 幂等检测必须用真机状态文本,不能再用飞书式权限名或「已发布」猜测。
func TestDingtalkStageDoneOnRealPages(t *testing.T) {
	stages := platformStages(Portals["dingtalk"])
	byID := map[string]setupStage{}
	for _, s := range stages {
		byID[s.ID] = s
	}
	if !byID["publish"].Done(dingVersionText) {
		t.Fatal("版本表格有「版本状态 + 已上线」应判 already_done")
	}
	if byID["publish"].Done(dingPermissionText) {
		t.Fatal("权限页不应被误判为已发布")
	}
	if byID["enable_perms"].Done(dingPermissionText) {
		t.Fatal("存在「未开通」权限时不应判已完成")
	}
	allOpen := "批量申请\n权限点code\nContact.User.Read 无需审批 已开通\nsnsapi_base 无需审批 已开通 默认开通"
	if !byID["enable_perms"].Done(allOpen) {
		t.Fatal("无「未开通」项时应判已完成")
	}
	for _, s := range stages {
		if s.ApplyNote == "" {
			t.Fatalf("%s 未声明 not_required 降级原因", s.ID)
		}
		if len(opsForScope(s.Action, "apply")) != 0 {
			t.Fatalf("%s 仍保留会误提交的 apply Ops", s.ID)
		}
	}
}

// 每个 Match 都要能在真机可见文本上命中;旧版推测按钮不得残留。
func TestDingtalkStageOpsUseRealMenus(t *testing.T) {
	realTexts := []string{
		"凭证与基础信息", "权限管理", "版本管理与发布", "批量申请",
		"创建新版本", "权限点code", "版本状态", "应用凭证",
		dingBaseinfoText, dingPermissionText, dingVersionText,
	}
	for _, s := range platformStages(Portals["dingtalk"]) {
		for _, op := range s.Action.Ops {
			if op.Match == "" {
				continue
			}
			re := regexp.MustCompile(op.Match)
			hit := false
			for _, txt := range realTexts {
				if re.MatchString(txt) {
					hit = true
					break
				}
			}
			if !hit {
				t.Fatalf("%s 的 Match /%s/ 在真机文本上无命中(推测选择器?)", s.ID, op.Match)
			}
			for _, bad := range []string{"创建版本|新建版本", "确定|确认|提交|申请", "权限申请|权限配置"} {
				if strings.Contains(op.Match, bad) {
					t.Fatalf("%s 残留旧推测 Match %q", s.ID, op.Match)
				}
			}
		}
	}
}

// 钉钉权限探针不再串出飞书 scope。
func TestProbePermsUnverifiedPerPlatform(t *testing.T) {
	d := ProbePerms("dingtalk")
	if d.Status != "unsupported" {
		t.Fatalf("钉钉探针应为 unsupported,实际 %s", d.Status)
	}
	got := strings.Join(d.Unverified, ",")
	if got != strings.Join(Portals["dingtalk"].Perms, ",") {
		t.Fatalf("钉钉 unverified 应为自己的 Perms,实际 %q", got)
	}
	if strings.Contains(got, "im:message") || strings.Contains(got, "bitable:app") {
		t.Fatalf("钉钉探针串入飞书 scope: %q", got)
	}
	if strings.Join(permsUnverified("feishu"), ",") != strings.Join(defaultPerms, ",") {
		t.Fatal("飞书 unverified 应保持默认权限集")
	}
	// 真机 /permission 页列的是英文 code;中文飞书式标签同样属于错误标注。
	for _, bad := range []string{"通讯录只读", "消息推送", "SmartForm"} {
		if strings.Contains(got, bad) {
			t.Fatalf("钉钉探针仍带推测的中文权限标签 %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "Contact.User.Read") {
		t.Fatalf("钉钉探针应带真机权限点 code: %q", got)
	}
	// 未声明 Perms 的平台也不能回退到飞书默认集。
	if u := permsUnverified("tencent-docs"); len(u) != 0 {
		t.Fatalf("腾讯文档不应回退飞书 scope: %v", u)
	}
}

// gate 只标已验证项:tab 导航实测过,但创建/权限/发布表单仍未校准。
func TestSupportedPlatformsTabsVerified(t *testing.T) {
	for _, x := range SupportedPlatforms(nil) {
		if x.TabsVerified != (x.ID == "dingtalk") {
			t.Fatalf("%s 的 tabs_verified 不实: %+v", x.ID, x)
		}
		if x.DomVerified != (x.ID == "feishu") {
			t.Fatalf("%s 的 dom_verified 不实: %+v", x.ID, x)
		}
	}
}

func TestSetupStageOutcomesNotRequired(t *testing.T) {
	failed, skipped := setupStageOutcomes([]SetupStep{
		{ID: "capture", Status: "done"},
		{ID: "enable_perms", Status: "not_required"},
		{ID: "publish", Status: "already_done"},
	})
	if len(failed) != 0 {
		t.Fatalf("not_required 不应算失败: %v", failed)
	}
	if len(skipped) != 1 || skipped[0] != "enable_perms" {
		t.Fatalf("not_required 应计入 skipped: %v", skipped)
	}
}
