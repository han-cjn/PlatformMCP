package onboard

import (
	"os"
	"strings"
	"syscall"
)

// MissingCreds 返回指定平台中环境变量缺失/为空的凭据名列表(按声明顺序)。
func MissingCreds(platform string) []string {
	var missing []string
	for _, c := range PortalCreds[platform] {
		if strings.TrimSpace(os.Getenv(c.EnvKey)) == "" {
			missing = append(missing, c.EnvKey)
		}
	}
	return missing
}

// PlatformOrder 自动向导遍历顺序:与体检表/能力矩阵一致。
func PlatformOrder() []string {
	return []string{"feishu", "dingtalk", "wecom", "tencent-docs"}
}

// InteractiveTTY 判定当前进程是否有可交互终端(标准输入为字符设备)。
// stdio 宿主(如 WorkBuddy)以管道方式拉起时 stdin 是 pipe,返回 false。
func InteractiveTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// StdinIsDevNull 判定 stdin 是否被重定向到 /dev/null(测试探针常见做法)。
// /dev/null 也是字符设备(InteractiveTTY 会误判 true),需单独排除:
// 对其输入会立即 EOF,向导会陷入"格式不符:不能为空"死循环。
func StdinIsDevNull() (bool, error) {
	stStdin, err := os.Stdin.Stat()
	if err != nil {
		return false, err
	}
	stNull, err := os.Stat("/dev/null")
	if err != nil {
		return false, err
	}
	a, ok1 := stStdin.Sys().(*syscall.Stat_t)
	b, ok2 := stNull.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, nil
	}
	return a.Dev == b.Dev && a.Ino == b.Ino, nil
}
