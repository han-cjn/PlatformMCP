# 技术方案：多平台接入引导自动化（hub onboard）

> 版本：v1.1（已实现） · 状态：**M1+M2+热重载已交付，待 commit** · 关联：[设计文档 §8](cross-platform-mcp-hub-design.md)、[§9](cross-platform-mcp-hub-design.md)
>
> 实现入口：`go run ./cmd/hub -onboard <platform>`（向导）/ 加 `-onboard-verify-only`（免交互体检）/ 加 `-onboard-browser`（浏览器执行器）。

## 1. 背景与目标

当前接入一个新平台（飞书/钉钉/企微/腾讯文档）的人工成本集中在两端：

1. **平台侧重**：登录开放平台 → 创建应用 → 开权限 → 复制 AppID/Secret →（可能）发布版本
2. **本地侧**：手工编辑 `.env`（易错：变量名抄错、值带空格、忘开权限）→ 重启验证 → 翻日志排查

结论（已确认）：**登录与创建无法全自动**（鸡生蛋：无凭据调不了 API、强交互认证、ToS 限制），但可以做到「人工只做登录 + 点确认，其余全自动化」。

**目标**：
- 一条命令完成「引导 → 收集凭据 → 校验 → 写 .env → 挂载 → 体检」全链路
- 人工模式（稳定主路径）与浏览器自动化模式（可选增强）共用同一套编排与清单，可随时互相切换降级
- 新平台接入耗时从「翻文档半小时」降到「约两分钟」

**非目标**（明确不做）：
- ❌ 自动化登录（扫码/短信/验证码）——合规红线
- ❌ 绕过企业管理员审批
- ❌ 自动申请腾讯文档 Token（它是浏览器登录态本身，只能人工登录后抄取）

## 2. 总体架构

```
┌─────────────────────────────────────────────────────────┐
│  数据层: platform.Portal（4 平台接入清单,纯数据,可单测)      │
│  portal URL / 步骤序列 / 权限 checklist / 凭据变量 / 校验器  │
└───────────────────────────┬─────────────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────┐
│  编排器: hub onboard <platform> [--browser]              │
│  逐步推进、执行器切换、凭据收集、失败降级                     │
└────────┬──────────────────────────────┬────────────────┘
         │                              │
┌────────▼─────────┐          ┌─────────▼──────────┐
│ 手动执行器(方案1)   │          │ 浏览器执行器(方案2)   │
│ 终端提示+人工操作    │◀─ 降级 ──│ 接管已登录会话代填    │
│ 粘贴凭据回终端      │          │ DOM 抽取凭据直写     │
└────────┬─────────┘          └─────────┬──────────┘
         │                              │
┌────────▼──────────────────────────────▼──────────┐
│  共享收尾链(只有一份)                                 │
│  凭据格式校验 → 平台 API 真连校验 → .env 原子写入       │
│  → MCP 挂载 → 路由自检 → 体检表输出                   │
└───────────────────────────────────────────────────┘
```

分层原则：**清单与收尾链单点维护，执行器可插拔**。浏览器执行器任何一步失败，编排器当场把该步骤切回手动提示，流程不断。

## 3. 数据层：平台接入清单（platform.Portal）

```go
type Portal struct {
    Name        string        // feishu / dingtalk / wecom / tencent-docs
    Title       string        // 飞书开放平台
    URL         string        // https://open.feishu.cn/app
    Steps       []Step        // 有序步骤(两种执行器共用)
    Creds       []Cred        // 本平台需要收集的凭据
    Validator   Validator     // 真连校验(见 §6.3)
}

type Step struct {
    ID       string          // "create-app" / "enable-perms" / ...
    Brief    string          // 终端显示的一行说明(手动模式)
    Detail   string          // 展开说明:具体点哪里、开哪些权限
    URLs     []string       // 相关页面(带 ?from=hub 跳转)
    // BrowserHint 浏览器执行器专用(方案2);为空表示该步必须人工
    Browser  *BrowserAction
}

type Cred struct {
    EnvKey     string        // FEISHU_APP_ID
    Label      string        // App ID
    Mask       bool          // Secret 类输入回显 ***
    Validate   func(string) error   // 格式校验(cli_ 前缀、长度等)
}
```

