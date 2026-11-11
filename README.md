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

**Agent 驱动的接入**（stdio 宿主推荐）：以下工具串成全自动链路，**四个平台全部可用**；用户只需在浏览器里登录，并完成两次选择框确认（选平台、确认动作清单），**无需切换到终端执行任何命令**。第一步的选择框会**列出项目支持的全部平台**（不只是缺凭据的），每项带中文名、凭据状态、`setup_supported` / `browser_capture` / `dom_verified`、可用能力、`credential_fields` 与说明：

| 工具 | 作用 |
|---|---|
| `hub.onboard.status()` | 返回 `platforms` 全量清单（含 `credential_fields`、`dom_verified`）及兼容字段 `supported` / `missing` / `configured`；能力清单从语义层矩阵动态生成 |
| `hub.onboard.start(platform)` | 起带调试端口的 Chrome 并打开该平台开放平台/授权页；**带状态感知**，已登录/已在详情页时不再列扫码建应用 |
| `hub.onboard.watch(platform)` | 只读轮询 CDP 标签 URL，按各平台声明的登录域与详情页特征判定进度 |
| `hub.onboard.setup(platform, app_name?, confirmed?)` | **推荐入口**：按平台声明式跑「打开/创建应用 → 抓凭据 → 权限 → 发布」，各阶段独立；腾讯文档授权页即凭据页，无需创建/权限/发布 |
| `hub.onboard.set_credentials(platform, values)` | **零 DOM 兜底**：Agent 用表单收集用户复制的凭据 → 格式校验 → 真连校验 → 写 `.env` → 热重载，四平台通用 |
| `hub.onboard.inspect(platform)` | 只读 dump 当前页面按钮文本、输入框 placeholder 与页面文本；候选带命中的标签/role（如 `a,[role=button]`） |
| `hub.onboard.create_app(platform, app_name?, confirmed?)` | 单独代建应用：幂等检查 + 显式确认门（feishu / dingtalk / wecom 定义了创建动作） |
| `hub.onboard.capture(platform)` | 从凭据页读取凭据 → 校验 → 写 `.env` → 热重载挂载（四平台均有声明式动作） |

**三个层次，逐层降级**：

1. **L1 凭据兜底**（`set_credentials`）：不依赖任何 DOM，四平台通用；真连校验失败一律不写盘，回复里不回显凭据值。任何页面选择器失败都降级到这里，永不要求用户切终端。
2. **L2 浏览器抓取**：四平台的 `collect-creds` 都挂了声明式动作。飞书按实测 DOM 读 `.secret-code__code`；钉钉/企微/腾讯文档用 `readPage` + 正则从整页可见文本抽取（零 class 依赖），抽不到按 `next` 降级 L1。
3. **L3 全链路**（`setup`）：`platformStages(p)` 按平台提供权限/发布 Ops，`Portal.Flow` 区分应用控制台与 Token 授权页，`createAppAction` 按 `create-*` 前缀匹配。`platforms[].dom_verified` **如实标注**：只有飞书的 Ops 在真实后台校准过，其余三个按可见文本声明、未经真机实测，`selector_miss` 时降级 L1，不静默重试提交。

`start` 会先跑一次 `watch`：若已停在应用详情页，返回 `phase=ready_to_capture`、`manual_steps=[]`，并跳过开新标签，直接引导 Agent 调 `setup`——避免「人已经登录了还让我扫码建应用」。仅 `waiting_login` 时才列出登录步骤。四平台的登录域与详情页特征由 `Portal.LoginHosts` / `CredPages` / `AppIDRe` 声明，判定统一收敛在 `portalDetailMatch`，不再把飞书的 `/app/<appId>` 硬编码成通用规则。

`create_app` 有两道门：**幂等门**（已打开详情页或列表页已有应用时一律不新建，直接复用并走 `capture`）与**确认门**（`confirmed=false` 只返回 `need_confirmation`，不碰任何按钮，让 Agent 用选择框征求同意）。创建表单按可见文本匹配按钮、CSS 多候选定位输入框，不绑定易变的 class；任一步未命中返回 `selector_miss` 并附页面候选元素，**降级且不静默重试提交**。提交后必须验证出现新的应用/机器人详情页才算成功。腾讯文档没有应用概念，该工具返回 `unsupported` 并指向 `set_credentials`。登录（扫码/验证码）仍需人工；权限开通与版本发布可交由 `setup` 自动完成。

