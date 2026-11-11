# 跨平台协作中台方案：腾讯 / 钉钉 / 飞书 文档 + IM 本地打通

> 版本 v1.0 · 2026-10-29 · 目标:本地运行,以 MCP 协议为统一接入层,打通三平台文档与 IM 能力

---

## 1. 需求澄清与边界

| 需求 | 解读 | 可行性结论 |
|---|---|---|
| 腾讯文档 | 官方远程 MCP(`https://docs.qq.com/openapi/mcp`),HTTP + Token | ✅ 官方直连 |
| 腾讯 IM | **已确认:企业微信**。发送走社区 webhook 机器人 MCP;接收走回调 + frp 内网穿透(已确认接受) | ✅ 企微落地 |
| 钉钉文档 + IM | 官方 DingTalk OpenAPI MCP(中间件桥接开放平台 API),覆盖消息/日程/任务/文档 | ✅ 官方直连 |
| 飞书多维表格 + IM | 官方 `lark-mcp` 本地 stdio,`preset.base.default` 覆盖 Bitable 全套 + IM | ✅ 官方直连 |
| 本地运行 | 所有 MCP Server 以 stdio/本地 HTTP 运行;**接收消息**是关键难点(见 §4.2) | ✅ 可落地 |

**核心架构决策**:不自己造 SDK 轮子,以「官方/社区 MCP Server 为适配层 + 本地统一 Hub 为编排层」。

---

## 2. 总体架构

```
┌─────────────────────────────────────────────────────────┐
│  AI 客户端层 (Trae / Claude / 自建 Agent)                │
│        │  MCP (stdio / streamable HTTP)                 │
├────────▼────────────────────────────────────────────────┤
│  本地 MCP Hub (Go 服务, 自研, ~核心交付物)                │
│  ├─ 工具聚合: 挂载下游 4 个 MCP Server, 命名空间隔离       │
│  │   (tencent.* / dingtalk.* / feishu.* / wecom.*)       │
│  ├─ 统一抽象: im.send / doc.read / table.query 语义层     │
│  ├─ 事件桥: 平台事件 → Redis Stream → 跨平台消息路由       │
│  ├─ 治理: 凭据管理 / 限流 / 幂等 / 审计日志                │
│  └─ ID 映射中心: 平台 ID ↔ 统一内部 ID (MySQL/SQLite)     │
├────────┬────────────┬────────────┬──────────────────────┤
│腾讯文档MCP│ DingTalk  │  lark-mcp  │ wecom-bot-mcp        │
│ (远程HTTP)│ 官方MCP   │ (本地stdio)│ (社区, webhook发送)   │
│          │ (stdio)   │            │ + 事件回调接收服务      │
└──────────┴───────────┴────────────┴──────────────────────┘
```

### 2.1 分层职责

| 层 | 职责 | 不做的事 |
|---|---|---|
| 适配层(各平台 MCP) | 协议转换、平台 API 调用、token 刷新 | 不含业务逻辑 |
| Hub 编排层(自研 Go) | 工具路由、统一语义、事件总线、幂等、审计 | 不直接调平台 OpenAPI |
| 客户端层 | 自然语言/规则触发的跨平台编排 | 不感知平台差异 |

---

## 3. 各平台接入明细

### 3.1 飞书(最成熟,先做)

```jsonc
// Hub MCP 配置
{
  "mcpServers": {
    "feishu": {
      "command": "npx",
      "args": ["-y", "@larksuiteoapi/lark-mcp", "mcp",
               "-a", "<APP_ID>", "-s", "<APP_SECRET>",
               "-p", "preset.base.default,preset.im.default"],
      "transport": "stdio"
    }
  }
}
```

- **能力**:Bitable 增删改查(`bitable.v1.appTableRecord.*`)、消息发送、群组管理
- **凭据**:自建应用 App ID/Secret,tenant_access_token 由 MCP 自管
- **接收消息**:飞书支持 **WebSocket 长连接事件订阅**,本地运行无需公网 —— 这是本地化的关键优势

### 3.2 钉钉

