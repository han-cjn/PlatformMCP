package onboard

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// Options 向导运行配置。
type Options struct {
	Browser    bool                              // 启用浏览器执行器(失败自动降级手动)
	DebugURL   string                            // Chrome 调试端口,默认 http://127.0.0.1:9222
	EnvPath    string                            // .env 路径,默认 ".env"
	Reload     func(env map[string]string) error // 收尾:写完 .env+Setenv 后热重载该平台
	VerifyOnly bool                              // 跳过向导:直接用当前环境变量做 真连校验+写盘+热重载(日常体检/验收用)
}

// Run 执行平台接入向导:逐步引导(手动/浏览器双执行器,可互相降级)
// → 凭据收集与格式校验 → 平台真连校验 → .env 原子写入 → 热重载回调。
func Run(p *Portal, opts Options) error {
	if opts.EnvPath == "" {
		opts.EnvPath = ".env"
	}
	env := map[string]string{}

	// VerifyOnly:跳过交互,凭据取自当前进程环境(即 .env 加载值)
	if opts.VerifyOnly {
		for _, c := range PortalCreds[p.Name] {
			if v := os.Getenv(c.EnvKey); v != "" {
				env[c.EnvKey] = v
			}
		}
		if len(env) == 0 {
			return fmt.Errorf("%s 的凭据均未配置,请先跑完整向导(去掉 -onboard-verify-only)", p.Name)
		}
	} else {
		var bx *BrowserExecutor
		if opts.Browser {
			var err error
			bx, err = NewBrowserExecutor(opts.DebugURL)
			if err != nil {
				fmt.Println("⚠️  浏览器执行器不可用,本次向导全部走手动模式:", err)
			} else {
				defer bx.Close()
				fmt.Println("✓ 浏览器执行器已连接(支持代填的步骤将自动执行,失败自动降级手动)")
			}
		}
		if err := runWizard(p, bx, env); err != nil {
			return err
		}
	}

	// ---- 真连校验:凭据错误允许重输,network 可重试 ----
	for {
		fmt.Printf("\n正在真连校验 %s 凭据...\n", p.Name)
		err := p.Validate(env)
		if err == nil {
			fmt.Println("✓ 凭据校验通过")
			break
		}
		var ve *ValidateError
		if errors.As(err, &ve) && ve.Kind == "credentials" {
			fmt.Println("✗ 凭据被平台拒绝:", ve.Msg)
			choice := prompt("输入 r 重新输入凭据 / 其他任意键放弃退出: ")
			if strings.TrimSpace(choice) != "r" {
				return errors.New("凭据校验未通过,已放弃(未写入 .env)")
			}
			if err := collectCreds(p, env); err != nil {
				return err
			}
			continue
		}
		// network:询问重试
		fmt.Println("⚠️ 平台不可达:", err)
		choice := prompt("输入 r 重试 / 其他任意键放弃退出: ")
		if strings.TrimSpace(choice) != "r" {
			return errors.New("平台不可达,已放弃(未写入 .env)")
		}
	}

	// ---- 收尾链:写 .env + 更新进程环境 + 热重载 ----
	fmt.Printf("正在写入 %s(原子写,权限 0600)...\n", opts.EnvPath)
	if err := UpsertEnv(opts.EnvPath, env); err != nil {
		return fmt.Errorf("写 .env 失败: %w", err)
	}
	for k, v := range env {
		os.Setenv(k, v) // 让本进程后续逻辑与热重载直接使用新值
	}
	for k, v := range env {
		fmt.Printf("  %s=%s\n", k, Mask(v))
	}
	if opts.Reload != nil {
		fmt.Println("正在热重载平台挂载...")
		if err := opts.Reload(env); err != nil {
			fmt.Println("⚠️ 热重载失败(可稍后重启进程恢复):", err)
			return err
		}
	}
	fmt.Printf("\n🎉 %s 接入完成\n", p.Title)
	return nil
}

