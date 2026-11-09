# MCP Hub

跨平台协作中台：用 MCP 协议把飞书、钉钉、企业微信、腾讯文档的消息、文档、表格能力统一收拢成一套接口，AI 客户端一次编写、多平台可用。

## 能做什么

### IM 消息

- 发送文本/Markdown 消息到飞书、钉钉、企微群聊
- 飞书自动转换为富文本 post，钉钉/企微原生支持 markdown

### 文档

- 腾讯文档：创建/追加/搜索文档
- 飞书：导入整篇 markdown 创建文档、搜索文档
- 明确标注不支持的能力（如飞书无追加工具），而非静默失败

### 表格

- 飞书多维表格：查询/插入/更新记录（统一筛选操作符、幂等写入）
- 钉钉智能表格：查询/插入/更新记录（operator_id 透传）
- 统一领域模型，操作符词汇自动翻译

### 事件桥

- 飞书 WebSocket 长连接接收消息
- 钉钉 Stream 模式接收消息
- 企微智能机器人收发一体
- 事件统一落库、去重、重试，支持 `#记录` 指令写入表格

### 日报

- 定时汇总飞书 Bitable 新增记录
- 生成腾讯文档日报（同日幂等更新）
- 推送链接到配置的目标群

### 接入引导

- 交互式向导配置四平台凭据
- 真连校验（换 token / MCP initialize / WS 认证）
- 原子写入 `.env`，热重载无需重启

## 能力矩阵

```
capability    dingtalk      feishu        tencent-docs  wecom
----------------------------------------------------------------------
doc.append    ✗             ✗             ✓             ✗
doc.create    ✗             ✓             ✓             ✗
doc.search    ✗             ✓             ✓             ✗
im.send_text  ✓             ✓             ✗             ✓
table.create  ✓             ✓             ✗             ✗
table.query   ✓             ✓             ✗             ✗
table.update  ✓             ✓             ✗             ✗
```

✓ 支持且已挂载  ○ 支持但未挂载  - 未声明  ✗ 明确不支持

## 快速开始

### 1. 准备凭据

复制 `.env.example` 为 `.env`，填入各平台凭据：

```bash
cp .env.example .env
```

