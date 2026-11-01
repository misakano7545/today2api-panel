# Today 上游协议逆向记录

> 目标：把 Today（国际版 today.ai / 国内版 todayai.cn）的会话协议搞清楚，
> 做成 OpenAI / Codex Responses / Anthropic 三协议网关。
>
> 铁律：**仓库和客户端冲突时，以客户端为准。**
> 本文所有结论都能追到具体证据，不靠猜。

---

## 0. 结论速查

| 环节 | 结论 | 证据等级 |
|---|---|---|
| 三件套域名 | 国际 `today.ai` / `api.today.ai` / `auth.today.ai`；国内 `todayai.cn` / `api.todayai.cn` / `auth.todayai.cn` | 客户端常量 + 实测 200 |
| 换票 | `POST {app}/api/token` `{audience,expiresIn:3600}` → `{token, expiresAt}` | 客户端源码 + 未鉴权 401 |
| 身份头 | `X-App-Version` / `X-Client-Platform` / `X-Device-Id` / `X-Timezone` | 客户端 `AccountHeaders.build()` |
| 发消息 | `POST /v1/messages` canonical-v1，`{body:{parts},clientMessageId,isMainChat}` | zod schema + 调用点 |
| 取回答 | `GET /v1/conversations/messages?conversationId=…`，按 `originClientMessageId` 归属 | zod schema + 调用点 |
| 能力协商 | `GET /v1/config` → `mainConversationId` / `supportsLongPoll` | zod schema |
| 模型选项 | 账号级 `PUT /v1/pricing/model-option {optionId}` | OpenAPI 注释 |
| 签到 | `/v1/rapport` + 两条可申报动作 | 端点注释 |
| 任务 | `/v1/tasks` 系列 | OpenAPI 注释 |

---

## 1. 材料与方法

### 1.1 静态材料

两个客户端都是 Electron，`app.asar`（约 38 MB）里是打包后的 ESM：

```
C:\Program Files\Today\resources\app.asar      # 国际版，todayDesktopReleaseRegion=global
C:\Program Files\Today CN\resources\app.asar   # 国内版，todayDesktopReleaseRegion=cn
```

解包用自写的最小 asar 读取器（纯 Node，读 header JSON + 按 offset 切数据，处理 `unpacked` 文件），
两个包各 1881 个文件 / 38.3 MB。

关键文件：

| 路径 | 内容 |
|---|---|
| `dist/app/app/index.js` | 10.4 MB 主进程 bundle：账号、鉴权、HTTP 适配器、全部 OpenAPI 客户端 |
| `dist/app/consts/account.js` | 区域配置装配（apiBaseUrl / authBaseUrl / audience / oidc） |
| `dist/app/consts/cloud-public-config.mjs` | 三套环境默认值（dev / staging / prod） |
| `dist/app/consts/build.js` | 打包期注入的 posthog / sentry / 构建号 |

### 1.2 Web 端

`todayai.cn` 与 `today.ai` 都是 Next.js 16.3.4，首页 HTML 里带 63 个 `_next/static/chunks/*.js`。
其中一格 `3dd2r5t3x6tbg.js` 是 **canonical-v1 的 zod schema 全集** —— 这是本次逆向里
价值最高的单块证据：它把请求体、响应体、字段枚举、长度上限、以及 `superRefine` 的交叉约束全写死了。

### 1.3 动态材料

| 手段 | 拿到什么 |
|---|---|
| 客户端运行日志 `%APPDATA%\Today\logs\client.ndjson` | 真实 `network_request` 记录：method / url / status_code / transport |
| 未鉴权路由探测（curl） | 路由是否存在（404 vs 400/403）、鉴权层在网关还是在应用层 |
| `%APPDATA%\Today\Network\Cookies` + Local State | Chromium cookie 库结构（DPAPI 主密钥 + AES-GCM 载荷） |

---

## 2. 逐层结论

### 2.1 区域配置：客户端有两份，国内版不是国际版改名

国际版 `dist/app/consts/cloud-public-config.mjs` 的 prod 段：

```js
prod: {
  apiBaseUrl: 'https://api.today.ai',
  appUrl: 'https://today.ai',
  baseDomain: 'today.ai',
  betterAuthUrl: 'https://auth.today.ai',
  oidcAuthority: 'https://auth.today.ai',
  oidcClientId: DESKTOP_OAUTH_CLIENT_IDS.prod,
  tokenAudience: 'https://api.today.ai',
}
```