### 3.1 四平台清单要点（数据，按现有踩坑经验固化）

| 平台 | 关键步骤 | 凭据 | 特殊注意（历史踩坑固化） |
|---|---|---|---|
| 飞书 | 创建企业自建应用 → 凭证页拿 ID/Secret → 开权限(im:message、bitable:app、im:chat) → 事件订阅选**长连接** → 创建版本发布 | FEISHU_APP_ID / FEISHU_APP_SECRET | 企业自建一般自动通过，部分企业需管理员审批 |
| 钉钉 | 应用开发 → 企业内部应用 → 凭证与基础信息 → 机器人配置(Stream 模式) → 权限(含 qyapi_addresslist_search) | DINGTALK_Client_ID / DINGTALK_Client_Secret | 变量名**大小写敏感**(官方 MCP 要求)；ROBOT_CODE=AppKey |
| 企微 | 管理后台 → 智能专区 → 智能机器人 → 拿 Bot ID/Secret → 拉群 | WECOM_BOT_ID / WECOM_BOT_SECRET | 无 REST 校验 API，用适配器 WS 建连校验 |
| 腾讯文档 | docs.qq.com/open/auth/mcp.html 人工登录 → 复制 Token | TENCENT_DOCS_TOKEN | Token=登录态,只能人工抄取;限流 2000 次/天 |

> 权限 checklist 中每项标明「权限名 → 对应 Hub 功能」（如 `im:message` → 发消息/回执），避免用户漏开导致挂载后调用 403 再回头返工。

## 4. 方案 1：手动执行器（主路径，M1 交付）

交互式 CLI 向导（`hub onboard feishu`）：

1. 打印平台 checklist（步骤 + 链接），并尝试 `open <URL>` 直接唤起浏览器
2. 用户登录平台、照提示操作（人工完成登录/验证码/审批）
3. 每步完成回车继续；到凭据收集步：
   - 终端输入（Secret 类回显 `*`，不经 shell history——读 /dev/tty）
   - 格式校验即时反馈（如飞书 AppID 必须 `cli_` 前缀）
4. 进入共享收尾链（§6），输出体检表
5. 全程写 audit（tool=`hub.onboard`），失败可断点续跑——已收集的凭据暂存内存，不落盘明文

交互实现：`bufio.Scanner` + `/dev/tty` 直读，**不引入** bubbletea/promptui 等重型 TUI 依赖（保持单二进制 + 零外部依赖的发布形态）。

## 5. 方案 2：浏览器执行器（可选增强，M2 交付）

### 5.1 技术选型

