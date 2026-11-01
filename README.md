# Today2API Panel

把 **Today（todayai.cn 国内版 / today.ai 国际版）** 的账号池，暴露成 **OpenAI / Codex Responses / Anthropic** 三种兼容接口的自托管网关，附内嵌 Web 管理面板。

单二进制、零外部依赖（只用 Go 标准库），面板内嵌在进程里。

```
客户端 ──Bearer sk-today-…──► 网关 ──Cookie→JWT──► api.todayai.cn / api.today.ai
                                │
                                ├─ 账号池（轮转 / 冷却 / 首字节前故障转移）
                                └─ 协议翻译（三种入站 → Today 私有会话协议）
```

## 快速开始

```bash
go build -o today2api ./cmd/server
./today2api -config config.json          # 首次运行会按默认值生成 config.json
# 浏览器打开 http://127.0.0.1:8690/  → 「账号」→ 粘贴 Cookie
```

拿 Cookie：浏览器登录 [todayai.cn](https://todayai.cn) 或 [today.ai](https://today.ai) → F12 → Application → Cookies → 复制 `__Secure-better-auth.session_token` 的值（或整段 Cookie 头）。网关会立刻用它换一次 JWT 验证，失败不会写库。

国际版还支持面板里「邮箱验证码」登录；**国内版已关闭邮箱登录**（上游返回 `auth.email_sign_in_disabled`），只能用 Cookie。

## 客户端接入

创建密钥后（面板「密钥」页，明文只显示一次）：

```bash
# OpenAI / 任意 OpenAI SDK
export OPENAI_BASE_URL=http://127.0.0.1:8690/v1
export OPENAI_API_KEY=sk-today-…

# Claude Code
export ANTHROPIC_BASE_URL=http://127.0.0.1:8690
export ANTHROPIC_API_KEY=sk-today-…

# Codex CLI：base_url 指到 http://127.0.0.1:8690/v1（走 /v1/responses）
```

| 路径 | 协议 | 流式 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | ✅ |
| `POST /v1/responses` | OpenAI Responses（Codex） | ✅ |
| `POST /v1/messages`、`/messages` | Anthropic Messages（Claude Code） | ✅ |
| `GET /v1/models` | 模型清单（静态 + 账号套餐探测） | — |
| `GET /healthz` | 存活探针 | — |

没配密钥时不校验鉴权（仅建议本机/内网用）；配了密钥后 `Authorization: Bearer …` 与 `x-api-key` 都认。

## 上游协议（为什么网关要翻译）

> 以下全部以 **真实客户端（Today Desktop 1.24.0）静态逆向 + 上游实测** 为准。
> 早期社区实现里的 `channel:"main"`、`202 + threadId`、`isMainChat=true` 轮询参数
> 都与客户端不一致，已按客户端改写，详见 [`docs/reverse-engineering.md`](docs/reverse-engineering.md)。

Today 没有 `chat/completions`，链路是三步：

1. **换票** — `POST {app}/api/token`，带 Cookie 与 `Origin`，body `{audience, expiresIn:3600}`，
   返回 `{token, expiresAt}`（小时级 JWT）。桌面端同时会发 `X-App-Version` / `X-Client-Platform`
   / `X-Device-Id` / `X-Timezone`，上游按这些头做版本与平台分支——缺了会掉进 legacy 分支。
2. **发消息** — `POST /v1/messages`（canonical-v1）：

   ```json
   {
     "body": { "parts": [ { "id": "<uuid>", "type": "text", "text": "…" } ] },
     "clientMessageId": "<uuid>",
     "isMainChat": true
   }
   ```

   立刻返回 `{canonicalMessage:{…}}`——**这是用户自己那条消息**，不是回答。
   指定会话时用 `isMainChat:false` + `conversationId`。
3. **取回答** — `GET /v1/conversations/messages?conversationId=…&limit=N`，
   在历史页里按 `originClientMessageId == 本轮 clientMessageId` 找到对应的 assistant 消息；
   正文逐次变长，`isFinal` 或 `state=completed` 即终稿。

所以「流式」是网关把轮询增量伪流式下发：首字 3~10 秒，之后按增量吐。
轮询间隔默认 250ms，可在面板调。

**模型选项是账号级的**：`PUT /v1/pricing/model-option {optionId}` 改的是这个账号的全局选择，
不是每条消息带的参数。所以并发切模型会互相踩，网关内部把它串行化并按账号缓存当前选项。

## 签到与做任务

Today 没有独立的 `daily-checkin` 端点。每日互动由 `/v1/rapport` 的规则卡片承载：

```bash
GET  /api/panel/accounts/{id}/rapport   # 积分 / 等级 / 规则卡片快照
POST /api/panel/accounts/{id}/checkin   # 读快照 → 上报动作 → 复读，返回积分 delta
POST /api/panel/checkin                 # 全账号批量签到（并发 4）
```

客户端有权主动申报的动作只有两条（上游注释写明「规则与分值由服务端持有，
客户端不能提供 rule key 或点数」）：

- `POST /v1/rapport/actions/notification-enabled`
- `POST /v1/rapport/actions/discord-joined`

两条都是幂等的，重复跑不会重复计分，所以后台定时签到是安全的：

```json
"checkin": { "enabled": true, "interval_minutes": 720 }
```

做任务走 `/v1/tasks` 系列：

| 面板接口 | 上游 |
|---|---|
| `GET /api/panel/accounts/{id}/tasks` | `/v1/tasks` + `/v1/tasks/summary` + `/v1/tasks/templates/types` + `/v1/tasks/recommendations` |
| `GET /api/panel/tasks` | 跨账号聚合 `/v1/tasks` |
| `POST …/tasks/{taskId}/stop` | `POST /v1/tasks/{id}/stop` |
| `POST …/tasks/{taskId}/follow-up` | `POST /v1/tasks/{id}/follow-up` |
| `POST …/tasks/recommendations/{recId}/apply` | `POST /v1/tasks/recommendations/{id}/apply` |

## 账号池行为

- 轮转选号：跳过停用、冷却中、本轮已失败的账号；
- 冷却：Cookie/JWT 失效 10 分钟、429 10 分钟、5xx 60 秒、网络错误 30 秒；
- **首字节前故障转移**：选号→切模型→发送→等首个增量之间出错会自动换下一个账号，下游只会看到一次正常响应；一旦已经吐过正文就不再换号（否则下游会拼出两段回答）。

## 配置

`config.json` 全部字段都可以在面板改（除 `listen` / `upstream`，改完重启生效）。

| 键 | 说明 |
|---|---|
| `listen` | 监听地址，默认 `0.0.0.0:8690` |
| `panel_password` | 面板密码；留空 = 只有本机（回环）能管理 |
| `default_model` | 客户端不传 model 时用哪个 |
| `reply_timeout_sec` | 单轮等回答上限，默认 240 |
| `poll_interval_ms` | 轮询间隔，默认 250 |
| `context_turns` | 0 = 只发当前消息（Today 自己按会话记上下文）；N = 把最近 N 轮历史拼进正文 |
| `checkin.enabled` | 是否后台自动签到，默认关闭 |
| `checkin.interval_minutes` | 自动签到间隔，开启且为 0 时按 720（12 小时） |
| `upstream.{cn,global}` | 三件套地址；留空用官方值，可指向自建中转 |
| `keys[]` | 下游密钥（只存 sha256） |
| `accounts[]` | 账号池（Cookie 明文，文件权限 0600） |

## 关于本项目

结构对照 [workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)：单二进制 + `cmd/` 入口 + `internal/{config,today,pool,server,panel}` 分层 + 内嵌单页面板 + 配置即状态。

协议事实源是本机安装的两个真实客户端（`C:\Program Files\Today` 国际版、`C:\Program Files\Today CN` 国内版），
逆向过程、证据与「与仓库冲突时以客户端为准」的裁定记录见 [`docs/reverse-engineering.md`](docs/reverse-engineering.md)。

历史参考（早期社区实现，部分字段与客户端不符，仅作背景）：

- [shuaichao171/TodayProxy](https://github.com/shuaichao171/TodayProxy)
- [MYCF711/magpie-today](https://github.com/MYCF711/magpie-today)
- [jzPPP3333/cpa-today-plugin](https://github.com/jzPPP3333/cpa-today-plugin)

> ⚠️ 非官方网关：上游随时可能收紧，账号风控由使用者自负；请只用自己的账号，别拿去做对外中转。

## 测试

```bash
go test ./...   # 含一个 mock 上游的端到端用例，覆盖三种入站协议
```

端到端用例自带 Today 上游替身（换票 / 发消息 / 轮询三段），因此不需要真账号。