- 使用官方 **DingTalk OpenAPI MCP**(服务端 API 中间件),本地 stdio 运行,配置 ClientID/ClientSecret
- **能力**:工作通知/群消息发送、待办、日程、文档 API(需在开放平台开通文档权限)
- **接收消息**:钉钉 **Stream 模式**(长连接)事件推送,同样无需公网,本地直连 ✅
- 权限提醒:文档 API 权限独立于 IM,需要在开放平台单独申请 scope

### 3.3 腾讯文档

```jsonc
{
  "mcpServers": {
    "tencent-docs": {
      "url": "https://docs.qq.com/openapi/mcp",
      "headers": { "Authorization": "<TOKEN>" }
    }
  }
}
```

- 远程 HTTP 接入,零部署;能力覆盖文档/表格/智能表的创建、查询、编辑
- **局限**:文档类平台无 IM 事件概念,只做「被调用」,不做「事件源」

### 3.4 腾讯 IM(企业微信,**已确认为智能机器人 API 模式**)

| 子能力 | 方案 | 备注 |
|---|---|---|
| 发送消息 | **企微智能机器人(Bot ID + Secret,API 模式)** — 已实现本地 MCP 适配器 `hub/adapters/wecom-aibot-mcp/`(官方 @wecom/aibot-node-sdk 薄封装,stdio 挂载,工具 `wecom_send_text`) | 管理后台 → 智能专区 → 智能机器人;个人/群会话均可发 |
| 接收消息 | **智能机器人 SDK 长连接**,与飞书 WS / 钉钉 Stream 同构,**不再需要 frp 穿透** | ✅ 已确认采用,用户已创建机器人并授权消息/文档/待办/日程 |
| 应用消息/文档 | 智能机器人已授权文档、待办 scope,直接可用 | |
| 备选 | 群机器人 webhook(仅固定群广播场景) | 社区 `wecom-bot-mcp`,50 行可自研替代 |

> 决策更新(2026-10-30):原计划「webhook 群机器人发 + frp 穿透收」改为「智能机器人收发一体」。frp 穿透从必选项降为备选,本地运行链路进一步简化。智能机器人官方若无现成 MCP,Hub 适配层为其内置一个薄实现(Bot ID/Secret 长连接 SDK),仍走统一语义层。

---

## 4. 关键技术设计

### 4.1 统一语义层(Hub 核心)

把三个平台能力收敛为最小公共语义集,避免客户端写平台 if-else:

| 统一工具 | 飞书映射 | 钉钉映射 | 企微映射 |
|---|---|---|---|
| `im.send_text(target, text)` | `im.v1.message.create` | 工作通知/群消息 API | webhook send |
| `im.on_message` 事件 | WS 长连接 | Stream 模式 | 穿透回调 |
| `table.query(app, table, filter)` | `bitable.v1.appTableRecord.search` | 文档表格 API | 智能表 API |
| `doc.create_read(type, title)` | — | 文档 API | tencent-docs MCP |

路由规则:Hub 内部维护 `capability → 平台实现` 注册表,调用方只写语义。

### 4.2 本地事件接收(最大工程难点)

| 平台 | 机制 | 公网需求 |
|---|---|---|
| 飞书 | WebSocket 长连接 | ❌ 不需要 |
| 钉钉 | Stream 长连接 | ❌ 不需要 |
| 企微 | 智能机器人 SDK 长连接 | ❌ 不需要(frp 仅作备选) |

事件管线:`平台事件 → 适配层规范化 → Redis Stream(持久化+消费组)→ Hub 路由规则 → 跨平台转发/触发文档写入`。
幂等:以 `platform + event_id` 为去重键,Redis SETNX + 24h TTL;消费侧 ack 失败重投 3 次后进死信队列。

### 4.3 凭据与安全

- 所有 Token/Secret 存本地 `.env`(不进 git),Hub 启动时加载并注入各 MCP 子进程环境变量
- 审计:每次跨平台写操作记录 `(时间, 语义工具, 源平台, 目标平台, payload 摘要, 结果)` 到本地 SQLite
- 最小权限:飞书只开 `bitable:app` + IM scope;钉钉按功能逐项申请

### 4.4 ID 映射中心

跨平台联动(如「钉钉群收到指令 → 写飞书多维表格」)需要实体映射:

```
mapping(platform, external_id, entity_type) ↔ internal_id
entity_type ∈ { user, group, doc, table }
```