**选 [rod](https://github.com/go-rod/rod)**，理由：
- 纯 Go，无 cgo/无外部二进制，符合单静态二进制发布形态
- `rod.New().ConnectURI("ws://127.0.0.1:9222")` 直连**用户已登录**的 Chrome 实例，这是本方案的核心形态（我们绝不做登录，接管的是现成会话）
- 内置元素等待/查找 API，比 chromedp 写选择器脚本更短

替代方案 chromedp 亦可行（同 CDP 协议），若对 rod 有顾虑可替换，接口层已隔离（`BrowserAction` 是数据结构）。

### 5.2 运行形态

```
# 前置:启动带调试端口的 Chrome(已封装成脚本,推荐)
./scripts/chrome-debug.sh            # 默认端口 9222
./scripts/chrome-debug.sh 9333       # 自定义端口

# 等价的手工命令(脚本内部即此参数)
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --remote-debugging-port=9222 \
  --user-data-dir="$HOME/.chrome-debug-profile" \
  --profile-directory=Default

hub onboard feishu --browser
# 编排器:连接 9222 → 按清单 Steps 逐个执行 BrowserAction → 失败步骤当场降级手动
```

> ⚠️ **`--user-data-dir` 必须显式指定为持久化目录，否则登录态会丢**（详见 §5.3）。
> 首次使用需在脚本打开的窗口里登录一次目标平台，之后登录态长期保留。

`BrowserAction` 数据结构（选择器版本化，UI 变更只改数据）：

```go
type BrowserAction struct {
    Script string        // 版本标记:"feishu@2026-11"
    Ops    []Op          // 序列:navigate / hover / click / waitVisible / waitURL / readText / fill / screenshot
}
// 凭据抽取:readText(选择器) → 直接进内存 → 写 .env
// 不经用户剪贴板,不打印明文,回显只显示前4位+***
```

### 5.3 Chrome 调试端口与登录态（重要踩坑）

**现象**：按 §5.2 旧命令（只带 `--remote-debugging-port=9222`、不指定 `--user-data-dir`）启动 Chrome 后，
发现飞书等平台**登录状态没了**，需要重新扫码登录。

**根因**（Chrome 自身行为，与 Hub 代码无关）：

1. 日常 Chrome 已在运行并占用**默认 profile**（`~/Library/Application Support/Google/Chrome`）
2. 再带 `--remote-debugging-port` 启动时，Chrome 发现默认 profile 被占用，**静默新建一个临时 profile**
   （落在 `/var/folders/...` 或 `.org.chromium.Chromium.*` 下），不复用原 profile
3. rod 连上的是这个空 profile → **看不到任何已登录会话**
4. 临时 profile 在进程退出后被系统回收 → 在其中做的登录**不会被保存**

> 注意：这不是「登录态被清空」，原 profile 的 cookie 完好无损。是两个 profile 互相隔离造成的错觉。
> 更需警惕的是**反过来的风险**：如果 Hub 把调试端口开在默认 profile 上，等于把用户日常浏览器的
> 完整会话暴露给自动化脚本，边界不清。

**修复**：调试端口必须配**独立且持久化**的 profile：

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --remote-debugging-port=9222 \
  --user-data-dir="$HOME/.chrome-debug-profile" \
  --profile-directory=Default
```

- 首次需在该窗口登录一次目标平台，登录态写入 `~/.chrome-debug-profile`，之后长期有效
- 与日常 profile **完全隔离**：日常 Chrome 照常用，自动化脚本只碰这个专用 profile
- 可长期保留这个专用 profile，仅存放待接入平台的登录态

**代码层防护**：`resolveWebSocketURL` 读取 `/json/version` 的 `userDataDir`，
若命中临时目录前缀（`/tmp/`、`/var/folders/`、`/private/tmp/`、`.org.chromium.`），
**直接报错并打印上面的启动命令**，避免代填脚本跑到一半才发现会话是空的。
判定逻辑抽为纯函数 `isEphemeralProfileDir`，单测覆盖。

### 5.4 标签页边界

执行器只操作自己创建的 `about:blank` 页（`initial` 字段）：

- `navigate` 幂等：已有目标 URL 的标签则切换，不把用户当前页面导走
- `waitURL` 会切到用户手动点开的详情页，`Close()` **只关 `initial`、绝不关用户页面**
- 任何情况下都不调用 `rod.Browser.Close()`——它会发 CDP `Browser.close` 直接关掉用户的整个 Chrome

### 5.5 降级矩阵

| 失败场景 | 处置 |
|---|---|
| 连不上 9222（Chrome 未启动） | 提示启动命令，或切手动模式 |
| **Chrome 使用临时 profile** | **直接报错并打印持久化 profile 启动命令**（见 §5.4） |
| 选择器失效（平台改版） | 该步骤打印 Brief/Detail 手动说明，人工完成后回车继续；后续步骤不中断 |
| 抽取的凭据校验失败 | 回退到终端输入重收 |
| `readText` 读到掩码字符 | 重试 10 次×500ms，仍失败则该步降级手动（掩码字符可由 `Op.MaskRune` 覆盖） |
| `waitURL` 60s 内无匹配标签 | 该步降级手动；命中需排除 `/app/list` 等假匹配（`Op.URLExcludes`） |

## 6. 共享收尾链（两种执行器合流）

### 6.1 .env 原子写入（复用/增强 config 包）

- **原子性**：写 `hub.env.tmp` → `os.Rename` 覆盖，中途崩溃不产生半截文件
- **无损**：逐行合并已存在的 `KEY=VALUE`（保留注释与未知变量），只 upsert 本次收集的键
- **权限**：写入后 `chmod 0600`；新建时同样
- **脱敏**：全程 Secret 不落日志、不进 audit（audit 只记「已收集」布尔值）

### 6.2 格式校验（Cred.Validate，纯函数，单测覆盖）

飞书 `cli_` 前缀；钉钉 AppKey 形如 `dingxxxxxx`；企微 Bot ID `ding` 之外的 `wbxxxx`/自定义；腾讯 Token 非空且长度区间。

### 6.3 平台真连校验（Validator，实测凭据有效性后才写入）

| 平台 | 校验方式（全部轻量、一次调用） | 失败输出 |
|---|---|---|
| 飞书 | `POST /open-apis/auth/v3/tenant_access_token/internal` 换 tenant_access_token | 区分「凭据错误」vs「网络/平台故障」 |
| 钉钉 | `POST /v1.0/oauth2/accessToken`(appKey/appSecret) | 同上 |
| 企微 | 拉起 wecom 适配器子进程，观察 WS `authed` 事件（10s 超时） | 建连失败≈凭据错误 |
| 腾讯文档 | 对 `https://docs.qq.com/openapi/mcp` 发 MCP `initialize`（Authorization 头） | 401≈Token 失效 |

校验通过 → 写 `.env` → **以新凭据即时挂载对应 MCP** → 路由自检 → 复用现有 `printHealthTable` 输出体检表（凭据 ✓/挂载 ✓/长连接）。

## 7. 落地计划

| 阶段 | 内容 | 规模预估 | 验收 |
|---|---|---|---|
| **M1**（先做） | Portal 数据层(4 平台清单) + 编排器 + 手动执行器 + 收尾链(原子 .env/校验/挂载/体检) + audit | 约新增 700~900 行 + 测试 | 每平台用真实凭据跑一遍 onboard；错误凭据被校验器拦住 |
| **M2**（确认后） | rod 执行器 + 飞书清单 BrowserAction 试点 → 复制钉钉/企微 | 约 400~600 行 + 每平台选择器脚本 | 浏览器代填飞书全流程；人为改坏一个选择器验证降级路径 |

M1 完成即独立可用；M2 的 `BrowserAction` 为空时自动纯手动，互不阻塞。

## 8. 测试与安全

**单测**（无网络，mock HTTP）：
- checklist 数据完整性（4 平台步骤/凭据/校验器非空）
- `.env` 合并写入：upsert、保留注释、原子性（模拟 rename 失败）、0600 权限
- 各 Validator 对 mock 服务端的通过/失败分类
- 降级矩阵：执行器返回 ErrSelectorMiss → 编排器切手动的路径

**安全边界**：
- Secret 只在内存与 0600 的 `.env` 中存在；终端回显 `****`；audit/日志脱敏
- 浏览器执行器仅连 `127.0.0.1:9222`，不做任何远程连接
- 不自动化登录；不存储任何平台主账号口令

## 9. 需要确认的点

1. **rod 选型**是否接受（替代 chromedp，理由见 §5.1）——只影响 M2，M1 无依赖
2. **M1 范围**：4 平台一次做齐，还是先飞书+钉钉（企微/腾讯文档清单随后补）？
3. 企微校验采用「拉适配器子进程看 authed」方案（无官方 REST 校验 API），确认可接受
4. onboard 期间如 Hub 正在运行：写 `.env` 后是否自动**热重载**对应平台（本期建议：提示用户重启，热重载列为后续增强）