// runWizard 逐步推进清单:浏览器执行器可用则代填,失败或未定义时给手动说明。
func runWizard(p *Portal, bx *BrowserExecutor, env map[string]string) error {
	fmt.Printf("\n==================== %s 接入向导 ====================\n", p.Title)
	fmt.Printf("共 %d 步。原则:登录/审批必须人工,表单类步骤可由浏览器代填。\n\n", len(p.Steps))
	for i, s := range p.Steps {
		fmt.Printf("第 %d/%d 步:%s\n", i+1, len(p.Steps), s.Brief)
		degraded := false
		if bx != nil && s.Browser != nil {
			fmt.Printf("  [浏览器] 执行脚本 %s...\n", s.Browser.Script)
			if err := bx.RunAction(s.Browser, env); err != nil {
				fmt.Printf("  [浏览器] ✗ %s\n", err)
				fmt.Println("  ↓ 已降级为手动,请按以下说明操作:")
				degraded = true
			} else {
				fmt.Println("  [浏览器] ✓ 步骤完成")
				if len(bx.Collected) > 0 { // readText 抽到的值直接进入凭据池
					for k, v := range bx.Collected {
						env[k] = v
						fmt.Printf("  [浏览器] 已抽取 %s = %s\n", k, Mask(v))
					}
					bx.Collected = map[string]string{}
				}
			}
		}
		if bx == nil || s.Browser == nil || degraded {
			for _, d := range s.Detail {
				fmt.Println("  -", d)
			}
			for _, u := range s.URLs {
				fmt.Println("  打开:", u)
				if cmd := exec.Command("open", u); cmd.Run() != nil {
					fmt.Println("    (自动打开失败,请手动复制到浏览器)")
				}
			}
		}
		if s.Cred {
			if err := collectCreds(p, env); err != nil {
				return err
			}
		} else if !s.hasBrowserDone(bx, degraded) {
			prompt("  完成后按回车继续... ")
		}
	}
	return nil
}

// hasBrowserDone 浏览器已代填完成的步骤不再阻塞等待人工回车。
func (s *Step) hasBrowserDone(bx *BrowserExecutor, degraded bool) bool {
	return bx != nil && s.Browser != nil && !degraded
}

// collectCreds 逐凭据终端输入:即时格式校验,Secret/Token 类不回显。
func collectCreds(p *Portal, env map[string]string) error {
	for _, c := range PortalCreds[p.Name] {
		for {
			fmt.Printf("  请输入 %s(%s): ", c.Label, c.EnvKey)
			val := readHidden(c.EnvKey)
			if c.Format != nil {
				if err := c.Format(val); err != nil {
					fmt.Println("  ✗ 格式不符:", err, "(请重输)")
					continue
				}
			}
			env[c.EnvKey] = val
			break
		}
	}
	return nil
}

// readHidden 含 SECRET/TOKEN 的键关闭回显(/dev/tty 直读,不进 shell history)。
func readHidden(envKey string) string {
	upper := strings.ToUpper(envKey)
	if (strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN")) && tty != nil {
		if b, err := term.ReadPassword(int(tty.Fd())); err == nil {
			fmt.Println()
			return strings.TrimSpace(string(b))
		}
		// tty 读失败(极端情况)时退回普通读
	}
	return strings.TrimSpace(prompt(""))
}

var stdin *bufio.Scanner

// tty 缓存 /dev/tty 句柄(若可用)。Agent 以 PTY/管道方式拉起向导时,
// os.Stdin 是管道无法交互,而 /dev/tty 仍指向用户终端,人工确认(回车)从这里读。
var tty *os.File

func init() {
	// 打开失败(无控制终端,如 CI)时保持 nil,退回 os.Stdin
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		tty = f
	}
}

// prompt 打印提示并读取一行(空输入返回空串)。优先读 /dev/tty,保证 Agent 拉起的
// 向导里用户的回车确认总能送达;无 TTY 时退回 os.Stdin(兼容脚本/测试管道输入)。
func prompt(msg string) string {
	fmt.Print(msg)
	if tty != nil {
		if ttyScanner == nil {
			ttyScanner = bufio.NewScanner(tty)
		}
		if ttyScanner.Scan() {
			return ttyScanner.Text()
		}
		return ""
	}
	if stdin == nil {
		stdin = bufio.NewScanner(os.Stdin)
	}
	if stdin.Scan() {
		return stdin.Text()
	}
	return ""
}

var ttyScanner *bufio.Scanner
