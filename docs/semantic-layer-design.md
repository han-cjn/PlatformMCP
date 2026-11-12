# 统一语义层：一次编写、多平台可用

> 版本 v1.1 · 2026-11-06 · 对应实现：`internal/semantic/{capability,mapping,capabilities,markdown,engine,adapters}.go`

## 0. 变更记录
变更记录

| 版本 | 变更 |
|---|---|
| v1.0 | 落地领域模型 + 声明式映射 + 执行引擎；`im.send_text` 三平台跑通 |
| v1.1 | 新增 `format` 参数支持 Markdown；飞书用 Adapter 转 `post` 富文本；新增 markdown 转换器与 11 项测试 |
| v1.2 | 扩展到文档能力:`doc.create` / `doc.append` / `doc.search`;腾讯 base64 用 Adapter,飞书追加显式 Unsupported;report 的 CreateDoc/UpdateDoc 改走语义层 |
| v1.3 | 扩展 `table.query` 跨平台(飞书+钉钉):统一操作符词汇翻译、empty/notEmpty 空值数组处理、钉钉 operator_id 必填透传;新增 9 项测试 |
| v1.4 | 扩展 `table.create` / `table.update`:飞书单条 API 循环调用 vs 钉钉批量 API;幂等键支持;新增 8 项测试 |

## 1. 要解决什么问题
要解决什么问题

改造前，语义层名不副实：

| 问题 | 代码位置 | 后果 |
|---|---|---|
| `payload` 原样直通给下游 | `cmd/hub/main.go` 旧注册逻辑 | 客户端仍要拼飞书 `params.receive_id_type` + `data.content`，没有真正屏蔽平台差异 |
| 平台差异用 `switch platform` 硬编码 | `internal/semantic/send.go` 旧实现 | 只覆盖 `im.send_text`；加平台/加能力都要改 Go 代码 |
| 能力矩阵只有 3 格 | `config.json` 的 `routes` | `table.query` 仅飞书、`doc.create` 仅腾讯文档，跨平台能力稀疏 |

**目标**：把「平台差异」从代码里提取出来变成数据。新增平台只加一份映射声明，不改 Go 代码。

## 2. 三层结构
三层结构

```
客户端（只写领域模型）
    ↓  { target, text, platform? }
语义层引擎（校验 → 映射 → 调用）
    ↓  平台专属参数
mapping 声明（每平台一份）
    ↓
下游 MCP 工具
```

### 2.1 领域模型（Capability）

描述「平台无关」的入参，同时用于**校验**和**生成 MCP 工具 schema**。

```go
type Capability struct {
    Name        string                     // im.send_text
    Title       string                     // 发送文本消息
    Description string                     // 给客户端看的说明
    Schema      map[string]FieldSpec       // 领域字段定义
    Required    []string                   // 必填字段
    Platforms   map[string]PlatformMapping // platform -> 映射规则
}

type FieldSpec struct {
    Type    string   // string / number / bool / object / array
    Desc    string
    Enum    []string // 可选值约束
    Default any      // 缺省值
}
```

`im.send_text` 的领域字段只有 3 个：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `target` | string | ✓ | 目标会话 ID。四平台叫法不同，统一收敛为 `target` |
| `text` | string | ✓ | 消息正文 |
| `title` | string | - | 可选标题，仅钉钉生效 |

**为什么叫 `target` 不叫 `chat_id`？** 四平台分别叫 `chat_id` / `openConversationId` / `chatid`。用中性词让客户端理解一致。

### 2.2 映射声明（PlatformMapping）

```go
type PlatformMapping struct {
    Tool        string            // 下游工具名
    Args        map[string]string // 下游参数路径 -> 表达式
    Const       map[string]any    // 固定值
    Encode      []FieldEncode     // 需序列化后嵌入的字段
    Adapter     string            // 复杂场景的具名 Go 适配器
    Unsupported string            // 非空表示明确不支持,值为原因
}
```

**表达式语法刻意保持极小**，避免变成需要调试的迷你编程语言：

| 语法 | 含义 |
|---|---|
| `$.field` | 取领域入参字段 |
| `$const.VALUE` | 字面量（自动推断 string/number/bool） |
| `$.field ?? fallback` | 取字段，空值时用右侧 |

覆盖不了的复杂场景（多步调用、需先建会话）用 `Adapter` 字段指向具名 Go 函数兜底。**普通适配零代码，复杂才写代码。**

### 2.3 三平台映射实例

**飞书**（`data.content` 是 JSON 字符串）：