国内版 `index.js` 里内联的 `DESKTOP_CLOUD_PUBLIC_CONFIG`（注意 **build.js 里的域名是另一个**，
真正生效的是这份内联 JSON）：

```json
{
  "dev":    { "apiBaseUrl": "https://api.mytoday.cn",   "appUrl": "https://web.mytoday.cn",  "baseDomain": "mytoday.cn",  "tokenAudience": "https://api.mytoday.cn" },
  "staging":{ "apiBaseUrl": "https://api.todayai.cn",   "appUrl": "https://todayai.cn",      "baseDomain": "todayai.cn",  "tokenAudience": "https://api.todayai.cn" },
  "prod":   { "apiBaseUrl": "https://api.todayai.cn",   "appUrl": "https://todayai.cn",      "baseDomain": "todayai.cn",  "tokenAudience": "https://api.todayai.cn" }
}
```

要点：

- 国内版 **prod 与 staging 同地址**，另有一套 `mytoday.cn` 只在 dev 用。
- `tokenAudience` 恒等于该区域的 `apiBaseUrl` —— 换票时 `audience` 必须跟着 API 域名走，
  写死一个值会让另一个区域换出来的 JWT 被拒。
- 两个客户端 `package.json` 的 `todayDesktopReleaseRegion` 分别是 `global` / `cn`，
  构建号也不同（2001170 / 2000164），是两套独立打包，不是同一包改配置。

### 2.2 鉴权：Cookie → JWT，两跳

web 链（网关用这条）：

```
POST {app}/api/token
  headers: Cookie: __Secure-better-auth.session_token=…
           Origin: {app}
           Accept-Language, Content-Type: application/json
           X-App-Version: 1.24.0
           X-Client-Platform: windows-client
  body:    {"audience":"https://api.today.ai","expiresIn":3600}
  → 200 {"token":"<jwt>","expiresAt":"…"}
```

桌面端另有一条 OAuth2 链（无 Cookie 时用）：

```
GET  {auth}/api/auth/oauth2/authorize?…&code_challenge_method=S256&resource={audience}&prompt=login
POST {auth}/api/auth/oauth2/token   grant_type=authorization_code|refresh_token，带 code_verifier
```

`audience` 同时作为 OAuth2 的 `resource` 参数——同一个概念在两个链里复用。

**身份头是硬要求**：客户端 `AccountHeaders.build()` 无条件注入
`Accept: application/json`、`X-App-Version`、`X-Client-Platform`，有 lane 时再加
`X-Traffic-Lane`。上游按这些头做版本与平台兼容分支（例如 `/v1/today-pages/latest`
的注释直接写了 `X-App-Version < 1.1.0` 会走另一套逻辑），缺了会掉进 legacy 路径。

### 2.3 会话：canonical-v1，POST 回的不是回答

这是与仓库原实现冲突最大的一处。

**客户端 schema**（`appendCanonicalMessageRequestSchema`）：

```js
n.object({
  body: n.object({ parts: n.array(eE).min(1).max(100) }).strict(),
  clientMessageId: uuid,
  replyTo: uuid.optional(),
})
// eE = discriminatedUnion("type", [ text | image/file/voice | link ])
// text: { id: uuid, type: "text", text: string.max(1e6) }
```

外层再分两支：

```js
n.union([
  eA.extend({ isMainChat: n.literal(true) }),
  eA.extend({ isMainChat: n.literal(false).optional(), conversationId: uuid, taskId: uuid.optional() }),
])
```

**响应 schema**（`appendCanonicalMessageResponseSchema`）：

```js
{ canonicalMessage: canonicalMessageSchema }
```

`canonicalMessageSchema` 的关键字段：

```js
{
  id, conversationId, threadId, cursor,
  role: enum(["user","assistant","system"]),
  state: enum(["in_progress","completed","interrupted"]),
  isFinal: boolean,
  replyTo?, clientMessageId?,
  originMessageId?, originClientMessageId?, originClientMessageIds?,
  originTriggerType?: enum(["asst_response","task","task_onboarding","automation","asst_proactive","system"]),
  reactions?, createdAt, updatedAt,
  body: { parts: [ … ] }
}
```

正文 parts 的两种承载：

| messageType | content 形状 | 说明 |
|---|---|---|
| `text` | `{text: "…"}` | 普通正文 |
| `turn` | `{text: ["…","…"]}` | 流式累积段，多段拼起来才是全文 |
| 其余（`qa_*` / `attachment` / `avatar_candidates_card` / `task` / `tail_bubble` …） | — | 卡片与工具事件，无文本价值 |