存储:SQLite(本地单节点够用),接口走 GORM,后续可平滑切 MySQL。

---

## 5. 落地路线(4 个 Milestone,每步可验收)

### M1:骨架 —— 单向打通(只发不收)
- [x] Hub 骨架代码完成(`hub/`,编译+冒烟通过):MCP 挂载框架、命名空间工具聚合、统一语义路由、审计落点、事件桥内存实现、ID 映射接口 ✅
- [x] 降级机制验证:单平台挂载失败不影响 Hub 启动与其余平台 ✅
- [x] Hub 挂载四平台成功:feishu 19 工具 / dingtalk 13 工具 / tencent-docs 226 工具 / wecom 3 工具,共 261 个 ✅(2026-10-30 实测)
- [x] 路由表已用真实工具名回填:`im_v1_message_create` / `sendMessageToGroupByRobot` / `wecom_send_text` / `bitable_v1_appTableRecord_search` / `doc.create_with_markdown` ✅
- [x] 腾讯文档创建实测通过:统一语义 `doc.create` → `tencent-docs.doc.create_with_markdown`,真实产出文档 ✅
- [x] 统一 `im.send_text` 实测:企微群 ✅、钉钉单聊+群 ✅、飞书群 ✅(message_id 返回正常)— **三家 IM 发送全部打通**
- **验收**:Hub 日志出现 4 个 MCP Server 的 `tools/list` 成功记录(✅ 已达);三条消息真实到达;腾讯文档可打开且内容正确(✅ 已达)

### M2:事件桥 —— 双向 IM
- [ ] 飞书 WS 长连接收到群消息并落 Redis Stream
- [ ] 钉钉 Stream 模式收到消息
- [ ] 企微 frp 穿透回调收到消息
- [ ] 幂等去重单测通过(同一 event_id 重放只处理一次)
- **验收**:向三个群发消息,Hub 日志均出现规范化事件;重复推送不产生重复记录

