# P0 / P1 / P2 修复报告

日期：2026-11-02 · 工作目录：`hub/` · 状态：**全部代码已落地，未 commit，待确认后提交**

## 一、结论

- **P0：3/3 完成**（P0-1 凭据出仓此前已完成，本次跳过）
- **P1：6/6 完成**
- **P2：7/7 完成**
- 质量门：`go build ./...` ✓、`go vet ./...` ✓、`go test ./...` ✓（adapter / event / store 三包全绿，含新增回归测试）
- 端到端冒烟：`#记录` 注入 → 落库 → worker → Bitable 写入 → 回执，链路不回退；重复投递与进程重启后均正确去重
- **修复过程中新发现并修复 1 个计划外 P0 级 bug**：事件队列缺少 processing 中间态，慢 handler（Bitable 写入约 5s）期间同一事件被调度循环重复认领，导致一条消息写入 Bitable **7 次**。已修复并加回归测试（见「计划外修复」）
- **残留/限制**（不影响验收，见末节）：wecom 长连接状态 Hub 侧只能通过事件到达间接观测，体检表显示 `-`；事件乱序只记录不排序（原方案即如此）

## 二、逐条清单（问题 → 修复 → 证据 → 验证）

### P0-2 企微适配器可靠投递 + Hub 先落库再 ACK
- **修复**：适配器侧内存队列 + 指数退避（1s→30s 封顶），`HUB_EVENT_RETRY_MAX`（默认 5）达上限后 `console.error` 丢弃，防无限堆积；Hub 侧 `Publish` 落库成功才返回，适配器收到 2xx 即代表已持久化。
- **证据**：[server.js](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/adapters/wecom-aibot-mcp/server.js) L28-50（重试循环 + X-Hub-Secret 头）；[bus.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/bus.go) L76-90（Publish 落库即 ACK）；[sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go) L151-159（EnqueueEvent）
- **验证**：`TestIngestPersistsBeforeReturn`（落库先于返回）；冒烟 curl 注入返回 202，日志「事件已落库」先于任何 handler 日志