```go
"feishu": {
    Tool: "im_v1_message_create",
    Const: map[string]any{
        "params.receive_id_type": "chat_id",
        "data.msg_type":          "text",
    },
    Args: map[string]string{"data.receive_id": "$.target"},
    Encode: []FieldEncode{{
        Target: "data.content",
        Format: "json",
        From:   map[string]string{"text": "$.text"},
    }},
}
```

**钉钉**（`msgParam` 传 object，由官方 MCP 序列化一次）：

```go
"dingtalk": {
    Tool: "sendMessageToGroupByRobot",
    Args: map[string]string{"openConversationId": "$.target"},
    Encode: []FieldEncode{{
        Target: "msgParam",
        Format: "plain",  // 不能写 "json":官方 dingtalk-mcp 会再 stringify 一次
        From: map[string]string{
            "title": "$.title ?? MCP Hub",  // 缺省回退
            "text":  "$.text",
        },
    }},
}
```

> 2026-11-12 真机实测：`dingtalk-mcp@1.1.21` 的 `dist/DingTalkMCPServer.js`
> 对 `extendType:'json'` 参数执行 `JSON.stringify(args[name])`。若 Hub 侧用
> `Format:"json"` 先生成字符串，会被二次序列化成带转义的字符串字面量，钉钉 API
> 返回 `invalidParameter.msgParam.invalid / msgParam必须是json格式`。钉钉 API 本身
> 要的确实是 JSON 字符串，但**只能由官方 MCP 生成一次**（直连 API 时则自己生成一次）。
> 修复后真机探针（传 object + 必然无效的会话 ID）返回 `invalid.openConversationId`，
> 证明消息体已被接受；经宿主 `im.send_text` 的完整链路复跑仍需重拉连接器。

**企微**（平铺，最简单）：

```go
"wecom": {
    Tool: "wecom_send_text",
    Args: map[string]string{"chatid": "$.target", "content": "$.text"},
}
```

**腾讯文档**（无 IM 能力，显式声明而非静默缺失）：

```go
"tencent-docs": {Unsupported: "腾讯文档无 IM 消息能力"}
```

## 3. 执行引擎
执行引擎

```go
func Invoke(ctx, cat *Catalog, caller Caller, capName, platform string,
    domain map[string]any, preferred []string) (*InvokeResult, error)
```

流程：

1. **选平台** — `platform` 为空时，按 `preferred`（配置 `default_platform_order`）→ 字典序，选第一个声明可用的平台
2. **查映射** — 未声明报「未声明平台」，`Unsupported` 非空报具体原因
3. **校验** — 补默认值 → 检查必填 → 类型/枚举检查
4. **映射** — `Const` → `Args` → `Encode`，路径支持 `a.b.c` 嵌套写入
5. **调用** — `caller.CallTool`，返回 `InvokeResult{Text, Platform, Capability, Tool}`

`Caller` 是接口，生产传 `*adapter.Manager`，测试注入假实现，**映射逻辑可脱离真实 MCP 连接单测**。

## 4. 工具注册的变化
工具注册的变化

**改造前**（客户端要拼 payload）：

```
im.send_text(platform: string, payload: string)   // payload 是 JSON 字符串
```

**改造后**（客户端只写领域字段）：

```
im.send_text(target: string, text: string, title?: string, platform?: string)
```

实际实测输出（`tools/list`）：

```json
{
  "properties": {
    "platform": {"description": "目标平台,可选值:dingtalk / feishu / wecom;省略则自动选择第一个可用平台", "type": "string"},
    "target":   {"description": "目标会话 ID。四平台叫法不同(飞书 chat_id / 钉钉 openConversationId / 企微 chatid),此处统一传 target", "type": "string"},
    "text":     {"description": "消息正文(纯文本)", "type": "string"},
    "title":    {"description": "可选标题,仅钉钉生效(其余平台忽略)", "type": "string"}
  },
  "required": ["target", "text"],
  "type": "object"
}
```

## 5. 能力矩阵自检
能力矩阵自检

启动时打印，声明支持与实际挂载的差异一目了然：

```
capability    dingtalk      feishu        tencent-docs  wecom
----------------------------------------------------------------------
im.send_text  ○             ✓             ✗             ✓

图例:✓ 支持且已挂载   ○ 支持但未挂载   - 未声明   ✗ 明确不支持
```

## 6. 向后兼容
向后兼容
`SendText(ctx, reg, mgr, platform, chatID, text)` 签名保持不变，内部改为走引擎。既有 3 个调用点无需修改：