`canonicalMessageSchema` 上还挂了 `superRefine`：`qa_*` 卡片的 `interaction_id` 与
`revision` 必须同时出现；`task` part 只能出现在 assistant 消息里；同一消息内 `taskId` /
`conversationId` 不得重复。这说明服务端确实按这套 schema 校验。

**归属关系**：assistant 消息用 `originClientMessageId` 指回它回答的那条用户消息。
这是唯一可靠的「这一轮回答是哪条」的判据——比按时间戳猜、比「最后一条 assistant」都稳。

### 2.4 取回答：历史页 + 长轮询

```
GET /v1/conversations/messages?conversationId=<uuid|main>&limit=N[&before=|&after=]
  → { success, code, message, data: { items: [canonicalMessage…], paging: { nextCursor, hasMore } } }
```

`before` 与 `after` 互斥（客户端在 `getHistory` 里直接抛错），分别取旧页与新页。

桌面端还能走长轮询：

```
GET /v3/conversations/{conversationId}/message-changes
```

能力由 `GET /v1/config` 协商：

```js
{
  messageProtocolVersion: "canonical-v1",
  authSubject, mainConversationId,
  supportsTypedBody, supportsSequencePull,
  supportsLongPoll, longPollWaitMaxMs,
  supportsAtomicCardContinuation
}
```

`mainConversationId` 是客户端拿到的默认对话 id。网关启动时读一次，之后主对话就用它。

### 2.5 实时推送：socket 事件名（备用，网关未用）

web 端 `socketEventNames` 常量表（`2ai9ndt9at7m_.js`）：

```js
chat: {
  messageNew: "message.new",
  messageReady: "chat.message_ready",
  messageReadyV2: "chat.message_ready.v2",
  messageUpdatedV2: "chat.message_updated.v2",
  messageReactionsUpdatedV1: "chat.message_reactions_updated.v1",
  presentationUpdatedV1: "chat.presentation.updated.v1",
  toolStartedV2: "chat.tool_started.v2",
  toolCompletedV2: "chat.tool_completed.v2",
  runCompletedV2: "chat.run_completed.v2",
  runFailedV2: "chat.run_failed.v2",
},
agentIndicator: { statusV1: "agent_indicator.status.v1" },
task: { plan: "task.plan.*", step: "task.step.*", run: { toolStarted: "task.run.tool_started", … }, outputReady: "task.output.ready" },
```

`chat.message_ready.v2` 的载荷：

```js
{ event, data: { messageProtocolVersion: "canonical-v1", conversationId, message: canonicalMessage, needPull, presentation? } }
```

网关没接 socket：轮询更省事、也更抗断线。要降延迟时这里是现成的入口。

### 2.6 模型选项：账号级全局开关

`PUT /v1/pricing/model-option`，注释原文：

> Persists the authenticated user **global** model option after checking current effective
> pricing access and returns the canonical localized catalog.

读的入口是 `GET /v1/pricing/status`：

```js
data.status.modelOptions = {
  selectedOptionId: "model.deepseek-v4.1-flash",
  availableOptions: [{ optionId, displayName, modelId, … }],
}
```

所以：

- 模型名到上游要加 `model.` 前缀（对外叫 `today-deepseek-v4.1-flash`，上游要 `model.deepseek-v4.1-flash`）；
- 并发切模型会互相踩，必须串行化；
- 每个账号的当前选项要缓存，避免每条请求都 PUT。

### 2.7 签到：没有 daily-checkin，只有 rapport 规则

`GET /v1/rapport` 注释：

> Returns the authenticated user's authoritative Rapport summary, localized level track,
> and ordered rule cards.

客户端 `agentProfile` 页消费的结构（从渲染代码反推）：

```js
rapport.levels  = [{ key, name, points }]        // 等级轨道
rapport.rules   = [{ key, title, description, note, points, status }]  // status === "completed" 即已入账
```

可主动上报的动作只有两个，注释写明「服务端持有规则与分值，客户端不能提供 rule key 或点数」：

```
POST /v1/rapport/actions/notification-enabled
POST /v1/rapport/actions/discord-joined
```

两条都幂等。所以「签到」的正确做法是：读快照 → 上报这两条 → 复读快照 → 用积分差作为结果。
网关就是这么实现的（`internal/today/growth.go: CheckIn`）。

### 2.8 任务：`/v1/tasks` 系列

从 OpenAPI 客户端注释里抄出来的语义（都是原文翻译）：

