package onboard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testCredValues = map[string]map[string]string{
	"feishu": {
		"FEISHU_APP_ID": "cli_a1b2c3d4e5f6", "FEISHU_APP_SECRET": "AbCdEfGhIjKlMnOp1234",
	},
	"dingtalk": {
		"DINGTALK_Client_ID": "dingabcdefgh", "DINGTALK_Client_Secret": "AbCdEfGhIjKlMnOp1234",
		"ROBOT_CODE": "dingrobotcode123",
	},
	"wecom": {
		"WECOM_BOT_ID": "botid_12345", "WECOM_BOT_SECRET": "AbCdEfGhIjKlMnOp1234",
	},
	"tencent-docs": {
		"TENCENT_DOCS_TOKEN": "tok_abcdefghijklmnopqrstuvwxyz",
	},
}

func okValidate(_ map[string]string) error { return nil }

// TestCredentialFieldsAllPlatforms 四个平台都必须有可收集的凭据字段,
// 这是 set_credentials 能「零 DOM、四平台通吃」的前提。
func TestCredentialFieldsAllPlatforms(t *testing.T) {
	want := map[string]int{"feishu": 2, "dingtalk": 3, "wecom": 2, "tencent-docs": 1}
	for _, id := range PlatformOrder() {
		fields := CredentialFields(id)
		if len(fields) != want[id] {
			t.Fatalf("%s 字段数应为 %d,实际 %d", id, want[id], len(fields))
		}
		for _, f := range fields {
			if f.EnvKey == "" || f.Label == "" {
				t.Fatalf("%s 字段不完整: %+v", id, f)
			}
			if _, ok := testCredValues[id][f.EnvKey]; !ok {
				t.Fatalf("%s 的字段 %s 不在测试样本里,可能平台键名漂移", id, f.EnvKey)
			}
		}
	}
}

func tempEnvPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".env")
}

// TestSetCredentialsIncomplete 缺字段时返回 required 且不触发真连/写盘/热重载。
func TestSetCredentialsIncomplete(t *testing.T) {
	path := tempEnvPath(t)
	validated, reloaded := false, false
	res, err := SetCredentials("feishu", map[string]string{"FEISHU_APP_ID": "cli_a1b2c3d4e5f6"}, CredOptions{
		EnvPath: path, SkipSetEnv: true,
		Validate: func(map[string]string) error { validated = true; return nil },
		Reload:   func(map[string]string) error { reloaded = true; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != "incomplete" || len(res.Required) != 2 {
		t.Fatalf("期望 incomplete + required 两项,实际 %+v", res)
	}
	if validated || reloaded {
		t.Fatal("缺字段时不应真连校验或热重载")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("缺字段时不应写 .env")
	}
}

func TestSetCredentialsInvalidFormat(t *testing.T) {
	path := tempEnvPath(t)
	res, _ := SetCredentials("feishu", map[string]string{
		"FEISHU_APP_ID": "not-a-cli-id", "FEISHU_APP_SECRET": "AbCdEfGhIjKlMnOp1234",
	}, CredOptions{EnvPath: path, SkipSetEnv: true, Validate: okValidate})
	if res.Phase != "invalid_format" {
		t.Fatalf("期望 invalid_format,实际 %+v", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("格式错误不应写盘")
	}
}

func TestSetCredentialsVerifyFailedNeverWrites(t *testing.T) {
	path := tempEnvPath(t)
	res, _ := SetCredentials("dingtalk", testCredValues["dingtalk"], CredOptions{
		EnvPath: path, SkipSetEnv: true,
		Validate: func(map[string]string) error { return errors.New("凭据被平台拒绝") },
	})
	if res.Phase != "verify_failed" || !strings.Contains(res.Message, "未写入") {
		t.Fatalf("期望 verify_failed 且提示未写盘,实际 %+v", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("真连失败绝不能把错误凭据写进 .env")
	}
}

// TestSetCredentialsAllPlatformsDone 四个平台都能走完同一条收尾链。
func TestSetCredentialsAllPlatformsDone(t *testing.T) {
	for _, id := range PlatformOrder() {
		t.Run(id, func(t *testing.T) {
			path := tempEnvPath(t)
			reloads := 0
			var validated map[string]string
			res, err := SetCredentials(id, testCredValues[id], CredOptions{
				EnvPath: path, SkipSetEnv: true,
				Validate: func(env map[string]string) error { validated = env; return nil },
				Reload:   func(map[string]string) error { reloads++; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Phase != "done" || len(res.Collected) != len(CredentialFields(id)) {
				t.Fatalf("期望 done + 全部字段,实际 %+v", res)
			}
			if reloads != 1 || len(validated) != len(CredentialFields(id)) {
				t.Fatalf("真连/热重载未按预期各执行一次: validate=%v reloads=%d", validated, reloads)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range testCredValues[id] {
				if !strings.Contains(string(raw), k+"="+v) {
					t.Fatalf(".env 缺少 %s", k)
				}
			}
			if strings.Contains(res.Message, testCredValues[id][CredentialFields(id)[0].EnvKey]) {
				t.Fatal("结果里不应回显凭据值")
			}
		})
	}
}

func TestSetCredentialsReloadFailedStillWroteEnv(t *testing.T) {
	path := tempEnvPath(t)
	res, _ := SetCredentials("tencent-docs", testCredValues["tencent-docs"], CredOptions{
		EnvPath: path, SkipSetEnv: true, Validate: okValidate,
		Reload: func(map[string]string) error { return errors.New("挂载失败") },
	})
	if res.Phase != "reload_failed" {
		t.Fatalf("期望 reload_failed,实际 %+v", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("热重载失败时 .env 应已写入")
	}
	if !strings.Contains(res.Next, "禁用/启用") {
		t.Fatal("next 应给出连接器刷新指引")
	}
}

func TestSetCredentialsWriteFailed(t *testing.T) {
	res, _ := SetCredentials("feishu", testCredValues["feishu"], CredOptions{
		EnvPath: t.TempDir(), SkipSetEnv: true, Validate: okValidate, // 目录不可当文件写
	})
	if res.Phase != "write_failed" {
		t.Fatalf("期望 write_failed,实际 %+v", res)
	}
}

// TestCapturePhaseMapping 公共收尾的新阶段必须映射到 Capture 的历史 phase,
// 且降级 next 指向 set_credentials,不能让用户切终端。
func TestCapturePhaseMapping(t *testing.T) {
	if got := capturePhase("invalid_format"); got != "incomplete" {
		t.Fatalf("invalid_format 应映射为 incomplete,实际 %s", got)
	}
	if got := capturePhase("verify_failed"); got != "verify_failed" {
		t.Fatalf("verify_failed 应原样透传,实际 %s", got)
	}
	// 页面读取/真连失败可以换零 DOM 的 set_credentials;写盘失败是本地文件
	// 权限问题,两条路径共用同一个 UpsertEnv,换工具无意义,应直指写权限。
	for _, phase := range []string{"incomplete", "verify_failed"} {
		if !strings.Contains(captureNext(phase), "hub.onboard.set_credentials") {
			t.Fatalf("%s 的 next 未给出 set_credentials 降级路径", phase)
		}
	}
	if !strings.Contains(captureNext("write_failed"), "写权限") {
		t.Fatal("write_failed 的 next 应明确指向工作目录写权限")
	}
}
