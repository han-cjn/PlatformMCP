// Package onboard 实现多平台接入引导(技术方案 hub onboard,见 docs/onboarding-automation-design.md)。
// 数据层(清单) + 编排器 + 双执行器(手动/浏览器) + 共享收尾链。
package onboard

import (
	"fmt"
	"os"
	"strings"
)

// UpsertEnv 将 kv 逐行合并进 .env:已存在的键覆盖、不存在的追加、
// 保留原有注释与未知变量;写临时文件后 rename,保证原子性;文件权限 0600。
func UpsertEnv(path string, kv map[string]string) error {
	var lines []string
	if raw, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(raw), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}

	// 待写键集合;逐行扫描,命中 KEY= 即原位覆盖
	pending := make(map[string]string, len(kv))
	for k, v := range kv {
		pending[k] = v
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		if v, ok := pending[key]; ok {
			lines[i] = key + "=" + v
			delete(pending, key)
		}
	}
	// 剩余键追加到末尾(pending 中只剩未命中的)
	for k, v := range pending {
		lines = append(lines, k+"="+v)
	}

	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600) // rename 不改变已存在目标权限,显式收紧
}

// Mask 凭据脱敏回显:只露前 4 位。
func Mask(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 8) + "(" + fmt.Sprint(len(s)) + "字符)"
}