需要哪些凭据见 [.env.example](.env.example) 注释，或参考[接入向导](#接入向导)。

### 2. 配置平台连接

复制 `config.example.json` 为 `config.json`：

```bash
cp config.example.json config.json
```

配置文件定义了下游 MCP Server 的启动方式、语义路由、推送目标等。`$ENV` 语法会从 `.env` 展开。

### 3. 运行

**stdio 模式**（默认，嵌入宿主进程）：

```bash
go run ./cmd/hub -config config.json
```

**常驻 HTTP 模式**（多客户端接入，SSE + StreamableHTTP）：

```bash
go run ./cmd/hub -config config.json -sse 127.0.0.1:9781
```

**接入向导**（交互式配置凭据）：

```bash
go run ./cmd/hub -config config.json -onboard feishu
```

**手动触发日报**：

```bash
go run ./cmd/hub -config config.json -run-report [-report-date 2026-11-06]
```

### 4. 接入 WorkBuddy（或其他 MCP 宿主）

在 `~/.workbuddy/mcp.json` 的 `mcpServers` 中加入：

```json
"mcp-hub": {
  "command": "/Users/hanyechao/hub/bin/hub",
  "args": ["-config", "/Users/hanyechao/hub/config.json"],
  "env": {
    "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
    "HOME": "/Users/hanyechao"
  }
}
```

要点：

- **先 `make build`** 产出 `bin/hub`，stdio 模式用编译产物而非 `go run`（避免首次编译延迟触发宿主超时）
- `env` 会**整体替换**进程环境：`PATH` 必须包含 node/npx 实际所在目录（Apple Silicon 为 `/opt/homebrew/bin`），`HOME` 必须存在（npx 缓存依赖 `~/.npm`）
- 无需指定 `cwd`：Hub 启动后会自动把工作目录锚定到 `-config` 所在目录，`.env`、`db_path`、企微适配器相对路径均与宿主 cwd 解耦
- 保存后到连接器管理页面对 `mcp-hub` 点「信任/启用」

## 统一语义层

客户端不需要知道各平台 API 的差异，只写领域模型：

```json
{
  "platform": "feishu",
  "target": "oc_xxx",
  "text": "**粗体** 消息",
  "format": "markdown"
}
```

飞书会自动转换为 post 富文本，钉钉/企微原生渲染。平台差异由语义层吸收：

- **声明式映射**：简单字段映射（`Args`/`Const`/`Encode`）
- **Adapter**：复杂转换（base64 编码、markdown→post、操作符词汇翻译、批量/单条结构组装）

### 支持的语义能力

| 能力 | 领域入参 | 说明 |
|---|---|---|
| `im.send_text` | `target`, `text`, `title?`, `format?` | 发送消息（plain/markdown） |
| `doc.create` | `title`, `markdown` | 创建文档 |
| `doc.append` | `doc_id`, `markdown`, `position?` | 追加内容（仅腾讯） |
| `doc.search` | `keyword` | 搜索文档 |
| `table.query` | `app`, `table`, `filters?`, `conjunction?`, `page_size?` | 查询记录 |
| `table.create` | `app`, `table`, `records`, `idempotency_key?` | 插入记录（幂等） |
| `table.update` | `app`, `table`, `records` | 更新记录 |

## 接入向导

**自动引导**：启动时检测到某平台缺凭据，终端直跑（有 TTY）会自动逐个进入交互向导，完成即热重载、全部接入后继续启动。WorkBuddy 等 stdio 宿主（stdin 是管道）跳过交互，只在 stderr 打一条指引日志。可用 `HUB_NO_AUTO_ONBOARD=1` 关闭（CI/测试环境）。

**Agent 驱动的接入**（stdio 宿主推荐）：以下工具串成全自动链路，用户只需在浏览器里扫码、确认应用名与点击按钮，**无需切换到终端执行任何命令**：

| 工具 | 作用 |
|---|---|
| `hub.onboard.status()` | 返回 `missing` / `configured` 平台清单，供 Agent 弹选择框 |
| `hub.onboard.start(platform)` | 起带调试端口的 Chrome 并打开开放平台页面；**带状态感知**，已登录/已在详情页时不再列扫码建应用 |
| `hub.onboard.watch(platform)` | 轮询 CDP 标签 URL，监听扫码完成 / 进入应用详情页 |
| `hub.onboard.create_app(platform, app_name?, confirmed?)` | 代建应用：幂等检查 + 显式确认门 + 自动接 `capture`（当前仅飞书） |
| `hub.onboard.capture(platform)` | 读 App ID/Secret → 校验 → 写 `.env` → 热重载挂载 |

`start` 会先跑一次 `watch`：若已停在应用详情页，返回 `phase=ready_to_capture`、`manual_steps=[]`，并跳过开新标签，直接引导 Agent 调 `capture`——避免「人已经登录了还让我扫码建应用」。仅 `waiting_login` 时才列出登录步骤。

`create_app` 有两道门：**幂等门**（已打开详情页或列表页已有应用时一律不新建，直接复用并走 `capture`）与**确认门**（`confirmed=false` 只返回 `need_confirmation`，不碰任何按钮，让 Agent 用选择框征求同意）。创建表单按可见文本匹配按钮、CSS 多候选定位输入框，不绑定易变的 class；任一步未命中返回 `selector_miss` 并附页面候选元素，**降级人工且不静默重试提交**。提交后必须验证出现新的 `/app/<appId>/...` 详情页才算成功。登录（扫码/验证码）、权限开通与版本发布仍需人工。

`watch` 靠 URL 信号判定阶段（登录页 `accounts.feishu.cn/.../login` → `open.feishu.cn/app` → 详情页 `/app/<appId>/...`），无需读 cookie 或碰登录动作。`capture` 不复用交互式向导 `Run()`（它全程 `fmt.Printf` 到 stdout 且有 5 处阻塞读终端），而是只执行凭据读取类浏览器动作，零 TTY、零 stdout 输出。Chrome 子进程的 stdio 全部丢弃、`Setsid` 脱离会话，Hub 退出不影响浏览器；CDP 请求显式禁用代理，避免宿主 `HTTP_PROXY` 把 `127.0.0.1` 转发成 502。

四平台凭据配置有交互式向导，含真连校验：

```bash
# 基础向导(手动输入凭据)
go run ./cmd/hub -config config.json -onboard feishu

# 浏览器执行器(自动填充表单,需先启动调试 Chrome)
./scripts/chrome-debug.sh
go run ./cmd/hub -config config.json -onboard feishu -onboard-browser

# 免交互体检(用现有凭据校验+热重载)
go run ./cmd/hub -config config.json -onboard feishu -onboard-verify-only
```

浏览器执行器会自动连接 Chrome 调试端口，按预定义步骤导航并提取凭据。首次使用需在打开的窗口里登录一次目标平台，之后登录态长期保留。

## 架构

```
AI 客户端(Trae / Claude Code / 自建 Agent)
         ↓  MCP(stdio / SSE / StreamableHTTP)
    ┌────▼────────────────────────────────┐
    │  本地 MCP Hub(Go 自研)              │
    │  ├─ 语义层:统一领域模型 → 平台参数    │
    │  ├─ 适配器:挂载/命名空间/热重载       │
    │  ├─ 事件桥:长连接 → 落库 → worker    │
    │  └─ 治理:幂等/审计/SQLite 状态       │
    └────┬────────────┬────────────┬──────┘
         │            │            │
    飞书 lark-mcp  钉钉官方MCP  企微 adapter  腾讯文档 MCP
    (stdio)      (stdio)      (stdio)     (HTTP)
```

## 项目结构

```
cmd/hub/            入口(main.go)
internal/
  adapter/          下游 MCP 连接管理(挂载/调用/热重载)
  semantic/         统一语义层(领域模型/映射/执行引擎)
  event/            事件桥(长连接接入/落库/分发/规则)
  report/           日报(定时/汇总/生成/推送)
  onboard/          接入向导(交互式配置/真连校验/浏览器执行器)
  health/           健康度(凭据/挂载/长连接/事件/调用计数)
  store/            SQLite 持久化(事件去重/审计/映射)
  config/           配置加载(.env 展开)
docs/               设计文档
adapters/           Node 侧适配器(企微)
scripts/            工具脚本(Chrome 调试启动器)
```

## 文档

- [统一语义层设计](docs/semantic-layer-design.md) — 领域模型、映射语法、能力矩阵、实测验证
- [跨平台协作中台设计](docs/cross-platform-mcp-hub-design.md) — 整体架构、各平台接入明细、风险登记
- [接入引导自动化设计](docs/onboarding-automation-design.md) — 向导流程、浏览器执行器、踩坑记录
- [P0/P1/P2 修复报告](docs/p0p1p2-fix-report.md) — 问题排查与修复证据
- [M3 场景 B 日报验收](docs/m3-scene-b-report.md) — 端到端实测记录

## 测试

```bash
go test ./...          # 全量测试(84 项)
go test ./internal/semantic/ -v  # 语义层详细输出
```

## 技术栈

- **语言**：Go 1.26
- **MCP SDK**：[mark3labs/mcp-go](https://github.com/mark3labs/mcp-go)
- **飞书 SDK**：[larksuite/oapi-sdk-go](https://github.com/larksuite/oapi-sdk-go)
- **钉钉 SDK**：[open-dingtalk/dingtalk-stream-sdk-go](https://github.com/open-dingtalk/dingtalk-stream-sdk-go)
- **浏览器自动化**：[go-rod/rod](https://github.com/go-rod/rod)
- **定时任务**：[robfig/cron](https://github.com/robfig/cron)
- **数据库**：SQLite（[modernc.org/sqlite](https://modernc.org/sqlite)）

## 环境要求

- Go 1.26+
- Node.js（企微适配器）
- Chrome（浏览器执行器，可选）

## License

内部项目，未开源。
