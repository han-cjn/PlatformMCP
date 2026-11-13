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
- **企微上行链路自检**：适配器把 6 类消息帧 POST 到 `HUB_EVENT_URL`（未配置时回落 `http://127.0.0.1:9780/events`），Hub 的 `/events` 落库时顺带 `NoteChat` 沉淀 `chat_id`，`hub.chats` 才有企微会话。若适配器启动时 stderr 显示「事件上报: (已关闭…)」或 `events` 表长时间无新增，检查 `HUB_EVENT_URL` 是否被显式设成空串、以及凭据是否失效。**机器人删除/重建后，旧 chatid 会永久留在 `chats` 表（该表无 TTL、不记录 bot 维度），发送前必须确认目标群，不要直接复用 `hub.chats` 里的历史 ID。**
- **企微主动推送前置条件与限频**：见下方[企微主动推送：会话激活与 846607 排查](#企微主动推送会话激活与-846607-排查)。
- **机器人删除后由 Agent 代建**：`hub.onboard.setup(wecom, confirmed=false)` 在「已确认列表页渲染完成 + 列表为空」时返回 `need_create_app`，确认清单首条即「创建应用「mcp-hub」」；用户确认后 `confirmed=true` 由 `CreateApp` 代填提交表单并自动 `Capture` 写 `.env`。**创建不可撤销，确认门是硬约束**；`create-bot` Ops 已在 2026-11-12 第二十一轮按真机 DOM 端到端校准（独立创建页 / API 模式 / 必填简介 / 先生成 Secret / 保存），`dom_verified` 仍保持 `false`（权限开通阶段未校准，整体标记不扩张）。2026-11-12 真机修复了三个让代建永不触发的缺陷：① 后台首页「工作台管理」链接 `#/app/workbenchEditor/index` 被 `portalAppID` 的 `appIDFromURL` 宽匹配抽成 `workbenchEditor`，冒充已有机器人 → `create_app.go` 新增 `appLinkIsPlatformApp`，声明了 `AppIDRe` 的平台只认真实 ID 格式（飞书保留宽匹配）；② `listExistingApps` 的 `a[href]` 扫描缺管理区域隔离 → `app_list.go` 新增 `pageInAdminArea`；③ `CreateApp` 幂等门与 `openAppForSetup` 忽略 `listVerified` → 未确认渲染完成时返回 `page_loading`，不复用脏条目、也不误创建。修复后真机：`listExistingApps verified=true / count=0`，`Setup(false)` plan 首条出现「创建应用」。`setupPlan` 对企微的「创建版本 1.0.0 并申请发布」已删除（`stages.go` 的 wecom publish 阶段改为只读状态核对 + `not_required` 降级，不再点未校准的发布按钮）；**仍推荐走 `create_app(confirmed=true)`**，它只做「建机器人 + 抓凭据」。

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

**企微多机器人**（顶层 `wecom.bots`）：

```json
"wecom": {
  "bots": [
    { "name": "mcp-hub", "bot_id": "$WECOM_BOT_ID", "secret": "$WECOM_BOT_SECRET" },
    { "name": "测试", "bot_id": "$WECOM_TEST_BOT_ID", "secret": "$WECOM_TEST_BOT_SECRET" }
  ]
}
```

单个机器人时保持旧版 `WECOM_BOT_ID` / `WECOM_BOT_SECRET` 即可；配置多个机器人后，`im.send_text` 必须显式带 `bot_name`，**Hub 不会自动选择第一个**，未明确选择会返回候选清单，未知名称返回 `unknown_bot`。接入多个机器人用 `hub.onboard.capture` 的 `app_name` / `hub.onboard.setup` 的 `app_name` 明确指定；原始凭据只写 `.env` 的独立变量，`wecom.bots[]` 仅保存 `$ENV` 引用，不覆盖其他机器人。定时推送无法临时追问用户，须在 `report.bot_name` 显式配置发送身份；未配置时拒绝推送。

2026-11-13 真机复核：后台确认「测试 / mcp-hub」两个候选；未指定接入返回 `need_bot_selection`，未知或身份不符在读取凭据前拒绝。用户明确选择「测试」后完成抓取、真连校验与独立环境变量引用写入；同一适配器进程的两个真实客户端均收到 `authenticated`，未调用发送接口、未发送真实消息。`hub.health.mounted=true` 仅表示 MCP 初始化与工具枚举成功，不代表各 WebSocket 已认证；独立验证进程也不等于宿主热重载。当前会话连接器管理接口未注册 `mcp-hub`，需在连接器管理页禁用/启用或新开会话加载新二进制。

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
| `im.send_text` | `target`, `text`, `title?`, `format?`, `bot_name?` | 发送消息（plain/markdown）；企微多机器人必须明确 `bot_name` |
| `doc.create` | `title`, `markdown` | 创建文档 |
| `doc.append` | `doc_id`, `markdown`, `position?` | 追加内容（仅腾讯） |
| `doc.search` | `keyword` | 搜索文档 |
| `table.query` | `app`, `table`, `filters?`, `conjunction?`, `page_size?` | 查询记录 |
| `table.create` | `app`, `table`, `records`, `idempotency_key?` | 插入记录（幂等） |
| `table.update` | `app`, `table`, `records` | 更新记录 |

### 企微主动推送：会话激活与 846607 排查

长连接智能机器人支持不依赖当次回调的主动推送，但**有 `chatid` 不等于允许推送**。根据企微官方[主动推送消息](https://developer.work.weixin.qq.com/document/path/101837)：

1. 用户必须先在目标会话中给机器人发过消息，之后机器人才能向该会话主动推送。
2. 群聊 `chatid` 应从回调事件中获取；单聊目标为用户的 `userid`。
3. 同一会话的回复和主动推送合计最多 **30 条/分钟、1000 条/小时**。

推荐操作顺序：

1. 将用户明确选择的机器人加入目标群。
2. 用户在群里 @该机器人发一条消息，完成会话激活。
3. 确认适配器已向 Hub 上报事件；`hub.chats` 中出现该群记录，且事件载荷的 `bot_name` 是所选机器人。
4. 使用新事件中的 `chatid` 和明确的 `bot_name` 调用 `im.send_text`。多个机器人不得默认选择第一项，也不得失败后自动改选其他机器人重发。

调用示例（群 ID 仅为占位符，不要直接复用历史缓存）：

```json
{
  "platform": "wecom",
  "target": "wr_xxx_from_current_group_event",
  "text": "今晚20点开会",
  "format": "markdown",
  "bot_name": "mcp-hub"
}
```

`846607 aibot send msg frequency limit exceeded` 的字面含义是频率超限，但也可能出现在目标会话未与机器人建立交互时。排查顺序：

| 检查项 | 处理方式 |
|---|---|
| 目标群是否与所选机器人互动过 | 先 @该机器人发消息，不要仅更换旧群 ID 或换机器人重试 |
| `bot_name` 是否明确且与群事件一致 | 多机器人必须由用户选择；未知名称直接拒绝 |
| 是否命中同会话限频 | 合并通知、减少自动重试；遵守 30 条/分钟、1000 条/小时 |
| 真实平台错误是什么 | 保留返回的 `errcode`、`errmsg`、`req_id`，不要按 `[object Object]` 猜测 |

适配器已处理 SDK 以普通错误帧 reject 的情况，发送失败会返回真实 `errcode/errmsg/req_id`，不再被 `String(err)` 格式化为 `[object Object]`。**遇到失败不会自动切换机器人或向多个群重发。**

2026-11-13 真机验证：使用旧群缓存调用 `mcp-hub` 曾返回 `846607`；用户在目标群与该机器人完成互动、Hub 收到两条真实群事件后，以同一个 `chatid` 和 `bot_name=mcp-hub` 再次调用，成功发送“今晚20点开会”。这验证了会话激活前置条件；不代表旧缓存可长期作为群身份依据。

## 接入向导

**自动引导**：启动时检测到某平台缺凭据，终端直跑（有 TTY）会自动逐个进入交互向导，完成即热重载、全部接入后继续启动。WorkBuddy 等 stdio 宿主（stdin 是管道）跳过交互，只在 stderr 打一条指引日志。可用 `HUB_NO_AUTO_ONBOARD=1` 关闭（CI/测试环境）。

**Agent 驱动的接入**（stdio 宿主推荐）：以下工具串成全自动链路，**四个平台全部可用**；用户只需在浏览器里登录，并完成两次选择框确认（选平台、确认动作清单），**无需切换到终端执行任何命令**。第一步的选择框会**列出项目支持的全部平台**（不只是缺凭据的），每项带中文名、凭据状态、`setup_supported` / `browser_capture` / `dom_verified`、可用能力、`credential_fields` 与说明：

| 工具 | 作用 |
|---|---|
| `hub.onboard.status()` | 返回 `platforms` 全量清单（含 `credential_fields`、`dom_verified`、`app_list_verified` / `app_list_url`、`tabs_verified`）及兼容字段 `supported` / `missing` / `configured`；能力清单从语义层矩阵动态生成 |
| `hub.onboard.start(platform)` | 起带调试端口的 Chrome 并打开该平台页面；**带状态感知**，已登录且已停在详情页时不再列扫码建应用，已登录但未进详情时**自动打开/激活应用列表页**（无需用户点「应用开发」） |
| `hub.onboard.watch(platform)` | 只读轮询 CDP 标签 URL，按各平台声明的登录域与详情页特征判定进度 |
| `hub.onboard.setup(platform, app_name?, confirmed?)` | **推荐入口**：按平台声明式跑「打开/创建应用 → 抓凭据 → 权限 → 发布」，各阶段独立；腾讯文档授权页即凭据页，无需创建/权限/发布 |
| `hub.onboard.set_credentials(platform, values)` | **零 DOM 兜底**：Agent 用表单收集用户复制的凭据 → 格式校验 → 真连校验 → 写 `.env` → 热重载，四平台通用 |
| `hub.onboard.inspect(platform)` | 只读 dump 当前页面按钮文本、输入框 placeholder 与页面文本；候选带命中的标签/role（如 `a,[role=button]`） |
| `hub.onboard.create_app(platform, app_name?, confirmed?)` | 单独代建应用：幂等检查 + 显式确认门（feishu / dingtalk / wecom 定义了创建动作） |
| `hub.onboard.capture(platform)` | 从凭据页读取凭据 → 校验 → 写 `.env` → 热重载挂载（四平台均有声明式动作） |

**三个层次，逐层降级**：

1. **L1 凭据兜底**（`set_credentials`）：不依赖任何 DOM，四平台通用；真连校验失败一律不写盘，回复里不回显凭据值。任何页面选择器失败都降级到这里，永不要求用户切终端。
2. **L2 浏览器抓取**：四平台的 `collect-creds` 都挂了声明式动作。飞书按实测 DOM 读 `.secret-code__code`；钉钉的详情页是单页多 tab（`CredTab` / `CredTabFragment` 声明），会**先点「凭证与基础信息」切到 `/baseinfo`** 再用 `readPage` + 正则抽取（零 class 依赖），企微/腾讯文档直接读整页文本。钉钉 `/baseinfo`：`Client ID` 明文可读；`Client Secret` **页面渲染**为 `当前使用:xxxx****xxxx` 掩码，但同一行右侧复制图标写入剪贴板的是**完整明文**（2026-11-12 用户手工点复制确认）。因此新增 `copyToCred` Op：注入 hook 拦截 `clipboard.writeText` / `execCommand('copy')` 捕获明文（**不读系统剪贴板**），捕获不到才降级 `navigator.clipboard.readText()`；按「文本最短的凭证块」定位，避免误点 `Client ID` 的复制按钮。读回值仍交格式校验，**绝不把掩码写进 `.env`**；复制按钮定位失效才按 `next` 降级 L1。主路径与降级路径已在本地构造页验证，2026-11-12 第十四轮又在真实 `/baseinfo` 页端到端跑通（见下方第十四轮）。
3. **L3 全链路**（`setup`）：`platformStages(p)` 按平台提供权限/发布 Ops，`Portal.Flow` 区分应用控制台与 Token 授权页，`createAppAction` 按 `create-*` 前缀匹配。`platforms[].dom_verified` **如实标注**：飞书与钉钉的 Ops 已在真实后台完整校准（钉钉自 2026-11-12 第十二轮起为 true）。`app_list_verified`（列表页 URL / 行选择器）与 `tabs_verified`（详情页 `/baseinfo` / `/permission` / `/version` 三个 tab 的导航与幂等文本）是两档独立标记，钉钉均为 true。钉钉的**权限、版本表单与「保存→生成草稿→发布按钮」全链路已真机验证**（见下方「钉钉表单自动提交」）；仍未覆盖的是创建应用表单（`/app/create`，属 `create_app.go` 独立链路，本次后台已有应用未走该分支），不掺进 `dom_verified`。

`start` 会先跑一次 `watch`：若已停在应用详情页，返回 `phase=ready_to_capture`、`manual_steps=[]`，并跳过开新标签，直接引导 Agent 调 `setup`——避免「人已经登录了还让我扫码建应用」。仅 `waiting_login` 时才列出登录步骤。四平台的登录域与详情页特征由 `Portal.LoginHosts` / `CredPages` / `AppIDRe` 声明，判定统一收敛在 `portalDetailMatch`，不再把飞书的 `/app/<appId>` 硬编码成通用规则。

`create_app` 有两道门：**幂等门**（已打开详情页或列表页已有应用时一律不新建，直接复用并走 `capture`）与**确认门**（`confirmed=false` 只返回 `need_confirmation`，不碰任何按钮，让 Agent 用选择框征求同意）。创建表单按可见文本匹配按钮、CSS 多候选定位输入框，不绑定易变的 class；任一步未命中返回 `selector_miss` 并附页面候选元素，**降级且不静默重试提交**。提交后必须验证出现新的应用/机器人详情页才算成功。腾讯文档没有应用概念，该工具返回 `unsupported` 并指向 `set_credentials`。登录（扫码/验证码）仍需人工；权限开通与版本发布可交由 `setup` 自动完成。

`setup` 是端到端入口：`confirmed=false` 先返回该平台的 `plan`（创建应用 / 开权限 / 发布版本等会在企业里真实产生记录；腾讯文档的 plan 只有「打开授权页 + 读 Token」），用户确认后带 `confirmed=true` 重调。应用控制台类平台会**自动进入应用列表页、识别已有应用并点进其详情页**（不需要用户点击；列表是行元素而非链接时按 Portal 声明的行选择器点击）、抓凭据写 `.env` 并热重载，再跑权限与发布两个**相互独立**的阶段；Token 授权页类平台直接读 Token 完成接入。若列表页还在加载、无法确认页面特征，返回 `phase=page_loading` 而不是给出含「创建应用」的确认清单——创建不可撤销，必须先确认真的没有可复用应用。

**飞书权限 API 优先**（2026-11-09 实测踩坑后调整）：先用只读探针 `GET /open-apis/im/v1/chats?page_size=1` 判断能力是否可用——通了返回 `api_verified` 并**跳过整个 UI 开通流程**，既避开「点不到菜单就报失败」，也避免权限已开通时重复提交；无法确认（`unknown` / `no_credentials`）同样跳过 UI、标 `skipped` 且不做任何变更；只有平台明确返回权限不足（`needs_perms`）才进 UI 开通。`perm_probe` 如实区分 `checked`（仅群列表读权限）与 `unverified`（`im:message` 发消息、`bitable:app` 多维表格），**不把单个 API 调通等同于三个 scope 全部已开通**。钉钉/企微没有等价的权限校验 API，改为走页面只读探测（点菜单 + `readPage`）做幂等检测，由 `Done(text)` 判断是否已开通，不会被 `unsupported` 探针短路成永久跳过。

**各阶段独立**：单阶段 `selector_miss` 只记它自己的失败并继续跑下一阶段（旧版遇第一个失败就整体 `return`，导致 `publish` 根本没执行，却被误读成「平台侧原本已完成」）；`phase=partial` 会列出失败阶段，`phase=done` 时若存在 `skipped` / `not_required` 阶段也会在 `next` 里明确提示未验证项。钉钉的权限阶段按声明的 `Perms` 逐个检查「code + 无需审批 + 已开通」（不要求 23 项全开），版本阶段仍用「版本状态 + 已上线」；未达成时按真机表单执行 apply（见下段）。`perm_probe.unverified` 按平台返回各自的 `Perms`，不再把飞书的 `im:message` / `im:chat` / `bitable:app` 串到钉钉。发布阶段仍先跑只读 `probe`（等菜单渲染 + `readPage`）做幂等检测，页面已显示「当前修改均已发布」返回 `already_done` 跳过。UI 点击的选择器与 `inspect` 候选采集**共用同一份标签集合**（含 `button` / `[role=button]` / `[role=tab]`），并先 `waitVisible` 等菜单文本出现再点，匹配放宽为允许数字角标；`candidates` 现在带标签/role，现场即可定位是哪种元素。凭据阶段非 `done` 一律计入 failed，不会被后续阶段的成功「洗」成整体 `done`。四平台都定义了创建/权限/发布或等价动作，`status` 的 `dom_verified` 标明是否经过真机校准，失败一律降级 `set_credentials`。

浏览器执行器的所有操作均有界超时（单步 6s、会话 25s）：`waitVisible` 不再用无超时的 `MustElement` 无限重试；`NewBrowserExecutor` 复用已有标签而非无条件新建 `about:blank`，不会再留下空白页签。`capture` 还会跳过标为 `Scope: "list"` 的步骤（如等 `app-card`），因此在详情页调用时不会卡在「正在执行中」。

`watch` 靠 URL 信号判定阶段，无需读 cookie 或碰登录动作；腾讯文档这类「授权页即凭据页」的平台会直接判为 `in_app_detail=true`。`capture` 不复用交互式向导 `Run()`（它全程 `fmt.Printf` 到 stdout 且有 5 处阻塞读终端），而是只执行凭据读取类浏览器动作，零 TTY、零 stdout 输出。Chrome 子进程的 stdio 全部丢弃、`Setsid` 脱离会话，Hub 退出不影响浏览器；CDP 请求显式禁用代理，避免宿主 `HTTP_PROXY` 把 `127.0.0.1` 转发成 502。

**跨平台标签隔离**（2026-11-11 实测）：调试 profile 会长期保留上一次接入的标签。飞书详情页 `open.feishu.cn/app/cli_xxx/baseinfo` 同时含 `/app/` 与 `/baseinfo`，曾命中钉钉的 `CredPages`，导致接入钉钉时 `watch` 误判已进详情页、`start` 提前返回 `ready_to_capture` 且不再打开钉钉页面、`setup` 的 `app_url` 指向飞书。修复后所有 URL 判定先经 `portalPageBelongs` 做域白名单校验（各平台主域 `feishu.cn` / `dingtalk.com` / `weixin.qq.com` / `docs.qq.com`，并合并 `LoginHosts` / `CredPages`），`start` 的提前返回还额外要求 `detail_url` 属于本平台并回传真实停留页面；`openPlatformEntryTab` 对会 302 到独立登录域的入口页按「同平台域内已有标签」幂等并激活到前台，避免重试 `start` 堆积重复标签。

**浏览器执行器同样按平台绑定标签**（2026-11-11 第二轮实测）：`NewBrowserExecutor` 原先取 `MustPages()` 里第一个真实标签，多标签时创建表单 / `inspect` / `readPage` 会跑在上一次接入的飞书页面上，表现为 `selector_miss` 且 candidates 全是飞书元素。现在 `NewBrowserExecutorForPlatform` + `pickPlatformPage` 按「本平台详情页 > 本平台应用列表页 > 本平台非登录页 > 登录页」选择并激活，找不到该平台标签就直接降级而不是在别的平台页面上执行动作；`inspect` 也不再忽略定位失败去 dump 飞书页面。

**应用列表页自动导航**（2026-11-11 第三轮真机实测）：开放平台入口页通常不渲染应用列表（钉钉 `?hash=%23%2F#/` 只有 3 个文档链接），停在入口页会把「读不到应用链接」误判成「企业里没有应用」。钉钉三段 URL 实测为：入口页 `?hash=%23%2F#/` → 列表页 `/fe/app?hash=...#/corp/app`（可直接导航，登录态在同一持久化 profile 内保留）→ 详情页 `/fe/ai?...#/app/<uuid>/dev`。`Portal.AppListURL` / `AppListMarkers` / `AppListRowSelector` 声明这三层，`ensureAppListTab` 用 CDP 幂等打开/激活列表页（短轮询 + 记住刚创建的标签 ID，避免连续 `start` 堆重复标签），`listExistingApps` 读 `tr[data-row-key] h5[title]` 行作为「已有应用」证据并返回 `NeedsClick`，`clickAppRow` 纯导航点击进入详情。`listVerified=false` 时 `setup` 返回 `page_loading` 而非创建清单，杜绝误建。详情页 URL 判定改用实测的 `#/app/<uuid>` + `/fe/ai`，并把 `/fe/app` 加入排除段，列表页不再被当成详情页。

**详情 tab 自动切换**（2026-11-11 第四轮真机实测）：钉钉点进应用默认停在 `/permission`，而旧版 `capture` 与 setup 阶段共用「当前页面」、各自假设已在自己的 tab，于是出现「抓凭据 `capture_failed`（AppKey 正则未匹配）、开权限第 3 步找不到确定/提交、发布找不到创建版本」三个连续失败。实测三段 hash 为 `/baseinfo`（`应用凭证` / `Client ID` 明文 / `Client Secret` 页面渲染成掩码，但复制图标给明文，见下方第十三轮）、`/permission`（`权限点code` / 英文 scope / `未开通` / `立即开通` / `批量申请`）、`/version`（`创建新版本` / `版本状态` / `1.0.0 已上线`）。现在 `collect-creds` 与两个 stage 的 probe 都先点自己的菜单并 `waitVisible(body)` 等 SPA 渲染，再 `readPage` 做幂等检测；`Done(text)` 使用真实状态文本，不再用飞书式权限名或「已发布」猜测。已上线应用会直接 `already_done`，不产生任何企业记录。

**钉钉表单自动提交**（2026-11-11 第五轮真机只读校准）：调试 Chrome 里只读点开「创建新版本」后确认它是同标签路由跳到 `/version/app_publish/new`（不是弹窗），字段为 `input#version`（默认 1.0.1）、`textarea#description`、应用可用范围 radio，按钮只有「保存」；只读核查版本表格仍只有 `1.0.0 已上线` 一行，**点开按钮本身不产生记录**。`/permission` 页每行是 `tr[data-row-key]`，文本含英文 code 与 `span「立即开通」`，「批量申请」按钮带 `disabled`（未勾选时无反应）。据此新增 `clickInRow` Op：按行文本正则定位 `tr`，再点该行内文本为 `Text` 的子元素，避免误开别的权限；`Portal.ApplyPerms` 只列真机未开通的 code（`snsapi_base` 默认已开通、行内无按钮，只进 `Perms`）。`confirmed=true` 后：权限阶段用搜索框按 code 过滤 → 等行 → 点该行「立即开通」；版本阶段点「创建新版本」→ 等 `input#version` → 填 `textarea#description` → 点「保存」。`PlanLine` 把「会在企业内产生开通/版本记录」写进确认清单。

**自动化的边界**（2026-11-12 更新）：版本阶段由 `ApplyWarning` 返回 `status=applied_unverified`，消息明确「只在企业内生成『开发中』版本草稿，**不自动点发布**——发布是不可逆动作，请人工在后台核对后点发布」。发布按钮的 DOM 已真机采集（`/version/app_publish/<id>` 详情页里的 `BUTTON.dtd-btn.dtd-btn-primary`），但刻意**不放进 Ops**。「保存」后新增一条 `waitVisible(body, 开发中|待发布|未发布)` 落库校验：**保存没真落库就 `selector_miss`**，不再像 11-12 之前那样静默无效却仍返回「已保存未发布」。创建应用表单仍未打开过（后台已有应用），属 `create_app.go` 独立链路。「立即开通」的二次确认弹窗已由 20:20 只读复核**证伪**，不再是待采集项。

**第十三轮：钉钉 Client Secret 也能自动拿**（2026-11-12）：此前注释、`captureNext` 提示、`README` 与专家包都写着「`Client Secret` 只显示掩码、平台不提供明文、必须人工复制一次」。这是**未经实测的推断**——依据只是 11-11 一次只读 DOM dump（HTML 脱敏 + 2600 字截断，只登记到 1 个 `aria-label=复制`），而且**从没有人点过复制按钮、没看过剪贴板内容**。用户截图指出掩码右侧同样有复制图标，并手工点复制确认剪贴板里是**完整明文**，旧结论被直接证伪。

- `browser.go` 新增 `copyToCred` Op：点击前注入 hook 捕获 `navigator.clipboard.writeText` / `document.execCommand('copy')` 的写入值（**不读系统剪贴板、不碰用户其它剪贴内容、不回显 Secret**）；hook 未捕获时才经 CDP `Browser.grantPermissions` 授权并降级 `navigator.clipboard.readText()`。定位用「`Target` 容器 + `Match` 文本正则 + 文本最短优先」，避免 `Client ID` / `Client Secret` 同处一个 `<tr>` 时误点前者。读到的值只进内存 `Collected`，格式合规与否统一由 `commitCredentials` 判定，掩码照常走 `invalid_format` 降级，**绝不伪装成成功**。
- `portal.go` 钉钉 `collect-creds` 第 6 步由「`extract` 抓掩码串」改为 `copyToCred`（`Script` 升级 `dingtalk@2026-11-12-creds`）；`capture.go` 的 `next`、`platforms.go` 的 hint 全部改为基于实测的诚实表述。
- **本地构造页真机验证**（模拟钉钉两列 `<td>` + 各带 `aria-label=复制`）：主路径 hook 捕获读到 33 位明文、`HAS_MASK=false`、`IS_ID_COPY=false`（确认没误点 Client ID）；降级路径 `readText` 读到 40 位明文。两路径均通过，单测 161+ 项全绿。
- **诚实边界（第十三轮当时，已闭环）**：`copyToCred` 的定位选择器最初基于历史 DOM dump 与截图，当时钉钉真机端到端尚未跑过；`dom_verified` 不因那次改动扩张，失败按 `selector_miss` 降级 `set_credentials`。**该边界已在第十四轮闭环，见下。**

**第十四轮：`copyToCred` 钉钉真机端到端验证通过**（2026-11-12）：用户指定 `/fe/ai#/app/<appId>/baseinfo` 页面，要求真机验证并「拿取」凭据。

- **环境**：上一轮按用户要求清掉了调试 Chrome 与 `~/.chrome-debug-profile`，本轮用 `--no-sandbox --disable-gpu --disable-software-rasterizer` 重建 9222 实例（默认参数仍 exit 133 / `GPU process isn't usable`），用户扫码登录后停在 `/baseinfo`。
- **先排掉一个假失败**：宿主拉起的 hub 进程仍是 `bin/hub` 重建前的二进制——`hub.onboard.capture(dingtalk)` 返回 `incomplete` + 旧断言「平台不提供明文,纯 DOM 无法抓取」，而这句已由 `a67295c` 删除、当前源码零残留。stdio 宿主不会自动重载二进制，因此该结果**不能作为真机判定**；需在连接器页面禁用/启用 `mcp-hub`（或新开会话）后才生效。
- **方法**：用最新源码现场编译同包只读探针（跑完即删、不进版本库）直连同一调试 Chrome，执行 `captureOps(credAction)`；只报告键名、长度、是否掩码、是否误点与格式校验结果，不回显任何值。
- **证据**：`dingtalk@2026-11-12-creds` 的 6 个 Ops 全绿；`Client ID` 20 位 `ding` 前缀、`Client Secret` **64 位非掩码**，两者格式均 `pass`；`Secret ≠ Client ID`（未误点 Client ID 复制按钮）；`{"hook":true,"captured":2}` 证明走 hook 主路径、**未降级读系统剪贴板**；复跑幂等仍 PASS。
- **收尾**：完整 `Capture` 返回 `phase=done`、两键均 collected，钉钉 token 接口真连校验通过，`.env` 写入仓库根（被 `.gitignore` 忽略）。**诚实边界：探针的 `reload` 是 no-op，宿主并未真正热重载**，`.env` 要等连接器重拉才生效，不把返回值里「已完成热重载」当真。
- **回归**：`gofmt` 空 / `go build ./...` / `go vet ./...` / 全量 6 包测试绿；工作区干净。
- **仍成立的边界**：`dom_verified` 维持 `true` 不扩张；创建应用表单（`/app/create`）未走过；**发布仍不自动点**（Hub 只建草稿）；未创建应用、未开权限、未点发布、未重试提交、未回显任何凭据值。

**第十五轮：修复钉钉发消息的 `msgParam` 双重序列化**（2026-11-12）：用户授权发「明天放假」通知时，`im.send_text(dingtalk)` 返回 `400 invalidParameter.msgParam.invalid / msgParam必须是json格式`。

- **根因**：`internal/semantic/mapping.go` 的 `Encode.Format="json"` 先把 `{title,text}` 序列化成 JSON 字符串；官方 `dingtalk-mcp@1.1.21` 的 `dist/DingTalkMCPServer.js` 对 `extendType:'json'` 参数又执行一次 `JSON.stringify` → 二次序列化成带转义的字符串字面量。`docs/semantic-layer-design.md:410` 历史用无效 target 得到 `invalid.openConversationId`（消息体被接受），说明是 `@latest` 上游行为变更导致的回归。
- **修复**：`capabilities.go` 钉钉 `Encode.Format` 改为 `plain`（原样传 object，由官方 MCP stringify 一次）；`semantic_test.go` 两处断言改断言 `map[string]any`，并新增回归测试模拟官方 MCP 的一次序列化。飞书 `data.content` 仍用 `json`，不受影响。
- **旁证**：直连钉钉官方 API、把 `msgParam` 作为 JSON 字符串只传一次即 `HTTP 200 + processQueryKey`，通知已投递到 `cidsMGUlJ…` 群；同时确认 `ROBOT_CODE=AppKey` 假设成立。`hub.tools` 的 `ExposedName` 只是清单报告，`main.go:330` 只注册语义工具，客户端调不到原生工具，故当时用直连 API 绕过、未改代码。
- **验证**：`gofmt` 空 / `go build ./...` / `go vet ./...` / 全量 6 包测试绿；新增 `TestDingTalkMsgParamNoDoubleEncoding` 锁定「Hub 传 object」契约，并用反面对照复现二次序列化。真机探针直连官方 `dingtalk-mcp`、传 object + **必然无效的会话 ID**，返回 `invalid.openConversationId`（消息体被接受、只是找不到会话），与 `docs/semantic-layer-design.md:410` 的历史口径一致，**未打扰任何真实群**。
- **诚实边界**：经宿主 `im.send_text` 的**完整链路 + 真实群投递仍未跑**——stdio 宿主不会自动重载二进制，需禁用/启用 `mcp-hub` 才生效；且完整复跑会再往群里发一条消息（不可逆），未经用户授权不擅自发。当前证据覆盖「Hub 参数构造正确」与「官方 MCP 接受 object」，**不宣称运行时端到端已验证**。

**第十六轮：`collect-creds` 自动抓 `ROBOT_CODE`，发群消息闭环**（2026-11-12）：用户给出「下午 4 点开会」的失败任务分享页——那次 `capture` 只写入 `DINGTALK_Client_ID` / `DINGTALK_Client_Secret`，`im.send_text` 随即被 `System parameter ROBOT_CODE is required` 拦下，而 `set_credentials` 因 `PortalCreds` 只声明两项也补不进，消息最终没发出。手工往 `.env` 写一次不可持续（会被清空/重跑覆盖），根上的修法是把它纳入自动抓取。

- **真机 DOM 事实**：钉钉「机器人」tab（`/bot`）的内容**不在外层 SPA**，而在同源 iframe `open-dev.dingtalk.com/fe/app?isHideOuterFrame=true#/pure/appMgr/inner/h5/...` 里，外层 `body` 只有左侧菜单。「复制 RobotCode」是 `<p class="robotStatus___…"><span role="img">…</span>复制 RobotCode</p>`——不是 `button`、没有 `aria-label`，已验证的 `copyToCred` 定位器抓不到。真机还发现：对该控件用 JS `el.click()` 会让 iframe 里的 handler 卡住 CDP 会话（25s 超时），**必须用视口绝对坐标的真实鼠标点击**。
- **实现**：新增第 13 种 Op `copyInFrame`（hook 装到主 window + 所有同源 iframe 的 window；跨 frame 按「文本最短容器 + `Match` + `btnSel`」定位，支持容器自身命中；iframe 偏移换算坐标后用 CDP 真实鼠标点击；读值仍走 hook 主路径，降级才读系统剪贴板）。`collect-creds` 由 6 步扩到 9 步：读完 Secret 后点「机器人」菜单、`copyInFrame` 抓 `ROBOT_CODE`。`PortalCreds["dingtalk"]` 增第三项 → `commitCredentials` 写 `.env`、`set_credentials.required` 与 `status.credential_fields` 同步变三项，`.env.example` 补说明（`config.json` 白名单早前已含 `ROBOT_CODE`）。**刻意不改动已真机验证的 `copyToCred`**。
- **真机验证**：用最新源码独立进程跑完整 `Capture(dingtalk)`，两次均 `phase=done`、`collected=[DINGTALK_Client_ID, DINGTALK_Client_Secret, ROBOT_CODE]`、真连校验通过、`.env` 落库三键（64 / 20 / 20 位、均无掩码）、复跑幂等（4.36s / 3.97s）。`/bot` 复制按钮读回的 RobotCode 是 20 位 `ding` 前缀明文，**等于 Client ID（AppKey）**，验证了第十五轮的假设，但坚持从页面复制按钮取值而非靠假设。
- **发消息闭环**：用真机抓到的 `ROBOT_CODE` + 第十五轮修复后的 **object 形态 `msgParam`**，经官方 `dingtalk-mcp` stdio 等价链路单次调用 `sendMessageToGroupByRobot`，把「各位同事，下午 4 点开会，请准时参加。」投递到用户已确认的 `cidsMGUlJ…` 群，返回 `processQueryKey`。这次同时验真了「真 RobotCode 有效」与「object msgParam 可接受」。
- **诚实边界**：`dom_verified` 维持 `true` 不扩张；独立探针的 `reload` 是 no-op，`Capture` 返回的「已完成热重载」在探针里不成立；**经宿主 `im.send_text` 的完整链路仍需禁用/启用 `mcp-hub` 连接器**才会加载新二进制与新 `.env`。`push_targets.dingtalk` 仍为空（`DINGTALK_OPEN_CONVERSATION_ID` 未写 `.env`），本轮发送显式指定了群 ID。未创建应用、未开权限、未点发布、未重试提交、未回显任何凭据值。

**第十七轮：企微智能机器人 `collect-creds` 真机端到端跑通**（2026-11-12）：用户要求把企业微信也接入自动化，「优先 B（已有机器人只抓取），没有再走 A（创建）」。

- **路径判定**：用户扫码登录企微管理后台后，机器人管理页 `/wework_admin/frame#/aiHelper/list?from=manage_tools&tab=manage` 已有名为「个人测试」的机器人（企业创建、API 模式）→ 走 B，不创建。
- **真机 DOM 实测**：点「个人测试」（`<span class="d2c_bot_name">`）跳详情页 `#/aiHelper/detail?aibotid=<数字>`；Bot ID / Secret 是纯 DOM 明文，每行右侧是 `<svg class="wd-icon-svg-copy_16">` 复制图标（非 button、无 aria-label、**不在 iframe**——与钉钉 RobotCode 的同源 iframe 场景不同，故用 `copyToCred` 而非 `copyInFrame`）。
- **实现**：企微 `collect-creds` 从「`readPage` + 两个 `extract`（正则 `[^\n]` 跨不了「Bot ID\n值」换行，必然抓不到）」改为「列表步骤（标 `Scope:"list"`，capture 时剔除）+ 详情页 `waitVisible Bot ID` + 两个 `copyToCred`（btnSel 指 `svg.wd-icon-svg-copy_16`）」。配套修正：`CredPages` 精确到 `aibotid=`（去掉把列表页误判成详情的 `/aiHelper/`）；`LoginHosts` 去掉主域 `work.weixin.qq.com`（否则详情页被 `isLoginPage` 误判成登录页）；`isAppListPage` 排除命中详情的 URL（企微列表/详情共用 `/wework_admin/frame` 前缀）；补 `AppListURL`/`AppListMarkers`/`AppListRowSelector`。脚本版本 `wecom@2026-11-creds` → `wecom@2026-11-12-creds`。
- **真机验证**：完整 `Capture(wecom)` 两次均 `phase=done`、`collected=[WECOM_BOT_ID, WECOM_BOT_SECRET]`、长连接真连校验通过（`长连接认证成功`）、`.env` 落库（35 / 43 位、均无掩码）、复跑幂等。
- **顺带修一个真实缺陷**：`ValidateWeCom` 用相对路径 `adapters/wecom-aibot-mcp/server.js` 起适配器子进程，工作目录依赖调用方——测试从 `internal/onboard` 跑时相对路径解析失败、真连验证误报「找不到模块」。修法：`cmd.Dir = moduleRoot()`（回溯到 `go.mod` 所在目录），与调用方工作目录解耦。
- **gate 口径**：企微 `app_list_verified` 据实测标 `true`（列表页可直达、行可点进详情）；`dom_verified` **保持 `false`**——`create-bot`（创建）与权限开通未真机校准，只验证了抓取链路；`tabs_verified` 保持 `false`（企微详情页是 aibotid 参数直达，非 tab 切换，该 gate 不适用）。未创建机器人、未改权限、未点发布、未回显任何凭据值。

**第十八轮：修 start 连开官网标签 + 后台页面样式错乱**（2026-11-12）：用户三张截图报障——浏览器堆了 5 个「企业微信」官网标签、`/wework_admin/frame` 左侧主导航被裁且底部出现横向滚动条、执行过程反复停在列表页抓不到凭据。

| # | 根因 | 证据 |
|---|---|---|
| ① | `Portals["wecom"].URL` 是官网首页（跳 `#/app/workbenchEditor/index` 营销页），不是管理后台；`start` 每次重试都拿它当落点 | `portal.go:311`、`launch.go` 旧兜底分支 |
| ② | 两处启动参数缺第十四轮实测必需的 `--no-sandbox --disable-gpu --disable-software-rasterizer`，本机默认参数 `exit 133 / GPU process isn't usable` ⇒ 调试 Chrome 起不来 ⇒ 每轮重试都走 `openURLWithSystem` 兜底，在**日常浏览器**里开不受 CDP 管辖的标签；旧实现无节流 | `README.md` 第十四轮、`launch.go` / `chrome-debug.sh` 旧参数 |
| ③ | `openPlatformEntryTab` 只按「域内已有标签」幂等；新标签注册窗口期内 URL 还是 `about:blank`，命中不了（`ensureAppListTab` 早有 pending 机制，入口页漏了） | `launch.go` / `app_list.go` |
| ④ | 启动参数无 `--window-size`；固定宽度的管理后台在默认窄窗口下横向溢出，左侧主导航被挤出视口 ⇒ 样式错乱 | 截图 2（侧栏只剩半个词 + 底部横向滚动条） |
| ⑤ | 执行过程卡住是**工具选错**：`capture` 按设计剔除 `Scope:"list"` 步骤、要求已停在 `#/aiHelper/detail?aibotid=`，只有 `setup` 会跑「navigate 列表页 → 点 `.d2c_bot_name` → 等 URL」进详情；`dom_verified=false` 又让 Agent 只做只读定位、不敢点，于是反复重试 | 截图 3 执行日志、`portal.go:336-350` |

- **修复（静态分析 + 单测）**：`launch.go` 新增 `platformEntryURL`（有 `AppListURL` 的平台 start 直接进管理后台，不动 `Portal.URL` 数据面）、`chromeLaunchArgs`（统一启动参数，补 3 个 GPU flag + `--window-size=1440,900`）、`entryPendingTab`（pending ID 幂等 + 优先激活列表/详情标签而非官网营销页）、`openURLWithSystem` 同 URL 5 分钟节流；`scripts/chrome-debug.sh` 同步参数并支持 `CHROME_EXTRA_FLAGS`。新增 5 个回归用例。

**第十八轮真机验证**（2026-11-12 18:35–18:52，调试 Chrome 从零拉起；全程不代做登录、不创建、不提交、不回显凭据值）：

| 验证项 | 真机结果 |
|---|---|
| 新参数能否自启动 | `StartPlatform(wecom)` → `chrome=started`，CDP `/json/version` 返回 `Chrome/158.0.8127.18`。本机默认参数原本 `exit 133` ⇒ **GPU flag 修复生效** |
| 窗口尺寸 / 样式 | CDP `Browser.getWindowForTarget` → `{left:0,top:33,width:1440,height:900,windowState:"normal"}`；页面 `innerWidth=1280`、`scrollWidth=1280` ⇒ **无横向溢出**（图2 的裁切不再出现） |
| start 落点 | 首次 `url=https://work.weixin.qq.com/wework_admin/frame#/aiHelper/list?...`；官网营销页标签数 **0** |
| 标签幂等 | 连续 3 次 `start`，page 标签 `2 → 2 → 2`；人为塞一个官网营销页标签后再 `start`，仍 `3 → 3` |
| 后台页面真渲染 | 详情页只读探测 `hasBotIdLabel=true / hasSecretLabel=true / copyIcons=2 / bodyLen=565`，`sw=1280 == iw=1280` ⇒ 样式结论不是在空白页上测的 |
| 只读门 | `setup(wecom, confirmed=false)` → `need_confirmation`、`open_app=already_open`、走 B 路径「个人测试」不新建；plan 无「创建应用」 |
| `Watch(wecom)` | `chrome_ready=true / login=logged_in / in_app_detail=true` ⇒ 图3「capture 停在列表页」前置条件已满足 |
| `Capture(wecom)` 端到端 | 两次幂等 `phase=done`、`collected=[WECOM_BOT_ID, WECOM_BOT_SECRET]`、真连校验通过、`.env` 落库（35 / 43 位、无掩码）、11.44s（< 25s 上限） |
| shell 脚本幂等 | 9222 已运行时 `./scripts/chrome-debug.sh` 返回「无需重复启动」，未拉起第二个实例 |

**真机额外发现并修复的两个缺陷**（静态分析看不到，只有跑起来才暴露）：

| # | 现象 | 根因 | 修复 |
|---|---|---|---|
| ⑥ | 人为留官网营销页标签时 `start` 报 `ready_to_capture`，前台却仍是营销页（详情页 `vis=hidden`） | 提前返回分支只置 `PageOpened=true` 就 return，**没激活标签** | `launch.go` 新增 `activatePlatformTab`（详情页 > 列表页 > 域内标签，只读激活、绝不新开）；新增回归用例 `TestActivatePlatformTabPrefersDetailOverMarketing` |
| ⑦ | `Capture(wecom)` 两次 `capture_failed / context deadline exceeded`，恰好 25.00s | 分步 CDP 诊断（堆栈留证 `/tmp/hub-step-probe.log`）：后台标签 `requestAnimationFrame` 被 Chrome 暂停，rod `Element.Click → WaitInteractable → ScrollIntoView → WaitStableRAF` 永久等不到回调，耗光会话 ctx。与选择器、坐标、页面渲染均无关 | `browser.go` 的 `copyToCred` 改为读元素几何后用 `Mouse.MoveTo/Click` 派发 CDP 原生鼠标事件（复用 `copyInFrame` 已验证模式），`runOp` 前加 `ensurePageVisible` 尽量激活标签。**修复后在所有标签 `vis=hidden` 的极端状态下仍 `phase=done`** |

- **回归**：`gofmt` 空 / `go build ./...` / `go vet ./...` / 全量 6 包测试绿 / `bash -n scripts/chrome-debug.sh` 通过；临时探针 `zz_livecheck_test.go` 跑完即删、未进版本库；`bin/hub` 重建（18:52）。
- **诚实边界**：① stdio 宿主不会自动重载二进制，需在连接器页面禁用/启用 `mcp-hub`（或新开会话）后修改才在运行时生效；② 探针的 `reload` 是 no-op，`Capture` 返回的「已完成热重载」在探针里不成立，`.env` 要等连接器重拉才生效；③ 探测前仓库根 `.env` 已不存在，本轮 `Capture` 重新落库（权限 600），未回显任何凭据值；④ **同类隐患未扩张修复**：`runOp` 的 `click` / `clickInRow` / `hover` 仍走 rod `Element.Click/MustHover`，窗口最小化时理论上仍可能挂 `WaitStableRAF`——本轮刻意不动它们，因为改动会波及第十七轮已真机验证的钉钉/飞书链路，且无法在不触发不可逆动作的前提下安全回归；⑤ 未创建机器人、未改权限、未点发布、未重试提交。

**第二十一轮：按用户确认代建 `mcp-hub` 并端到端校准 `create-bot`**（2026-11-12 20:07–20:45，未提交）：用户确认「用默认名 mcp-hub 按照你的建议改」。本轮**真实创建了 1 个 API 模式智能机器人**，新凭据已写入 `.env`（35 / 43 位，不回显值）；`bin/hub` 重建 20:43。

| # | 真机暴露的事实 | 修复 |
|---|---|---|
| ① | `setupPlan` 给企微套 `app_console` 通用模板，确认清单出现「创建版本 1.0.0 并申请发布」；publish 阶段的 probe 还会点未校准的「发布/启用」按钮 | `stages.go` 的 wecom publish 改为纯只读状态核对 + `ApplyNote` → `not_required`，`PlanLine` 明确「无版本概念，不创建版本、不点发布」；新增 1 个回归用例 |
| ② | 执行器绑到后台首页 `#/index` 而非机器人列表页：`appListBase` 只比 host+path，而 `#/index`、`#/aiHelper/list`、`#/aiHelper/detail` 共用 `/wework_admin/frame` | `app_list.go` 新增 `appListHashPrefix`，声明了 `AppListURL` 的平台再比 hash 路径；飞书无声明，行为不变 |
| ③ | CDP `/json/new` 打开的列表页渲染成**英文 UI**（`Intelligent Bot Create / Created by company / Create Bot`），中文 markers 一条不中 → `listVerified=false` → 硬闸门永远拒绝代建 | `Portal.AppListMarkersAlt` 备用语言组（任一组全中即已渲染）+ `markersRendered` 空白归一化（SPA 的换行会把 marker 拆开）；首页只命中单个词不算通过 |
| ④ | 点「创建机器人」进入的是**独立 SPA 页** `#/aiHelper/create`，不是弹窗；旧 Ops 第 1 步在首页等「创建机器人」按钮必然 `selector_miss` | `create-bot` 脚本升级 `wecom@2026-11-12-create-api-mode`：`navigate` 复用/打开创建页 → 等名称框 → 切模式 → 填名称 → 填简介 → 生成 Secret → 保存 |
| ⑤ | 默认「普通模式」用的是企微自带模型；只有 **API 模式**才有 Bot ID / Secret 长连接凭据。模式 radio 的 `label` 自身无文本，按文本筛不到 | `clickAt` 支持 `Index`（与既有 `pick` 一致的 0-based 语义），真机 DOM 第 2 个 label 即 API 模式；radio 重复点击幂等 |
| ⑥ | 创建页常是 `hidden` 标签：rod 的 `MustClick/MustInput` 会挂 `WaitStableRAF`；即便用坐标点击且 `elementFromPoint` 命中按钮内部，hidden 状态下 React `onClick` 也不触发 | 新增 `clickAt`（读几何 + `scrollIntoView` + CDP 原生鼠标事件 + 点击前 `Activate`）、`fillJS`（原生 setter + input/change 事件 + 读回校验）两种 Op；只用于企微创建链路，不碰已验证的飞书/钉钉 `click` 链路 |
| ⑦ | 保存静默无效：toast「请输入简介」（简介必填）、补上后 toast「**生成Secret后方可保存机器人**」（API 模式须先点 `span.sdk-get-secret`） | Ops 补 `fillJS` 填简介 + `clickAt` 生成 Secret；新增 `Op.Optional`，Secret 生成后按钮消失时静默跳过，保证整套 Ops 幂等 |
| ⑧ | 保存其实**成功**了，但详情页跳转超过 15s；`waitForNewDetail` 超时误报 `create_failed`，`next` 还让用户「手动完成创建」——**会诱导重复提交不可撤销的创建表单** | `createAppURLWaitMax` 15s→30s；新增 `createdAppInList` 兜底（列表页确认新应用已出现 → `openAppForSetup` 进详情）；失败文案区分「已创建但没进详情」与「确实没创建」，两种都禁止重复提交 |

**验收**：`Watch` 停在 `#/aiHelper/detail?aibotid=…&tab=manage`；刷新列表页见 `Created by company mcp-hub API 模式`；`Capture(wecom)` 两次 `phase=done`（11.48s / 11.46s），`collected=[WECOM_BOT_ID, WECOM_BOT_SECRET]`，`.env` 落库 35 / 43 位；幂等门真机验证 `CreateApp(confirmed=true)` 返回 `already_in_detail`、`created_url` 为空、`Capture` 仍 `done`，**没有第二次创建**。`gofmt` 空 / `go build` / `go vet` / 全量 6 包测试 / `bash -n` 全绿；临时探针加了 `HUB_LIVE_PROBE=1` 守卫（防止 `go test ./...` 误触发有副作用的创建），跑完即删、未入库。

**诚实边界**：① 探针的 `reload` 是 no-op，「已完成热重载」在探针里不成立，`.env` 要等连接器重拉才生效；② stdio 宿主不会自动重载 `bin/hub` 与 `config.json`/`.env`/`server.js`，需**禁用/启用 `mcp-hub` 连接器**或新开会话；③ 新机器人尚未被拉进目标群（人工步骤），`im.send_text` 仍需显式 `target`；④ 事件桥 `.env` 仍无 `HUB_EVENT_SECRET`，上一轮修的适配器默认回落与白名单也需宿主重载才生效；`chats` 表里的旧 chatid 属已删除的旧机器人，**不要复用**；⑤ 未改权限、未点发布、未回显任何凭据值、未提交；`dom_verified` 维持 `false`（权限阶段未校准）。

**端到端真机执行**（2026-11-11 第六轮，`setup(platform="dingtalk", confirmed=true)`）：留证 `/tmp/setup-true-20261111.json`，结果 `phase=partial`，**与预期不同**，如实记录：

| 阶段 | 实测 | 说明 |
|---|---|---|
| `open_app` | `already_open` | 已有应用详情页，未新建 |
| `capture` | `incomplete` | `Client Secret` 掩码，按设计拒绝写 `.env`、降级 `set_credentials`；`.env` 实测仍只有飞书两项，无误写 |
| `enable_perms` | `selector_miss` | apply 第 5 步 `waitVisible tr[data-row-key]` 按 `/Calendar.Event.Read/` 未命中。前 3 步已执行 ⇒ **`Contact.User.Read` 的「立即开通」被真实点击过**（20:20 只读复核确认它已变「已开通 / 移除权限」、页面无二次确认弹窗）；第 2 个权限点未执行 |
| `publish` | `already_done` | 版本页已有 `1.0.0 已上线`，`Done` 判幂等跳过，**没有生成 1.0.1 草稿，也没进版本表单页** ⇒ 发布按钮仍未采集 |

执行耗时 8.85s（<25s 会话上限），第 5 步的 `context deadline exceeded` 是 `ElementR` 单步 6s 超时，属真实 `selector_miss`，不是会话耗尽。执行后只读快照（`/tmp/ding-state-after-20261111.txt`）确认版本表格仍只有 `1.0.0 已上线` 一行、无可见弹窗。据此修两处诚实性问题：① 旧版 `PlanLine` 承诺「自动创建新版本并保存、会产生版本记录」，与实际幂等跳过不一致，改为写明「已有上线版本则跳过、不产生新记录」；② 权限阶段补 `ApplyWarning`，`apply` 全跑完也只标 `applied_unverified`，明确未完整验证、不重试提交。`dom_verified` **按实测结果保持 false**——端到端没跑通就不能标绿，已实测部分由 `app_list_verified` / `tabs_verified` 单独标注。

**扫码恢复登录态后的只读复核与修复**（2026-11-11 第七轮）：SSO 挑战页人工扫码后，用纯只读 DOM 快照（`/tmp/ding-peek-all-20261111.txt`、`/tmp/ding-elementr2-20261111.txt`）拿到三条硬事实：① `Contact.User.Read` 已是「已开通 / 移除权限」，整页无二次确认弹窗 ⇒「立即开通」**一步生效**，旧版「可能有确认框」的猜测证伪；② `Calendar.Event.Read` 仍「未开通」，且连续 3 次覆盖式 `fill` + `ElementR` 全部命中 ⇒ 第 5 步不是选择器/正则问题，而是点击后列表异步刷新让下一次 `fill/waitVisible` 落在过渡态；③ 已上线版本的详情页只有「查看 / 查看版本详情」，没有发布按钮 ⇒ **采集「保存」后的发布按钮只能真保存，无法只读取得**。据此把权限 apply 从每 code 3 步改为 5 步：先等搜索框重新渲染 → 按 code 过滤 → 等行 → `clickInRow` 带 `SkipIfMatch`（已开通的行按钮是「移除权限」，幂等跳过而不是 `selector_miss`）→ 等该行变「已开通」作为下一个 code 的稳定锚点。`ApplyWarning` 同步改为「新增序列未经真机端到端执行」，不再宣称二次确认框未采集。新增序列只做了只读证据链校准，**未再跑 `confirmed=true`**，避免重复不可逆动作。

**用户授权后的二次端到端执行**（2026-11-11 第八轮，`setup(platform="dingtalk", confirmed=true)`，用户明确说「执行」）：留证 `/tmp/setup-true-20261111-v2.json` 与只读快照 `/tmp/ding-snapshot-20261111.txt`，结果 `phase=partial`，**比第六轮更进一步、但仍有诚实偏差**，如实记录：

| 阶段 | 实测 | 说明 |
|---|---|---|
| `open_app` | `already_open` | 已有应用详情页，未新建 |
| `capture` | `incomplete` | `Client Secret` 掩码降级 `set_credentials`（同第六轮） |
| `enable_perms` | `applied_unverified` | 五个 step 序列**跑通**（不再 `selector_miss`），Calendar.Event.Read 的「立即开通」已点；事后只读快照确认 Calendar.Event.Read 已是「无需审批 已开通 移除权限」⇒ **权限副作用真实生效**（运行时仍保守标 `applied_unverified`） |
| `publish` | `applied_unverified` | 五个 step 跑完、标 `applied_unverified`，但事后只读快照确认版本列表仍只有 `1.0.0 已上线`、**没有 1.0.1 草稿** ⇒ 「保存」并未生成待发布版本 |

**诚实偏差**：第六轮把 publish 标 `already_done`（没进表单）；第八轮标 `applied_unverified`（apply 执行了）但**仍未生成草稿**。

### 第十二轮：钉钉发布链路真机跑通（2026-11-12）

在第八轮基础上又跑了三轮 `setup(dingtalk, confirmed=true)`，前两轮**仍无草稿**，靠只读探针（`formprobe` 读 `el.value`、`saveprobe` 统计「保存」候选、`scoreprobe` 打分）逐层定位，才发现此前两个判断都不完整：

| 轮次 | 修复 | 结果 |
|---|---|---|
| 第九轮 | 补 `fill input#version = "1.0.1"` | 版本号真机写入成功且跨 Chrome 重启留存，但**草稿仍不生成** |
| 第十轮 | 发现版本描述 label 带 `dtd-form-item-required`（必填），而 `MustInput` 键盘事件在焦点未落 textarea 时**静默丢失**（版本号生效、描述落空的非对称现象）→ `fill` 加读回校验 + JS 原生 setter 降级 | 版本号与描述**双双写入成功**，草稿**仍不生成** |
| 第十一轮 | 真根因：`ElementR` 返回文档顺序第一个匹配元素，而 `menuCandidateSelector` 里 `div` 排在 `button` 之前，「保存」命中的是 `div.sectionBottom`（**968px 宽的按钮容器**），点它不触发内层 `button` 的 React 事件 ⇒ **点击静默无效**。修法：`pick` 改走 `pickByText` + `clickability` 打分（`BUTTON`/`[role=button]`/`A` 加分，容器、超宽、隐藏、disabled 扣分） | **`1.0.1 开发中` 草稿真实生成**；点该行「查看」进 `/version/app_publish/<id>`，采集到「发布」按钮为 `BUTTON.dtd-btn.dtd-btn-primary`（外层 `div.right` 同样是干扰容器） |

**幂等验证**：紧接着再跑一次，`publish => already_done`，版本列表仍只有 `1.0.1 开发中` 一个草稿，无重复创建。

**gate**：钉钉 `dom_verified` 置为 `true`（版本表单字段、提交动作、落库校验、发布按钮 DOM、权限逐 code 开通均已真机验证）。发布动作本身刻意**不由 Hub 执行**——不可逆企业动作留给人工。创建应用表单（`/app/create`）仍未走过，属 `create_app.go` 独立链路。

> 只读诊断 `debug/statecheck` 曾误用 rod 的 `browser.MustClose()`（会发 CDP `Browser.close` 关掉整个调试 Chrome），导致调试浏览器两次「自然退出」；已移除。登录（SSO 扫码）仍必须人工，Hub 不代做。

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