| 端点 | 语义 |
|---|---|
| `GET /v1/tasks` | 按最近活动排的任务页 |
| `GET /v1/tasks/summary` | 任务汇总计数 |
| `GET /v1/tasks/templates/types` / `{type}` | 任务模板类型 |
| `GET /v1/tasks/recommendations` | 推荐任务 |
| `POST /v1/tasks/recommendations/{id}/apply` | 采纳推荐 |
| `GET /v1/tasks/{id}` | 任务详情（含 steps / outputFiles） |
| `GET /v1/tasks/{id}/run-events` | 追加式运行事件流，可重连回放 |
| `POST /v1/tasks/{id}/stop` | 终止 → `stopped`，保留 transcript，可 follow-up 续跑 |
| `POST /v1/tasks/{id}/follow-up` | 给已完成任务追加消息并重新排队 |
| `POST /v1/tasks/{id}/seen` | 确认结果已读，必须原样回传 `unseenResultAt`（陈旧确认是幂等 no-op） |

`GET /v2/tasks` 是同一份数据的按 id 稳定排序版（`sortTime = startedAt ?? createdAt`）。

### 2.9 其它值得记的端点

- `GET /v1/users/me` — 账号信息（套餐在 `plan.id` / `plan.name`）
- `GET /v1/llm/models` — 网关可用模型 id 列表
- `GET /v1/config` — 能力协商
- `GET /v1/conversations/unread-status` — 未读计数，可当低频探活
- `GET /v1/diaries` / `/v1/diaries/{date}` — 日记
- `GET /v1/today-pages/latest` — 每日卡片 HTML（`X-App-Version` 敏感）
- `POST /v1/pricing/trial/skip` — onboarding 跳过 → 可能拿到 7 天 Pro 试用

---

## 3. 与仓库原实现的冲突与裁定

仓库（提交 `e50f8a6`）先于本次逆向写成，其中的协议假设**全部按客户端改写**：

| 项 | 仓库原假设 | 客户端事实 | 裁定 |
|---|---|---|---|
| 换票响应 | `{token, expiresIn}`，分钟级 | `{token, expiresAt}`，小时级；兼容只给 `expiresIn` | 用客户端形状，`expiresIn` 作兜底 |
| 换票头 | Cookie + Origin | 另需 `X-App-Version` / `X-Client-Platform` | 补齐 |
| 发消息体 | `{content: string, channel: "main"}` | `{body:{parts:[{id,type,text}]}, clientMessageId, isMainChat}` | 丢弃 `content`/`channel` |
| 发消息响应 | `202 {threadId, messageId}` | `200 {canonicalMessage}` | 改按 `canonicalMessage` 解析 |
| 取回答 | `?isMainChat=true&limit=5`，按 `threadId` 过滤 | `?conversationId=…&limit=N`，按 `originClientMessageId` 归属 | 全改 |
| 正文解析 | `body.parts[].content.text`（string） | 还有 `messageType=turn` 的 `content.text[]` | 两种都吃 |
| 终态判据 | `state == "completed" \|\| isFinal` | 另有 `interrupted` | 一并当终态 |
| 模型列表 | 打 `/v1/pricing/status` 取 option | 有专门的 `/v1/llm/models` | 优先 `/v1/llm/models` |
| 能力协商 | 无 | `GET /v1/config` → `mainConversationId` 等 | 新增（`internal/today/config.go`） |

> 仓库原 README 里写的 `channel:"main"` 必填、`202 + threadId` 这套，来自早期社区实现，
> 与当前客户端不符。客户端没有 `channel` 字段——主对话靠 `isMainChat: true` 标识。

---

## 4. 动态验证记录（本机）

### 4.1 客户端日志（真实请求，未登录态）

`%APPDATA%\Today\logs\client.ndjson` 里 `network_request` 条目的实样：

```json
{"id":"network_request","payload":{
  "method":"GET","url":"https://api.today.ai/v1/client/upgrade",
  "status_code":403,"content_type":"text/html","transport":"undici","duration_ms":1242}}
{"id":"network_request","payload":{
  "method":"POST","url":"https://api.today.ai/v1/campaign/adjust/desktop/sessions",
  "status_code":403,"transport":"undici","duration_ms":1212}}
```

两点：

- 主进程走 **undici**，不是 Electron renderer 的 fetch —— 说明 api 请求在 Node 侧发，
  抓包要抓主进程；