`setup` 是端到端入口：`confirmed=false` 先返回该平台的 `plan`（创建应用 / 开权限 / 发布版本等会在企业里真实产生记录；腾讯文档的 plan 只有「打开授权页 + 读 Token」），用户确认后带 `confirmed=true` 重调。应用控制台类平台会**自动打开或创建应用详情页**（从列表页读出应用链接直接打开，不需要用户点击）、抓凭据写 `.env` 并热重载，再跑权限与发布两个**相互独立**的阶段；Token 授权页类平台直接读 Token 完成接入。

**飞书权限 API 优先**（2026-11-09 实测踩坑后调整）：先用只读探针 `GET /open-apis/im/v1/chats?page_size=1` 判断能力是否可用——通了返回 `api_verified` 并**跳过整个 UI 开通流程**，既避开「点不到菜单就报失败」，也避免权限已开通时重复提交；无法确认（`unknown` / `no_credentials`）同样跳过 UI、标 `skipped` 且不做任何变更；只有平台明确返回权限不足（`needs_perms`）才进 UI 开通。`perm_probe` 如实区分 `checked`（仅群列表读权限）与 `unverified`（`im:message` 发消息、`bitable:app` 多维表格），**不把单个 API 调通等同于三个 scope 全部已开通**。钉钉/企微没有等价的权限校验 API，改为走页面只读探测（点菜单 + `readPage`）做幂等检测，由 `Done(text)` 判断是否已开通，不会被 `unsupported` 探针短路成永久跳过。

**各阶段独立**：单阶段 `selector_miss` 只记它自己的失败并继续跑下一阶段（旧版遇第一个失败就整体 `return`，导致 `publish` 根本没执行，却被误读成「平台侧原本已完成」）；`phase=partial` 会列出失败阶段，`phase=done` 时若存在 `skipped` 阶段也会在 `next` 里明确提示未验证项。发布阶段仍先跑只读 `probe`（等菜单渲染 + `readPage`）做幂等检测，页面已显示「当前修改均已发布」返回 `already_done` 跳过。UI 点击的选择器与 `inspect` 候选采集**共用同一份标签集合**（含 `button` / `[role=button]` / `[role=tab]`），并先 `waitVisible` 等菜单文本出现再点，匹配放宽为允许数字角标；`candidates` 现在带标签/role，现场即可定位是哪种元素。凭据阶段非 `done` 一律计入 failed，不会被后续阶段的成功「洗」成整体 `done`。四平台都定义了创建/权限/发布或等价动作，`status` 的 `dom_verified` 标明是否经过真机校准，失败一律降级 `set_credentials`。

浏览器执行器的所有操作均有界超时（单步 6s、会话 25s）：`waitVisible` 不再用无超时的 `MustElement` 无限重试；`NewBrowserExecutor` 复用已有标签而非无条件新建 `about:blank`，不会再留下空白页签。`capture` 还会跳过标为 `Scope: "list"` 的步骤（如等 `app-card`），因此在详情页调用时不会卡在「正在执行中」。

`watch` 靠 URL 信号判定阶段，无需读 cookie 或碰登录动作；腾讯文档这类「授权页即凭据页」的平台会直接判为 `in_app_detail=true`。`capture` 不复用交互式向导 `Run()`（它全程 `fmt.Printf` 到 stdout 且有 5 处阻塞读终端），而是只执行凭据读取类浏览器动作，零 TTY、零 stdout 输出。Chrome 子进程的 stdio 全部丢弃、`Setsid` 脱离会话，Hub 退出不影响浏览器；CDP 请求显式禁用代理，避免宿主 `HTTP_PROXY` 把 `127.0.0.1` 转发成 502。

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
go test ./...                 # 全量测试
go test ./... -list '^Test'   # 列出全部测试用例(数量随版本变化,不写死)
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