- `internal/event/rules.go:80` — `#记录` 指令回执
- `internal/report/report.go:414` — 日报推送
- `cmd/hub/main.go:266` — 不支持消息类型提示

`reg *Registry` 参数保留但不再用于解析工具名（路由已内置到能力定义）。**新代码请直接用 `Invoke`。**

## 7. Markdown 支持（v1.1）
Markdown 支持（v1.1）

### 7.1 为什么不设独立的 im.send_markdown

实测三个平台的下游工具后发现了关键事实：

| 平台 | 下游工具 | 真实行为 |
|---|---|---|
| 钉钉 | `sendMessageToGroupByRobot` | `msgParam.text` 官方描述就是「**markdown格式**」 |
| 企微 | `wecom_send_text` | 内部发送 `msgtype:'markdown'`、`markdown:{content}` |
| 飞书 | `im_v1_message_create` | `msg_type` 区分 `text` / `post`；text 会把 markdown 当**纯文本显示** |

也就是说钉钉和企微的"文本"接口**本身就按 markdown 渲染**。如果再加一个 `im.send_markdown`，在这两个平台上会与 `im.send_text` 完全等价——纯粹是语义重复。

**因此改用 `format` 参数**表达"要不要按 markdown 渲染"：

| format | 飞书 | 钉钉 | 企微 |
|---|---|---|---|
| `plain`（默认） | `msg_type=text`，`content={"text":...}` | 原文传 `msgParam.text` | 原文传 `content` |
| `markdown` | **`msg_type=post`**，转嵌套富文本 | 原文传（原生渲染） | 原文传（原生渲染） |

### 7.2 飞书 post 转换器

飞书 `post` 要求嵌套结构，这是声明式映射（字段路径 + 序列化）**表达不了**的类型转换，因此用 `Adapter` 兜底——这也验证了 v1.0 预留 Adapter 机制的必要性：

```json
{"zh_cn":{"title":"日报","content":[
  [{"tag":"text","text":"标题","style":["bold"]}],
  [{"tag":"text","text":"重点","style":["bold"]},{"tag":"text","text":" 完成了 "}],
  [{"tag":"text","text":"• 条目一"}],
  [{"tag":"text","text":"• 条目二"}]
]}}
```

转换器 `MarkdownToFeishuPost` 支持：

| Markdown | 转换为 |
|---|---|
| `# 标题` | 提取为 post `title`，正文加 `bold` 样式 |
| `**粗体**` | `style:["bold"]` |
| `*斜体*` | `style:["italic"]` |
| `` `代码` `` | `style:["code"]` |
| `~~删除线~~` | `style:["lineThrough"]` |
| `[文字](url)` | `{"tag":"a","text":..,"href":..}` |
| `- 列表项` | 独立一行，前缀 `•` |
| `1. 列表项` | 独立一行，保留序号 |

**设计取舍**：只做保守子集，不支持嵌套列表、表格、代码块、HTML。不支持的语法**降级为纯文本**（宁可少渲染，也不显示乱码或报错）。

### 7.3 踩坑记录：列表项必须独立成行

初版实现按空行切块，导致连续列表项被合并进同一 post 行，渲染成一整行文字，列表结构丢失。测试断言"至少 4 行"失败后定位到根因：

> 飞书 post 的 `content` 是 `[][]inline`，**外层每个元素是一个显示行**。多个列表项塞进同一行会渲染成连续文字。

修复：`splitBlocks` 遇到列表项/标题时强制打断段落合并（`flushPara`），让每项独立成行。

## 8. 文档能力（v1.2）

### 8.1 能力矩阵现状

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

**能力不对称是真实的，如实声明而非强行统一**：

| 能力 | 腾讯文档 | 飞书 | 钉钉 | 企微 |
|---|---|---|---|---|
| `doc.create` | `doc.create_with_markdown`（base64） | `docx_builtin_import`（直接 markdown） | ✗ | ✗ |
| `doc.append` | `doc.insert_markdown`（需 idx） | ✗ 无追加工具 | ✗ | ✗ |
| `doc.search` | `manage.search_file` | `docx_builtin_search` | ✗ | ✗ |
| `table.query` | ✗（sheet.* 结构差异大，未收敛） | `bitable_v1_appTableRecord_search` | `listNotableRecords` | ✗ |

### 8.2 关键设计决策

**1. 腾讯建文档用 Adapter，不是声明式映射**

`doc.create_with_markdown` 要求 `base64_markdown`（base64 编码字符串）。字段映射表达不了"编码"这种转换，所以用 `tencent.doc_create` adapter：