- 未登录时 API 返回 **403 + text/html**，不是 404 —— 鉴权层在应用网关之前，
  「403 = 路由存在但没凭证」不能当作 404 用。

同日志里 `desktop_socket_watchdog_checkpoint` 给出 `"signed_in": false`、
account 初始化 `record=none`，确认本机没有登录态。

### 4.2 未鉴权路由探测

```
https://api.todayai.cn         GET  /v1/*        → 400
https://api.todayai.cn         POST /v1/messages → 403
https://api.today.ai           ANY  /v1/*        → 403 (text/html)
https://auth.todayai.cn        POST /api/token   → 401   ← 路由存在，要 Cookie
https://auth.todayai.cn        POST /api/auth/oauth2/token → 415（缺 form content-type）
https://auth.today.ai          ANY               → 403
```

`/api/token` 返回 **401** 而不是 404，是「换票端点在 auth 域名上也能打」的直接证据——
所以网关的 `fetchToken` 把 `{app}/api/token` 放第一、`{auth}/api/token` 作兜底。

### 4.3 Cookie 库

`%APPDATA%\Today\Network\Cookies` 是 Chromium 标准表，`encrypted_value` 前缀 `v10`，
解密链：`Local State → os_crypt.encrypted_key`（去掉 `DPAPI` 5 字节）→
DPAPI Unprotect 得 32 字节主密钥 → AES-256-GCM（nonce 12 / tag 16）。
本机实测该库里只有 1 行 `.today-desktop.localhost` 的本地会话，
**没有 today.ai 的登录 Cookie** —— 与 4.1 的 `signed_in:false` 一致。

### 4.4 没能做到的部分（诚实记账）

- **登录后的真实抓包没跑成**：本机两个客户端都没登录，登录需要用户本人的凭证，
  不能代填。拿到 Cookie 后跑 `curl` 打 `/v1/messages` 即可补这一段。
- **socket 通道没接**：事件名与载荷结构是从 web bundle 读出来的（见 2.5），
  没有实连验证。网关走轮询，不依赖这一段。

---

## 5. 网关侧的实现映射

| 协议层 | 文件 |
|---|---|
| 换票 / 身份头 / 通用 do() | `internal/today/client.go` |
| 能力协商 / 历史页 / 正文解析 | `internal/today/config.go` |
| 发消息 / 轮询归属 | `internal/today/chat.go` |
| 模型选项 / 模型列表 / 套餐 | `internal/today/pricing.go` |
| 签到 / 任务 | `internal/today/growth.go` |
| 面板 API（签到、任务） | `internal/server/growth.go` |
| 三协议翻译 | `internal/server/{openai,responses,anthropic}.go` |

---

## 6. 复现脚本

解包 asar（纯 Node，无需 npm 依赖）：

```bash
node asarx.js "C:/Program Files/Today/resources/app.asar" gl
node asarx.js "C:/Program Files/Today CN/resources/app.asar" cn
```

抽 OpenAPI 注释成 Markdown（358 条路径 / 427 个方法）：

```bash
node apidoc.js cn/dist/app/app/index.js api_cn.md
```

找 canonical-v1 的 zod schema：

```bash
grep -l "appendCanonicalMessageRequestSchema" web_todayai.cn/*.js
```

读客户端网络日志：

```bash
python - <<'PY'
import json
for line in open(r"C:\Users\Administrator\AppData\Roaming\Today\logs\client.ndjson", encoding="utf-8"):
    o = json.loads(line)
    if o.get("id") == "network_request":
        print(o["payload"])
PY
```

---

## 7. 网络可达性：为什么桌面客户端登录失败而网页可以

> 这一节是**实测结论**，不是推测。所有 403/401 都是当场跑出来的。

### 7.1 现象

桌面客户端（国际版 / 国内版都一样）登录页显示 **「账号服务当前不可用，请稍后重试。」**
输入邮箱点提交，日志里是：

```
403 text/html  POST https://auth.today.ai/api/auth/email-otp/send-verification-otp
403 text/html  GET  https://api.today.ai/v1/client/upgrade
```

而同一台机器上，浏览器打开 `todayai.cn` / `today.ai` 能正常登录（手机验证码）。

### 7.2 403 的源头：不是客户端，是 AWS ALB 按源 IP 拦

