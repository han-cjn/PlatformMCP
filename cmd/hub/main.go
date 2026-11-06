// Hub 主进程:挂载下游 MCP Server,聚合工具,以 stdio 对外暴露统一语义层。
//
// 运行:go run ./cmd/hub -config config.json
// 凭据缺失的平台自动降级跳过,不影响其他平台挂载;启动末尾输出凭据体检表(P1-5)。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/example/mcp-hub/internal/adapter"
	"github.com/example/mcp-hub/internal/config"
	"github.com/example/mcp-hub/internal/event"
	"github.com/example/mcp-hub/internal/health"
	"github.com/example/mcp-hub/internal/onboard"
	"github.com/example/mcp-hub/internal/report"
	"github.com/example/mcp-hub/internal/semantic"
	"github.com/example/mcp-hub/internal/store"
)

const dedupTTL = 24 * time.Hour // 设计文档 §5:去重窗口 24h

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	runReport := flag.Bool("run-report", false, "手动执行一次日报(M3 场景 B)后退出,便于验收/补跑")
	reportDate := flag.String("report-date", "", "配合 -run-report 指定归属日期(YYYY-MM-DD),默认今天")
	onboardPlatform := flag.String("onboard", "", "进入平台接入向导(如 -onboard feishu):引导→校验→写.env→热重载")
	onboardBrowser := flag.Bool("onboard-browser", false, "向导启用浏览器执行器(需 Chrome 带 --remote-debugging-port=9222 --user-data-dir=~/.chrome-debug-profile 启动并已登录平台;临时 profile 会丢登录态,已做检测拦截)")
	onboardVerify := flag.Bool("onboard-verify-only", false, "配合 -onboard:跳过向导,直接用现有凭据做真连校验+写盘+热重载(日常体检)")
	sseAddr := flag.String("sse", "", "以常驻 HTTP 服务暴露 MCP(如 -sse 127.0.0.1:9781):/sse 兼容 SSE 客户端,/mcp 走 StreamableHTTP;为空则 stdio 模式")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil))) // 日志走 stderr,stdout 留给 MCP 协议

	// 优雅退出:SIGTERM/SIGINT 触发 drain(P2-7)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hr := health.NewRegistry()

	if err := config.LoadDotEnv(".env"); err != nil {
		if errors.Is(err, config.ErrDotEnvMissing) {
			slog.Error(".env 不存在:所有凭据将缺失,全部平台降级", "path", ".env")
		} else {
			slog.Error(".env 加载失败", "err", err)
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("配置加载失败", "err", err)
		os.Exit(1)
	}

	// 持久化存储(P0-2/P1-1/P1-6:去重+事件队列+审计+会话缓存共用同一 DB)
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = "hub.db"
	}
	st, err := store.OpenSQLite(dbPath, dedupTTL)
	if err != nil {
		slog.Error("打开持久化存储失败", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()
	st.PruneDedup()
	st.RecoverProcessing() // 回收上次崩溃残留的 processing 事件

	mgr := adapter.NewManager(ctx, cfg.Servers, hr)
	defer mgr.Close()
	registry := semantic.NewRegistry(cfg.Routes)

	// P1-3:路由表与真实工具启动自检,不一致按 ERROR 输出并标注 degraded
	routeIssues := mgr.ValidateRoutes(cfg.Routes, hr)

	// 接入向导模式:引导→真连校验→写 .env→热重载(完成即退出,不启动事件桥/stdio 服务)
	if *onboardPlatform != "" {
		runOnboard(ctx, *onboardPlatform, *onboardBrowser, *onboardVerify, *configPath, cfg, hr, mgr)
		return
	}

	hub := server.NewMCPServer("mcp-hub", "0.2.0", server.WithToolCapabilities(false))

	// 元工具:平台与工具盘点
	hub.AddTool(mcp.NewTool("hub.platforms",
		mcp.WithDescription("列出已成功挂载的下游平台(含 degraded 标注)"),
	), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, _ := json.Marshal(hr.Snapshot())
		return mcp.NewToolResultText(string(out)), nil
	})
	hub.AddTool(mcp.NewTool("hub.tools",
		mcp.WithDescription("列出全部已挂载工具(含平台命名空间前缀)"),
	), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, _ := json.Marshal(mgr.Tools())
		return mcp.NewToolResultText(string(out)), nil
	})

	// P1-4:长连接与调用健康状态
	hub.AddTool(mcp.NewTool("hub.health",
		mcp.WithDescription("各平台健康状态:凭据/挂载/长连接/最近事件/调用计数/限流计数"),
	), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, _ := json.MarshalIndent(hr.Snapshot(), "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	})

	// P0-3:会话枚举——飞书实时拉取群列表,企微/钉钉来自消息帧沉淀的缓存
	hub.AddTool(mcp.NewTool("hub.chats",
		mcp.WithDescription("枚举可用会话:飞书走 im_v1_chat_list;企微/钉钉来自已接收消息帧缓存"),
	), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := map[string]any{"seen": st.Chats()}
		if resp, err := mgr.CallTool(ctx, "feishu", "im_v1_chat_list", map[string]any{"params": map[string]any{"page_size": 50}}); err == nil {
			var parsed struct {
				Data struct {
					Items []struct {
						ChatID string `json:"chat_id"`
						Name   string `json:"name"`
					} `json:"items"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(resp), &parsed) == nil {
				for _, it := range parsed.Data.Items {
					st.NoteChat("feishu", it.ChatID, it.Name)
				}
			}
			result["feishu_raw"] = resp
		} else {
			result["feishu_error"] = err.Error()
		}
		result["push_targets"] = cfg.PushTargets
		out, _ := json.MarshalIndent(result, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	})

	// 统一语义工具:capability 来自配置路由表,参数 = platform + JSON payload
	for _, capability := range registry.Capabilities() {
		cap := capability
		hub.AddTool(mcp.NewTool(cap,
			mcp.WithDescription(fmt.Sprintf("统一语义:%s(按 platform 路由到对应下游工具)", cap)),
			mcp.WithString("platform", mcp.Required(), mcp.Description("目标平台:feishu / dingtalk / wecom / tencent-docs")),
			mcp.WithString("payload", mcp.Required(), mcp.Description("下游工具参数,JSON 对象字符串")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			platform, err := req.RequireString("platform")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			payloadStr, err := req.RequireString("payload")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(payloadStr), &args); err != nil {
				return mcp.NewToolResultError("payload 非法 JSON: " + err.Error()), nil
			}
			downstream, err := registry.Resolve(cap, platform)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			text, callErr := mgr.CallTool(ctx, platform, downstream, args)
			_ = st.AppendAudit(store.AuditRecord{
				Time: time.Now(), Tool: cap, Target: platform,
				Summary:   fmt.Sprintf("downstream=%s", downstream),
				Succeeded: callErr == nil, Err: errStr(callErr),
			})
			if callErr != nil {
				return mcp.NewToolResultError(callErr.Error()), nil
			}
			return mcp.NewToolResultText(text), nil
		})
		slog.Info("统一语义工具已注册", "capability", cap)
	}

	// ===== 事件桥:Ingress(去重+落库即ACK)→ Dispatcher(worker 池)→ 规则 =====
	dispatcher := event.NewDispatcher(st, cfg.Workers, 3)
	dispatcher.Start(ctx)
	ingress := event.NewIngress(dispatcher, st, os.Getenv("HUB_EVENT_SECRET"))

	// HTTP 上报端点(企微适配器等进程外事件源;P2-1 鉴权,P2-7 优雅退出)
	httpAddr := os.Getenv("HUB_EVENT_ADDR")
	if httpAddr == "" {
		httpAddr = "127.0.0.1:9780"
	}
	httpSrv, err := event.NewHTTPServer(httpAddr, ingress)
	if err != nil {
		slog.Error("事件端点配置非法", "err", err)
		os.Exit(1)
	}
	go func() {
		slog.Info("事件 HTTP 端点启动", "addr", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("事件 HTTP 端点退出", "err", err)
		}
	}()

	// 飞书 WS 长连接(凭据存在才启动,缺失自动降级并计入体检表)
	if id, sec := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET"); id != "" && sec != "" {
		hr.SetCredentials("feishu", true)
		go func() {
			if err := event.StartFeishuWS(ctx, id, sec, ingress, hr); err != nil {
				slog.Error("飞书 WS 启动失败(降级)", "err", err)
			}
		}()
	} else {
		hr.SetCredentials("feishu", false)
		slog.Error("飞书凭据缺失(FEISHU_APP_ID/FEISHU_APP_SECRET),长连接降级")
	}

	// 钉钉 Stream 长连接
	if id, sec := os.Getenv("DINGTALK_Client_ID"), os.Getenv("DINGTALK_Client_Secret"); id != "" && sec != "" {
		hr.SetCredentials("dingtalk", true)
		if err := event.StartDingTalkStream(ctx, id, sec, ingress, hr); err != nil {
			slog.Error("钉钉 Stream 启动失败(降级)", "err", err)
		}
	} else {
		hr.SetCredentials("dingtalk", false)
		slog.Error("钉钉凭据缺失(DINGTALK_Client_ID/DINGTALK_Client_Secret),长连接降级")
	}

	// 企微凭据体检(长连接在适配器子进程内,Hub 侧通过事件到达率观测)
	hr.SetCredentials("wecom", os.Getenv("WECOM_BOT_ID") != "" && os.Getenv("WECOM_BOT_SECRET") != "")
	hr.SetCredentials("tencent-docs", os.Getenv("TENCENT_DOCS_TOKEN") != "")

	// 场景 A 规则:#记录 → 飞书 Bitable → 回执
	if appToken := os.Getenv("BITABLE_APP_TOKEN"); appToken != "" {
		rule := &event.RecordRule{
			Target: event.BitableTarget{
				AppToken: appToken,
				TableID:  os.Getenv("BITABLE_TABLE_ID"),
				Field:    getenvDefault("BITABLE_CONTENT_FIELD", "内容"),
			},
			Reg: registry, Mgr: mgr,
		}
		dispatcher.Subscribe(rule.Handle)
		slog.Info("#记录 规则已启用", "table", rule.Target.TableID)
	} else {
		slog.Warn("BITABLE_APP_TOKEN 未配置,#记录 规则停用")
	}

	// ===== M3 场景 B:日报联动(Bitable 增量 → 腾讯文档 → 推 IM)=====
	var pipeline *report.Pipeline
	if appToken := os.Getenv("BITABLE_APP_TOKEN"); appToken != "" {
		pipeline = report.NewPipeline(st, &report.MCPDownstream{
			Mgr: mgr, Reg: registry,
			AppToken: appToken, TableID: os.Getenv("BITABLE_TABLE_ID"),
			CreatedField: getenvDefault(cfg.Report.CreatedField, "创建时间"),
		}, cfg.Report, cfg.PushTargets)

		// 手动触发入口:元工具 hub.report.run(可选 date 参数,默认今天)
		hub.AddTool(mcp.NewTool("hub.report.run",
			mcp.WithDescription("手动执行一次日报:汇总 Bitable 增量 → 生成/更新腾讯文档 → 推送目标群"),
			mcp.WithString("date", mcp.Description("归属日期 YYYY-MM-DD,默认今天")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			now := time.Now()
			if d := req.GetString("date", ""); d != "" {
				t, err := time.ParseInLocation(time.DateOnly, d, pipeline.Location())
				if err != nil {
					return mcp.NewToolResultError("date 非法,应为 YYYY-MM-DD: " + err.Error()), nil
				}
				now = t
			}
			res := pipeline.Run(ctx, now, "manual")
			out, _ := json.MarshalIndent(res, "", "  ")
			return mcp.NewToolResultText(string(out)), nil
		})
	} else {
		slog.Warn("BITABLE_APP_TOKEN 未配置,日报联动(M3)停用")
	}

	// P2-2:不支持的消息类型给明确回执,不静默
	dispatcher.Subscribe(func(ctx context.Context, e event.Event) error {
		if e.Type != "im.message.unsupported" {
			return nil
		}
		chatID, _ := e.Payload["chat_id"].(string)
		msgType, _ := e.Payload["msg_type"].(string)
		if chatID == "" {
			return nil
		}
		_, err := semantic.SendText(ctx, registry, mgr, e.Platform, chatID,
			fmt.Sprintf("⚠️ 暂不支持 %s 类型消息,请发送文本", msgType))
		return err
	})

	dispatcher.Subscribe(func(_ context.Context, e event.Event) error {
		hr.TouchEvent(e.Platform)
		slog.Info("事件已接收", "platform", e.Platform, "type", e.Type, "source_id", e.SourceID)
		return nil
	})

	// P1-5:启动体检表(稍等长连接上报 StreamUp,避免全部显示 "-")
	time.Sleep(1500 * time.Millisecond)
	printHealthTable(hr, routeIssues)

	slog.Info("MCP Hub 启动", "platforms", mgr.Platforms(), "tools", len(mgr.Tools()), "db", dbPath)

	// 手动补跑模式:执行一次日报后退出(不进 stdio 服务)
	if *runReport {
		if pipeline == nil {
			slog.Error("日报联动未启用(BITABLE_APP_TOKEN 缺失)")
			os.Exit(1)
		}
		now := time.Now()
		if *reportDate != "" {
			t, err := time.ParseInLocation(time.DateOnly, *reportDate, pipeline.Location())
			if err != nil {
				slog.Error("report-date 非法,应为 YYYY-MM-DD", "err", err)
				os.Exit(1)
			}
			now = t
		}
		res := pipeline.Run(ctx, now, "manual-cli")
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(os.Stderr, "---- 日报运行结果 ----")
		fmt.Fprintln(os.Stderr, string(out))
		if res.Status == "failed" {
			os.Exit(1)
		}
		return
	}

	// 内置 cron 调度(表达式/时区/开关走 config.json 的 report 段)
	scheduler := report.StartScheduler(ctx, pipeline)

	// MCP 服务出口:stdio(嵌入宿主进程)或常驻 HTTP(SSE + StreamableHTTP,供多客户端接入)
	serveErr := make(chan error, 1)
	if *sseAddr != "" {
		if !strings.HasPrefix(*sseAddr, "127.0.0.1:") && !strings.HasPrefix(*sseAddr, "localhost:") {
			slog.Warn("⚠️ -sse 绑定非回环地址:MCP 端点无鉴权,暴露到网络有风险,建议仅本机使用", "addr", *sseAddr)
		}
		mux := http.NewServeMux()
		sseSrv := server.NewSSEServer(hub, server.WithSSEEndpoint("/sse")) // 旧 SSE 客户端
		stSrv := server.NewStreamableHTTPServer(hub)                       // 新 StreamableHTTP 客户端
		mux.Handle("/", sseSrv)                                            // 兜底:处理 /sse 与其 message 回传路径
		mux.Handle("/mcp", stSrv)
		mcpHTTP := &http.Server{Addr: *sseAddr, Handler: mux}
		go func() {
			slog.Info("MCP Hub HTTP 服务启动", "addr", *sseAddr, "sse", "http://"+*sseAddr+"/sse", "streamable", "http://"+*sseAddr+"/mcp")
			serveErr <- mcpHTTP.ListenAndServe()
		}()
		// 退出时关闭 MCP HTTP
		defer func() {
			shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = mcpHTTP.Shutdown(shCtx)
		}()
	} else {
		go func() { serveErr <- server.ServeStdio(hub) }() // stdio 主循环;父进程关闭 stdin 时退出
	}
	select {
	case err := <-serveErr:
		if err != nil {
			slog.Error("Hub 退出", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("收到退出信号,开始 drain")
	}

	// drain:HTTP 端点 → 调度器 → dispatcher(等在途任务)→ 下游连接
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	scheduler.Stop()
	dispatcher.Stop(10 * time.Second)
	slog.Info("Hub 已停止")
}

// printHealthTable 启动体检表:每平台 凭据/挂载/长连接 一览(P1-5)。
func printHealthTable(hr *health.Registry, routeIssues []adapter.RouteIssue) {
	snap := hr.Snapshot()
	platforms := []string{"feishu", "dingtalk", "wecom", "tencent-docs"}
	check := func(ok bool) string {
		if ok {
			return "✓"
		}
		return "✗"
	}
	fmt.Fprintln(os.Stderr, "==================== 凭据体检表 ====================")
	for _, p := range platforms {
		st := snap[p]
		stream := st.Stream
		if stream == "" {
			stream = "-"
		}
		fmt.Fprintf(os.Stderr, "  %-14s 凭据 %s  挂载 %s  长连接 %s\n",
			p, check(st.Credentials), check(st.Mounted), stream)
	}
	if len(routeIssues) > 0 {
		fmt.Fprintf(os.Stderr, "  路由自检问题 %d 条(详见上方 ERROR 日志)\n", len(routeIssues))
	}
	fmt.Fprintln(os.Stderr, "==================================================")
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runOnboard 接入向导入口:完成后重载配置(http headers 需重新展开)+热重载挂载+路由自检+体检表。
func runOnboard(ctx context.Context, platform string, useBrowser, verifyOnly bool, configPath string, cfg *config.Config, hr *health.Registry, mgr *adapter.Manager) {
	p, ok := onboard.FindPortal(platform)
	if !ok {
		slog.Error("未知平台,可选: feishu / dingtalk / wecom / tencent-docs", "platform", platform)
		os.Exit(1)
	}
	opts := onboard.Options{
		Browser:    useBrowser,
		VerifyOnly: verifyOnly,
		Reload: func(env map[string]string) error {
			// http 类平台(如 tencent-docs)的 headers 展开发生在 config.Load,
			// 写盘+Setenv 之后必须重新 Load 才能带上新 Token
			fresh, err := config.Load(configPath)
			if err != nil {
				return err
			}
			for _, sc := range fresh.Servers {
				if sc.Name == platform {
					return mgr.Reload(ctx, sc)
				}
			}
			return fmt.Errorf("config.json 中找不到平台 %s 的 servers 定义", platform)
		},
	}
	if err := onboard.Run(p, opts); err != nil {
		slog.Error("接入向导未完成", "platform", platform, "err", err)
		os.Exit(1)
	}
	// 收尾:路由自检 + 体检表(热重载后的最新状态)
	fresh, err := config.Load(configPath)
	if err == nil {
		cfg = fresh
	}
	routeIssues := mgr.ValidateRoutes(cfg.Routes, hr)
	// 凭据体检:onboard 模式提前退出,这里按环境变量统一标注所有平台
	hr.SetCredentials("feishu", os.Getenv("FEISHU_APP_ID") != "" && os.Getenv("FEISHU_APP_SECRET") != "")
	hr.SetCredentials("dingtalk", os.Getenv("DINGTALK_Client_ID") != "" && os.Getenv("DINGTALK_Client_Secret") != "")
	hr.SetCredentials("wecom", os.Getenv("WECOM_BOT_ID") != "" && os.Getenv("WECOM_BOT_SECRET") != "")
	hr.SetCredentials("tencent-docs", os.Getenv("TENCENT_DOCS_TOKEN") != "")
	printHealthTable(hr, routeIssues)
}