### P0-3 群/会话目标管理配置驱动 + 会话枚举
- **修复**：`push_targets` 入 config.json（`$FEISHU_CHAT_ID` / `$DINGTALK_OPEN_CONVERSATION_ID` / `$WECOM_CHAT_ID`，值在 .env）；新增元工具 `hub.chats`：飞书实时调 `im_v1_chat_list` 并沉淀 SQLite，企微/钉钉从消息帧自动缓存（`noteChat`），输出 seen 列表 + 当前 push_targets。
- **证据**：[config.json](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/config.json) L3-8；[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L102-127；[bus.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/bus.go) L92-96；[sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go) L229-248
- **验证**：`TestAuditAndChats`（会话 upsert）；冒烟日志中事件消费后 chats 表沉淀 `wrKSFIDgAA4PBzkx5yw1OeRltHkrIXYQ`

### P1-1 去重窗口持久化（24h）
- **修复**：dedup 表落 SQLite，`(platform, source_id)` 主键兜底，TTL 24h（设计文档 §5；旧代码硬编码 10min 已废弃）；启动 `PruneDedup` 清理过期行；过期事件允许重处理，窗口内重复丢弃。
- **证据**：[sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go) L48-50、L120-139；[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L31（`dedupTTL = 24 * time.Hour`）
- **验证**：`TestDedupWindow` / `TestDedupPersistsAcrossRestart` / `TestDedupExpired`；冒烟：重启 Hub 后重发同 source_id → 200 + 日志「重复事件已丢弃」，Bitable 0 次新增写入

### P1-2 事件处理异步化
- **修复**：Ingress 只做归一化+去重+落库即返回；Dispatcher worker 池（`workers` 可配，默认 4）异步消费；失败退避重试（attempts*2s），超 maxAttempts（3）进 dead 死信保留现场。
- **证据**：[ingress.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/ingress.go) L26-42；[bus.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/bus.go) L99-128、L161-194；[sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go) L206-217
- **验证**：`TestEventQueueLifecycle`（退避窗口不可领取 / done 不再领取）；冒烟 202 响应在 Bitable 写入（约 5s）之前返回

### P1-3 启动路由自检
- **修复**：`ValidateRoutes` 遍历 routes 与已挂载工具逐一比对，缺失/多余按 ERROR 输出并对平台标 degraded，汇总进 `hub.platforms`/`hub.health` 快照；核心比对逻辑抽为纯函数 `CheckRoutes` 可单测。
- **证据**：[manager.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/adapter/manager.go) L80-130；[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L75
- **验证**：`TestCheckRoutes`（platform_not_mounted / tool_not_found）；本轮冒烟日志「路由自检通过 routes=3」（上一轮 wecom 挂载失败时曾正确报 ERROR，验证过负路径）

### P1-4 长连接健康状态 + 优雅退出信号
- **修复**：`health.Registry` 逐平台维护 connected/last_event_at/last_error；飞书 WS、钉钉 Stream 上下线均上报；企微适配器监听 `disconnected` 复位 authed 触发 SDK 重连；新增 `hub.health` 元工具；main 用 `signal.NotifyContext(SIGINT/SIGTERM)`。
- **证据**：[health.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/health/health.go)；[feishu.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/feishu.go) L26、L60-65；[dingtalk.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/dingtalk.go)；[server.js](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/adapters/wecom-aibot-mcp/server.js) L80；[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L39、L94-99
- **验证**：冒烟体检表 feishu/dingtalk「长连接 up」；`hub.health` 输出快照

### P1-5 凭据体检表
- **修复**：启动末尾输出每平台「凭据 ✓/✗ 挂载 ✓/✗ 长连接」；`.env` 缺失/加载失败升级 ERROR；打印前等待 1.5s 让长连接上报状态，避免全显示 `-`。
- **证据**：[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L44-50、L255-257、L282-306；[config.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/config/config.go)（`ErrDotEnvMissing`）
- **验证**：冒烟输出四平台全 ✓ 表格（见冒烟日志）

### P1-6 审计与 ID 映射落 SQLite
- **修复**：mappings/audits 两表与去重/事件队列/会话缓存共用 `hub.db`（WAL 模式，busy_timeout 5s）；Store 接口不变，内存实现保留供测试。
- **证据**：[sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go) L40-47、L73-117；调用点 [main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L154-158
- **验证**：`TestAuditAndChats`

### P2-1 /events 鉴权
- **修复**：配置 `HUB_EVENT_SECRET` 时强制校验 `X-Hub-Secret`；未配置时拒绝绑定非回环地址并 WARN 提示风险。本轮已在 .env 生成随机密钥（未入库）。
- **证据**：[ingress.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/ingress.go) L44-51、L70-83
- **验证**：`TestHTTPAuth`；冒烟：无头 401、错误头 401、正确头 202

### P2-2 飞书消息类型兼容
- **修复**：按 msg_type 分支；text 正常入 `im.message`，图片/富文本/合并转发等产生 `im.message.unsupported` 事件，订阅 handler 回执「⚠️ 暂不支持 X 类型消息」。
- **证据**：[feishu.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/feishu.go) L18-52；回执 handler [main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L234-246
- **验证**：单测覆盖 Normalize 归一； unsupported 分支靠代码路径审查 + 回执 handler 与已验证的 SendText 同链路（未发真实图片消息，建议真机随手发一张图片确认回执）

### P2-3 钉钉 Stream panic recover + 退避重启
- **修复**：goroutine 内 `runDingTalkStream` recover 转错误，外层 1s→32s 指数退避重连；修掉「`cli.Start` 连接建立即返回 nil 被误判退出」造成的重连风暴（返回 nil 后阻塞等 ctx 取消）。
- **证据**：[dingtalk.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/dingtalk.go) L18-45、L48-82
- **验证**：冒烟日志钉钉连接稳定，无抖动重连（此前版本每秒级重连）

### P2-4 环境变量按前缀透传
- **修复**：ServerConfig 新增 `env_prefixes`（大小写不敏感 HasPrefix），与既有 `env` 白名单并存（向后兼容）；钉钉配 `DINGTALK`、企微配 `WECOM_`。
- **证据**：[manager.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/adapter/manager.go) L188-193；[config.json](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/config.json)
- **验证**：`TestBuildEnvPrefixPassthrough`

### P2-5 腾讯文档配额治理
- **修复**：`CallTool` 全平台计数（`AddCall`）；错误文本/200 响应体命中 429/rate limit/quota/too many requests/频率/超限 特征时 `AddLimitError` + `slog.Error("⚠️ 平台限流/配额告警")`；计数暴露于 `hub.health`。
- **证据**：[manager.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/adapter/manager.go) L238-261
- **验证**：`TestLooksLikeLimitError`（含中英文特征）

### P2-6 事件总线健壮性
- **修复**：dispatchOne 对每个 handler 独立 recover + 30s 超时，单 handler 出错只记 errs 不中断其余订阅者；任一失败则 MarkRetry 记录原因，超限进 dead。
- **证据**：[bus.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/bus.go) L161-194
- **验证**：`TestDispatchIsolationAndRetry`（panic handler 隔离 + 死信）

### P2-7 优雅退出
- **修复**：收到信号 → HTTP `Shutdown(5s)` → `dispatcher.Stop(10s)`（拒收新事件 + 等在途）→ 关闭下游连接。
- **证据**：[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go) L262-278；[bus.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/event/bus.go) L132-142
- **验证**：冒烟 `pkill` 后日志出现完整 drain 序列「收到退出信号…Hub 已停止」，无 panic

## 三、计划外修复（冒烟新发现）

**事件队列重复消费（P0 级）**：一条 `#记录` 被写入 Bitable 7 次。根因：`ClaimPending` 只读不改状态，慢 handler 执行期间调度 ticker 每 1s 重复认领同一行。
**修复**：事务内原子认领并置 `processing`；`MarkRetry` 显式回到 `pending`；启动 `RecoverProcessing` 回收崩溃残留；新增回归断言「processing 中的事件不应被重复领取」。
**证据**：[sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go) L163-198、L208；[sqlite_test.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite_test.go) L72-75
**验证**：修复后同一事件仅 1 次 Bitable 写入（`grep -c '已写入 Bitable'` = 1）。

另修复两个适配器回归（此前版本不存在）：package.json 补 `"type":"module"` + `@modelcontextprotocol/sdk`/`zod` 依赖；WSClient 构造去掉不兼容的 logger 选项。

## 四、测试结果

```
ok  github.com/example/mcp-hub/internal/adapter
ok  github.com/example/mcp-hub/internal/event   2.670s
ok  github.com/example/mcp-hub/internal/store   0.934s
```
（cmd/hub、config、health、semantic 无测试文件；build + vet 全过）

## 五、改动文件

| 文件 | 变更 |
|---|---|
| `internal/store/sqlite.go` | 新建：五表 + 去重/队列/审计/会话/映射，processing 原子认领 |
| `internal/event/bus.go` | 重写：Dispatcher worker 池、handler 隔离、优雅停止 |
| `internal/event/ingress.go` | 重写：落库即 ACK、共享密钥鉴权、回环保护 |
| `internal/event/feishu.go` | 重写：msg_type 分支、健康上报 |
| `internal/event/dingtalk.go` | 重写：recover + 退避重连、修复重连风暴 |
| `internal/health/health.go` | 新建：健康注册表 |
| `internal/adapter/manager.go` | 路由自检、前缀透传、限流分类告警、调用计数 |
| `internal/config/config.go` | EnvPrefixes / DBPath / Workers / PushTargets、ErrDotEnvMissing |
| `cmd/hub/main.go` | 重写：信号处理、体检表、hub.health/hub.chats、drain 顺序 |
| `adapters/wecom-aibot-mcp/server.js` | 重写：可靠上报队列、disconnect 复位、ESM 修复 |
| `adapters/wecom-aibot-mcp/package.json` | type:module + 补 MCP SDK/zod 依赖 |
| `config.json` | db_path/workers/push_targets/env_prefixes/HUB_EVENT_SECRET 透传 |
| `.env` / `.env.example` | 补 HUB_EVENT_SECRET、推送目标等（.env 不入库） |
| `.gitignore` | 补 `*.db` / `*.db-wal` / `*.db-shm` |
| 测试 ×3 | `store/sqlite_test.go`、`event/ingress_test.go`、`adapter/manager_test.go` |

## 六、需要你确认的点

1. **commit 授权**：全部改动在工作区未提交，确认后我按批（P0 / P1 / P2 / 计划外修复）或单 commit 提交，你定。
2. **冒烟产生的数据**：Bitable 收集表里有本次验证写入的 8 条记录（7 条重复消费 bug 期间 + 1 条修复后），需要我删掉吗？本地 `hub.db` 我已重置过一次，当前只含最终验证的 1 条事件。
3. **P2-2 真机确认**：建议在飞书群里随手发一张图片，确认收到「暂不支持」回执（代码路径已通，但未发真实图片）。
4. **残留限制**（接受即关闭）：① wecom 长连接在适配器子进程内，Hub 体检表只能显示 `-`，靠事件到达间接观测；② 事件乱序只记录不重排（原方案范围）；③ `hub.chats` 的企微/钉钉会话依赖「先收到过消息」才可见。