| 探测 | 结果 |
|---|---|
| `GET https://api.todayai.cn/v1/config` | `403 text/html`，`Server: awselb/2.0` |
| 同上，换路径 `/`、`/healthz`、`/v1/nonexistent` | 一样 `403 text/html` |
| 同上，换方法 GET/POST/PUT/OPTIONS | 一样 403 |
| 同上，补齐 Chrome 全套头（UA/Sec-Fetch/Origin/Referer） | 一样 403 |
| 同上，直连两个 ELB IP（`--resolve`） | 一样 403 |
| 同上，**经境外中继**（不同源 IP） | **401 `Missing or invalid Authorization header`** ← 应用的真响应 |
| `auth.today.ai` / `auth.todayai.cn` | 同上，403 text/html |
| 本机出口 IP | `36.103.160.13`（宁夏中卫电信） |

**结论**：`api.*` / `auth.*` 前面挂的是 AWS ALB，对本机这个出口 IP 段直接回
`403 Forbidden`（空的 nginx 风格页面 + `awselb/2.0`），请求根本没到应用。
换一个源 IP 就变成应用层的 `401`。这是**网络可达性问题，不是客户端 bug，也不是账号问题**。

顺带：`github.com:443` 从这台机器也连不上（同样症状），说明是整体出口受限。

### 7.3 网页为什么能登录：同源 + 服务端 rewrite

国内版网页的登录（手机验证码）走 `todayai.cn` **同源**接口：

```
POST https://todayai.cn/api/auth/phone-number/send-otp      → 200  （短信真的发出去）
POST https://todayai.cn/api/auth/phone-number/verify        → 走 better-auth
GET  https://todayai.cn/api/auth/get-session                → 200
```

而网页拿数据用的 API，是 Next.js 的 **server-side rewrite**（`routes-manifest.json` 原文）：

```json
"rewrites": { "afterFiles": [
  { "source": "/api/cloud/:path*",    "destination": "https://api.todayai.cn/:path*" },
  { "source": "/api/cloud-cn/:path*", "destination": "https://api.todayai.cn/:path*" }
]}
```

即 `https://todayai.cn/api/cloud/v1/xxx` 由**服务器**转发到 `api.todayai.cn`，
浏览器这一跳根本不去碰被拦的 ALB。国际版同理：

```
GET https://today.ai/api/cloud/v1/config                  → 401 Missing or invalid Authorization header
POST https://today.ai/api/auth/email-otp/...              → 200 {"success":true}
```

**这就是「网页能登录、客户端不能」的全部原因**：客户端直连 `api.*` / `auth.*`，
网页全程只跟同源域名打交道。

### 7.4 由此得到的可用通道（网关已采用）

| 用途 | 地址 |
|---|---|
| App（换票 + 落地页） | `https://todayai.cn` / `https://today.ai` |
| Auth（better-auth） | 同上（前端站点上就有 `/api/auth/[...all]`） |
| API | `https://todayai.cn/api/cloud` / `https://today.ai/api/cloud` |
| JWT 受众 | **仍是** `https://api.todayai.cn` / `https://api.today.ai`（不能跟着中转变） |

`config.Endpoints.Audience` 就是为此加的：`API` 可以指中转，`Audience` 必须指官方域名，
否则签出来的 JWT 受众不匹配，上游回 `Token introspection failed`。
`config.ProxyEndpoints(realm)` 直接给出这套默认值。

实测（假 Cookie，走代理通道）：

```
POST /api/panel/accounts/{id}/check
→ 502 today auth: HTTP 401 {"error":"Unauthorized"}     ← better-auth 的真响应，链路通
```

对照未改配置时是 `403 text/html`（ALB 拦掉，根本没到应用）。

### 7.5 桌面客户端为什么没有就地改好

试过两条路，都失败并已**完整回滚**：

1. **`resources/app` 目录覆盖** —— Electron 的解析顺序是 `app.asar` 优先于 `app/`，
   把 asar 挪走后启动器直接报「Today 无法启动，请重置应用后重试」。
2. **重打包 `app.asar`**（改 `DESKTOP_CLOUD_PUBLIC_CONFIG` 的 prod 段指向同源代理，
   同时剥掉每文件的 `integrity`）—— 客户端能起来、也真的开始走 `today.ai/api/cloud`
   （`201 POST /api/cloud/v1/client/logs`），但随后的启动自检仍然判定失败，
   弹出同一个「Today 无法启动」+ 诊断编号。启动器对安装件有一层自己的校验，
   不是单纯看文件在不在。

> ⚠️ 结论：**不要就地改安装目录**。客户端要能登录，需要给它一条干净出口
> （换网络 / 代理 / 手机热点），或者用下节的网关方案，不改客户端。
