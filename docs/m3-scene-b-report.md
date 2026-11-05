# M3 场景 B 开发与验收报告：每日 Bitable 增量 → 腾讯文档日报 → 推 IM

日期：2026-11-02 · 工作目录：`hub/` · 状态：**未 commit，待确认后提交**

## 一、结论

**M3 场景 B 已端到端跑通。** 真实凭据手动触发 4 次：首次建文档 + 推企微群成功；无新增时正确 skipped；同日二次有新增时**更新同一篇文档**（file_id 不变）并再次推送成功。`go build` / `go vet` / `go test ./...` 全绿（新增 report 包 6 个测试：回填、增量、空数据跳过、同日幂等、水位线持久化、limit 不重试 / transient 有限重试）。

日报文档（真实生成，可打开）：https://docs.qq.com/doc/DY2F1TmF5d2JNa1ZV

## 二、实现要点与文件清单

| 要求 | 实现 |
|---|---|
| 定时调度 | 进程内 cron（robfig/cron/v3），表达式/时区/开关走 `config.json` 的 `report` 段；手动入口 = 子命令 `-run-report [-report-date]` + 元工具 `hub.report.run(date?)` |
| 增量汇总 | 水位线存 SQLite `kv` 表（`report.last_success_unix`），重启不丢；无水位线时回填 `backfill_hours`（默认 24h）；拉取用 `bitable_v1_appTableRecord_search` + `automatic_fields:true`，按记录顶层 `created_time` 过滤，分页全量后本地过滤 |
| 日报生成 | 统一语义 `doc.create` → `tencent-docs.doc.create_with_markdown`（base64 Markdown）；标题 `群聊记录日报 YYYY-MM-DD`；正文 = 条数概览 + 按来源平台/操作人分组 + 明细列表 |
| 幂等 | `report_docs` 表存 date → file_id/url；同日重复触发走 `doc.insert_markdown` 更新同一篇（详见「残留」），不新建 |
| 推送 | 默认企微（`push_platforms: ["wecom"]`），feishu/dingtalk 可配；内容含标题、条数摘要、链接；复用 `semantic.SendText` |
| 可靠性 | 错误分类 limit（429/配额，**不重试**）/ config / transient（最多重试 2 次，2s/4s 退避）；每次运行（含 skipped/failed）写 `audits`（`tool='report.daily'`，summary 为完整 JSON：日期/条数/file_id/url/推送结果/错误分类） |
| 空数据 | 不建文档、不推送，水位线照常推进，审计留 `skipped` |

**新增/改动文件**：
- 新建 [report.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/report/report.go)（Pipeline + Downstream 抽象 + MCP 实现 + Markdown 生成 + 错误分类）
- 新建 [scheduler.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/report/scheduler.go)（cron 调度，优雅停止）
- 新建 [report_test.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/report/report_test.go)（6 个测试）
- 改 [sqlite.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/store/sqlite.go#L144-L182)：`kv` + `report_docs` 两表及读写方法
- 改 [config.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/config/config.go#L40-L48)：`ReportConfig`
- 改 [manager.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/internal/adapter/manager.go#L280)：导出 `IsLimitError`
- 改 [main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go#L237-L265)（pipeline 装配 + `hub.report.run` 元工具）、[main.go](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/cmd/hub/main.go#L294-L320)（`-run-report` 子命令 + cron 启动 + drain）
- 改 [config.json](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/config.json#L9-L16) / [config.example.json](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/config.example.json) / [.env.example](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/hub/.env.example#L36-L38)
- 改 [设计文档 §9](file:///Users/hanyechao/Documents/Trae-CN/temp_20261029_02/cross-platform-mcp-hub-design.md#L245-L264)：M3 运行配置说明
- `go.mod`：新增 `github.com/robfig/cron/v3`

## 三、实测记录（真实凭据）

| 时间 | 触发 | 结果 |
|---|---|---|
| 15:49 | `-run-report` 第 1 次 | **failed/transient**：Bitable 响应解析失败（search 参数结构用错，path/params 不分）→ 修复后重试；审计已留 failed 记录 ✓（失败不静默） |
| 15:52 | `-run-report` 第 2 次 | `skipped`：回填窗口 24h 内无新增（此前的测试记录均早于水位线解析窗口），不建文档不推送 ✓ |
| 15:55:07 | curl 注入 `#记录 M3 日报端到端验证记录`（企微群）| 202 落库 → worker 写 Bitable 成功（record_id=reczz28UjZP1OJtt）✓ |
| 15:55:28 | `-run-report` 第 3 次 | `success`：**records=1，创建文档 file_id=cauNaywbMkVU**，企微群推送 ok ✓ |
| 15:56:12 | `-run-report` 第 4 次 | `skipped`：无新增，**未新建文档**（幂等负路径）✓ |
| 15:57:14 | curl 注入 `#记录 M3 二次触发更新验证`（钉钉群来源）| 写 Bitable 成功（reczz28UjbYA9goK）✓ |
| 15:57:26 | `-run-report` 第 5 次 | `success`：**records=1，更新同一篇 file_id=cauNaywbMkVU（未新建）**，企微群推送 ok ✓ |

- 文档链接：https://docs.qq.com/doc/DY2F1TmF5d2JNa1ZV （首次创建返回的是协议相对 URL `//docs.qq.com/...`，已加归一化修正并修复库存量）
- 审计核查：`sqlite3 hub.db` 中 `audits` 表有全部 6 次运行记录（failed/skipped/success × 各状态、records、file_id、推送结果齐全）；`report_docs` 表仅 1 行（2026-11-02 → cauNaywbMkVU）
- Bitable 无重复写入：两次 success 各对应 1 条人工注入记录，汇总侧只读不写 ✓

## 四、残留问题与需要你确认的点

1. **「更新同一篇」的实现形态**：远程腾讯文档 MCP（227 个工具）**没有整篇覆写 API**（无 overwrite_doc_with_markdown/html）。同日重跑采用「同 file_id 顶部插入最新快照块」，链接不变、顶部永远是最新数据，但旧内容会留在下方。可接受即关闭；若要真正覆写需等官方 MCP 补 API 或改用 docx 原生动图 API 自研。
2. **请真机确认两件事**：① 打开上面的文档链接看排版；② 企微群应收到 2 条日报推送（15:55、15:57）。
3. **推送失败重试的副作用**：若「建/更文档成功但推送失败」，transient 重试会在文档顶部再插一个快照块（内容相同）。发生概率低，已在审计中可查；如介意可后续把推送移出重试域。
4. **skipped 也推进水位线**：空窗口会推进水位线（避免每次重扫空窗），意味着「水位线之后、运行之前」写入的记录一定被下次捕获，语义正确；但如果你想让空窗口**不**推进水位线（更保守），改一行即可。
5. **cron 表达式实测**：当前配 `0 18 * * *`（每天 18:00）。调度器启动日志已验证加载，未到真实触发时刻；如想立刻看 cron 路径，可临时改成 `*/2 * * * *` 观察一轮，或直接认可手动入口 + 单测覆盖。
6. **commit 授权**：本轮 M3 改动 + 上轮 P0/P1/P2 修复均在工作区未提交，确认后我一并提交（或分两个 commit）。