```go
"tencent-docs": {Tool: "doc.create_with_markdown", Adapter: "tencent.doc_create"}
// adapter 内部:base64.StdEncoding.EncodeToString([]byte(markdown))
```

**2. 飞书追加显式 Unsupported，而非留空**

飞书 `docx_v1_document_rawContent` 只能读，`docx_builtin_import` 只能整篇导入。没有"向已有文档追加"的工具。强行用"先读出全文 + 拼接 + 整篇导入"模拟，会覆盖原文档、丢失格式，是错误抽象。如实标记 `Unsupported`，客户端得到明确报错，而不是静默得到一篇被破坏的文档。

**3. 领域入参归一**

| 领域字段 | 腾讯 | 飞书 |
|---|---|---|
| `title` | `title` | `data.file_name` |
| `markdown` | `base64_markdown`（编码） | `data.markdown`（原文） |
| `doc_id` | `file_id` | — |
| `position` | `idx` | — |
| `keyword` | `search_key` | `data.search_key` |

### 8.3 report 链路已改走语义层

`report.go` 的 `CreateDoc`/`UpdateDoc` 原来手写 `base64.StdEncoding.EncodeToString`，现改走 `semantic.Invoke` 调 `doc.create`/`doc.append`。base64 编码逻辑收敛到 adapter，消除重复。

## 9. 表格能力（v1.3）

### 9.1 真实差异

| 维度 | 飞书 `bitable_v1_appTableRecord_search` | 钉钉 `listNotableRecords` |
|---|---|---|
| 定位表格 | `path.app_token` + `path.table_id` | `baseId` + `sheetIdOrName` + **`operatorId`** |
| 筛选条件键 | `data.filter.conditions[{field_name, operator, value}]` | `filter.conditions[{field, operator, value}]` |
| 条件组合键 | `conjunction` | `combination` |
| 操作符词汇 | `is / isNot / contains / isGreater / isLess ...` | `equal / notEqual / contain / greater / less ...` |
| 分页 | `params.page_size` / `page_token` | `maxResults` / `nextToken` |

**`table.query` 只能是最小公约数**。领域操作符用统一词汇（`eq/ne/contains/not_contains/empty/not_empty/gt/gte/lt/lte`），由 `opMap` 翻译成平台词汇。

### 9.2 踩坑：empty/notEmpty 的 value 处理

初版实现无条件传 `value: [""]`，飞书报 `InvalidFilter`。查官方文档确认：

> 当 operator 为 isEmpty 或 isNotEmpty 时，value 需填空值 `[]`，且**请确保按照 `"value":[]` 格式传入，否则将报缺失 value 值的错误**。

修正：`OpNeedsValue` 判定语义上是否需要值，飞书 adapter 对 `empty/not_empty` 传 `[]string{}`。

### 9.3 钉钉 operator_id 必填

钉钉智能表格所有工具都要求 `operatorId`（操作人 unionId），这是平台特有必填项。领域模型把它作为可选字段，钉钉 adapter 里校验缺失时报错。

### 9.4 实测验证

| 场景 | 结果 |
|---|---|
| 飞书 `not_empty`（内容非空） | **14 条** |
| 飞书 `eq`（来源平台=wecom） | **10 条** |
| 飞书 `contains`（内容含"终验"） | **10 条** |
| 飞书 `and` 组合（来源=wecom 且内容含"M2"） | **1 条** |
| 钉钉缺 operator_id | `必须提供 operator_id(操作人 unionId)` |
| 企微查询 | `不可用: 企业微信无在线表格能力` |

### 9.5 写入能力（table.create / table.update）

**平台差异**：

| 维度 | 飞书 | 钉钉 |
|---|---|---|
| API 形态 | **单条**（`bitable_v1_appTableRecord_create/update`） | **批量**（`insertNotableRecords`/`updateNotableRecords`） |
| 记录定位 | `record_id` 在 `path` 里 | `records[].id` 里 |
| 幂等 | `params.client_token`（uuidv4） | 无 |

**领域模型统一为批量**：`records: [{id?, fields: {字段名: 值}}]`。
- 飞书 Adapter **循环调用**单条 API，每失败一条即报错并指明序号
- 钉钉 Adapter 一次传入批量数组
- 飞书幂等键 `idempotency_key` → `params.client_token`，同 key 重复调用不产生重复记录（实测验证）

**实测**：
- 真实创建记录成功（`成功创建 1 条记录`）
- 幂等键重复调用未产生重复记录（查询验证 `semantic-test` 来源仅 1 条幂等测试记录）
- 更新缺 `id` 报错：`第 1 条记录缺少 id(更新必须指定记录 ID)`