### M3:联动场景(业务价值验证,**文档汇总推 IM 优先**)
- [ ] 场景 B(优先):每日定时,汇总飞书 Bitable 新增记录 → 生成腾讯文档日报 → 企微群推送链接
- [ ] 场景 A:钉钉群关键词(如「#登记」)→ 自动写入飞书多维表格指定表
- **验收**:场景 B 端到端跑通后再做 A;两个场景审计表均有完整链路记录

### M4:加固
- [ ] 凭据轮换流程文档化;限流(每平台 QPS 阈值);死信队列告警
- [ ] 回归测试集:每个统一工具 × 每个平台 ≥1 用例
- **验收**:checklist 全绿,故障演练(单平台 token 失效)不影响其他平台

---

## 6. 风险登记

| 风险 | 等级 | 缓解 |
|---|---|---|
| ~~「腾讯 IM」实为微信个人号~~ | ✅ 已关闭 | 已确认为企业微信 |
| 钉钉文档权限审批慢 | 🟡 中 | M1 只用 IM scope,文档权限并行申请 |
| ~~企微接收需穿透,稳定性差~~ | ✅ 已关闭 | 改用智能机器人 SDK 长连接,无需 frp |
| 社区 wecom-bot-mcp 维护断更 | 🟢 低 | webhook 协议简单,可 50 行自研替代 |
| 三平台限流策略不同 | 🟢 低 | Hub 侧统一令牌桶,按平台配置 |

---

## 7. 已确认决策记录

1. 腾讯 IM = **企业微信智能机器人(API 模式,Bot ID + Secret)**,收发一体,长连接收消息;webhook 群机器人降为备选
2. 企微需要接收消息 → 智能机器人长连接解决,**frp 穿透不再需要**
3. 飞书无现成自建应用 → 按 §8 引导创建(约 10 分钟,无需写代码)
4. 联动场景优先级:**文档汇总推 IM 优先**(M3 先做场景 B)

---

## 8. 飞书自建应用操作指引(图形界面,无需写代码)

> 目标产物:一对 `App ID` + `App Secret`,以及已授权的 Bitable / IM 权限。全程在飞书开发者后台点击完成。

### 步骤 1:创建企业自建应用
1. 浏览器打开 [飞书开发者后台](https://open.feishu.cn/app),用飞书账号登录
2. 点击「创建企业自建应用」,填写名称(如「协作中台 Hub」)和图标,提交
3. 进入应用详情页,「凭证与基础信息」页签可复制 **App ID** 和 **App Secret**

### 步骤 2:开通权限(权限管理 → 开通权限)
| 权限 scope | 用途 |
|---|---|
| `bitable:app` | 多维表格读写(场景 B 数据源) |
| `im:message` / `im:message:send_as_bot` | 机器人发消息 |
| `im:chat` | 群组信息读取(M2 收消息) |
| `contact:user.base:readonly`(可选) | ID 映射中心解析用户名 |

### 步骤 3:开启机器人能力 + 事件订阅
1. 「应用能力 → 机器人」:启用机器人(M2/M3 收发消息前提)
2. 「事件订阅」:M2 阶段选择**长连接(WebSocket)模式**,订阅 `im.message.receive_v1` 事件 —— 本地运行无需公网地址

### 步骤 4:发布版本
「版本管理与发布」→ 创建版本 → 提交发布(企业自建应用一般自动通过,部分企业需管理员审批)

### 步骤 5:配置到 Hub
把 App ID/Secret 写入本地 `.env`:
```bash
FEISHU_APP_ID=cli_xxxx
FEISHU_APP_SECRET=xxxx
```
Hub 启动时注入 lark-mcp 子进程,即完成接入。

**验收**:本地执行 `npx -y @larksuiteoapi/lark-mcp mcp -a $FEISHU_APP_ID -s $FEISHU_APP_SECRET -p preset.base.default` 后,客户端能列出 `bitable.v1.appTableRecord.search` 等工具。

> 同理,钉钉侧需在[钉钉开放平台](https://open-dev.dingtalk.com/)创建「企业内部应用」获取 ClientID/ClientSecret 并开启 Stream 模式;企微侧在「管理后台 → 应用管理 → 机器人」拿 webhook key。腾讯文档 Token 在[腾讯文档开放平台](https://docs.qq.com/open/document/mcp/)申请。四组凭据齐了即可跑 M1。

---

## 9. M3 场景 B 日报:运行配置

链路:内置 cron 定时 → 拉取飞书 Bitable 增量记录(水位线持久化在 `hub.db` 的 kv 表,重启不丢)→ 生成/更新腾讯文档日报(同日幂等:`report_docs` 表存 date → file_id 映射)→ 推送链接到目标群 → 全程审计(audits 表 `tool='report.daily'`)。

**config.json 的 `report` 段**(全部可选,缺省即默认值):

| 键 | 默认 | 说明 |
|---|---|---|
| `enabled` | false | 是否启用内置 cron;false 时仍可手动触发 |
| `cron` | 空 | 标准 5 段表达式,如 `0 18 * * *`(每天 18:00) |
| `timezone` | Asia/Shanghai | IANA 时区 |
| `backfill_hours` | 24 | 首次运行(无水位线)的回填窗口 |
| `created_field` | 创建时间 | Bitable 创建时间字段(自动字段 `created_time` 兜底) |
| `push_platforms` | ["wecom"] | 推送平台开关,可加 feishu/dingtalk(需对应 `push_targets`) |

**.env 需要**:BITABLE_APP_TOKEN / BITABLE_TABLE_ID(数据源)、TENCENT_DOCS_TOKEN(建文档)、`WECOM_CHAT_ID` 等推送目标群。

**手动触发 / 补跑**:`./hub -run-report [-report-date YYYY-MM-DD]`,或 MCP 元工具 `hub.report.run`(可选 `date` 参数)。两者都依赖上述 `BITABLE_APP_TOKEN`:该变量为空时 `hub.report.run` 不会注册、`tools/list` 里没有它,CLI 也会直接报错退出。

**行为约定**:无新增记录 → 不生成空日报、不推送,审计留 `skipped`;同日重复触发 → 更新同一篇文档(远程腾讯文档 MCP 无整篇覆写 API,实现为「同 file_id 顶部插入最新快照」,链接不变);429/配额类错误分类为 `limit` 不重试,其余 transient 错误最多重试 2 次。
