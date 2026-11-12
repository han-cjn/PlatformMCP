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
2. **L2 浏览器抓取**：四平台的 `collect-creds` 都挂了声明式动作。飞书按实测 DOM 读 `.secret-code__code`；钉钉的详情页是单页多 tab（`CredTab` / `CredTabFragment` 声明），会**先点「凭证与基础信息」切到 `/baseinfo`** 再用 `readPage` + 正则抽取（零 class 依赖），企微/腾讯文档直接读整页文本。钉钉 `/baseinfo`：`Client ID` 明文可读；`Client Secret` **页面渲染**为 `当前使用:xxxx****xxxx` 掩码，但同一行右侧复制图标写入剪贴板的是**完整明文**（2026-11-12 用户手工点复制确认）。因此新增 `copyToCred` Op：注入 hook 拦截 `clipboard.writeText` / `execCommand('copy')` 捕获明文（**不读系统剪贴板**），捕获不到才降级 `navigator.clipboard.readText()`；按「文本最短的凭证块」定位，避免误点 `Client ID` 的复制按钮。读回值仍交格式校验，**绝不把掩码写进 `.env`**；复制按钮定位失效才按 `next` 降级 L1。主路径与降级路径已在本地构造页真机验证，**钉钉真机端到端尚未验证**（登录态已清，见下段）。
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
- **诚实边界**：`copyToCred` 的定位选择器基于历史 DOM dump 与截图，**钉钉真机端到端尚未跑过**（上一轮按用户要求清掉了调试 Chrome 与 profile，登录态已失效）。因此 `dom_verified` 不因本次改动扩张，失败仍按 `selector_miss` 降级 `set_credentials`。要闭环需重启调试 Chrome + 用户重新扫码登录。

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