## 10. 实测验证
实测验证

### 8.1 v1.0 基础链路

| 场景 | 结果 |
|---|---|
| 飞书真实链路 | 下游返回 `code:230001 invalid receive_id` — 证明请求真实到达飞书，`target → data.receive_id` 映射正确 |
| 腾讯文档 | `能力 "im.send_text" 在 tencent-docs 上不可用: 腾讯文档无 IM 消息能力` |
| 缺 text | `im.send_text 入参非法: 缺少必填字段 text` |
| 空 target | `im.send_text 入参非法: 缺少必填字段 target` |
| 未知能力 | `tool 'no.such' not found` |

### 8.2 v1.1 Markdown 链路

用同一段 markdown（标题 + 粗体 + 行内代码 + 链接 + 两个列表项）实测四平台：

| 场景 | 下游返回 | 判读 |
|---|---|---|
| 飞书 `format=markdown` | `code:230001 invalid receive_id` | **`msg_type=post` 的嵌套结构被接受**，仅会话 ID 无效 |
| 飞书 `format=plain` | `code:230001 invalid receive_id` | text 类型结构正常 |
| 钉钉 `format=markdown` | `code:invalid.openConversationId` | markdown 原文被接受 |
| 企微 `format=markdown` | `发送失败: undefined` | 已发到 WS 层，会话 ID 无效 |
| 非法 `format=html` | `format 取值 html 不在允许范围 [plain markdown]` | 枚举校验生效 |

> 判读方法：用**必然无效的 target** 调用，如果错误是「会话 ID 无效」而不是「参数结构错误」，说明消息体构造完全正确，只是找不到会话。这是不打扰真实群聊前提下验证映射正确性的可靠手段。

### 8.3 v1.2 文档链路真实调用

启动真实 Hub，用 MCP 协议实测，结果全部符合设计：

| 调用 | 下游返回 | 判读 |
|---|---|---|
| 腾讯 `doc.create` | `{"file_id":"cTwXGYeXrLND","file_url":"//docs.qq.com/doc/...","version":1}` | **真实创建成功**，base64 编码链路正确 |
| 飞书 `doc.create` | `code:99991672 Access denied ... [drive:drive, docs:document:import]` | 工具/参数结构正确，**仅应用缺权限**（与语义层无关，需开权限） |
| 企微 `doc.create` | `能力 "doc.create" 在 wecom 上不可用: 企业微信无在线文档创建能力` | Unsupported 声明生效 |
| 飞书 `doc.append` | `能力 "doc.append" 在 feishu 上不可用: 飞书 docx 暂无追加/插入工具` | 如实声明能力缺失 |
| 腾讯 `doc.append` | `code:11510 no rows in result set` | 参数结构正确，`file_id` 不存在导致业务错误 |
| 腾讯 `doc.search` | 返回真实文档列表（含「群聊记录日报 2026-11-02」「北京实用信息文档」） | **真实搜索成功** |

### 8.4 单测覆盖

50 项（v1.4 新增 8 项表格写入测试），覆盖：三平台映射等价性、Markdown→post 转换、format 枚举校验、base64 编码解码还原、飞书追加 Unsupported、操作符词汇翻译、empty/notEmpty 空值数组处理、钉钉 operator_id 必填校验、批量/单条结构组装、幂等键透传、矩阵包含全部能力行。

## 11. 扩展新平台 / 新能力

**加平台**（以新增"企业微信应用消息"为例）：在 `capabilities.go` 的 `Platforms` 里加一段：

```go
"wecom-app": {
    Tool: "app_send_text",
    Args: map[string]string{"touser": "$.target", "content": "$.text"},
},
```

**加能力**：定义新的 `*Capability`，加进 `builtinCapabilities`。工具注册、schema 生成、矩阵自检**全部自动生效**。

## 12. 已知边界
已知边界

- **`table.query` 跨平台统一是难点**，不建议硬做。飞书 Bitable、钉钉智能表格、腾讯文档表格的字段类型、筛选语法、分页方式都不同。建议只统一最常用子集（"按条件查行、返回列名到值的 map"，筛选表达式统一为 `field eq value`），超出子集时直接用平台限定工具 `feishu.bitable_v1_appTableRecord_search`。**不要为了统一而假装统一。**
- 表达式不支持条件分支、循环、函数调用。遇到这类需求用 `Adapter` 兜底，而不是扩展表达式语法。
- `Encode.Format` 目前只有 `json` / `plain`；新格式需在 `Build` 里加分支。
