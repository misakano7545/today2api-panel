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

Today 没有 `chat/completions`：

1. `POST /api/token`（带 Cookie）换一个分钟级过期的 API JWT；
2. `POST /v1/messages` 只回 **202**（用户消息已入库），拿不到回答；
3. 回答要靠 `GET /v1/conversations/messages?isMainChat=true` 轮询取回，正文逐次变长，`state=completed` 即终稿。

所以「流式」是网关把轮询增量伪流式下发：首字 3~10 秒，正文按增量吐。轮询间隔默认 250ms，可在面板调。`channel: "main"` 是必填的，缺了这条消息不进主对话、后续永远查不到。

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
| `upstream.{cn,global}` | 三件套地址；留空用官方值，可指向自建中转 |
| `keys[]` | 下游密钥（只存 sha256） |
| `accounts[]` | 账号池（Cookie 明文，文件权限 0600） |

## 关于本项目

结构对照 [workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)：单二进制 + `cmd/` 入口 + `internal/{config,today,pool,server,panel}` 分层 + 内嵌单页面板 + 配置即状态。

上游协议细节参考（均未直接拷贝代码）：

- [shuaichao171/TodayProxy](https://github.com/shuaichao171/TodayProxy) — 两级鉴权、canonical 会话、`/v2/messages`
- [MYCF711/magpie-today](https://github.com/MYCF711/magpie-today) — 载荷上限、登录态、会话语义
- [jzPPP3333/cpa-today-plugin](https://github.com/jzPPP3333/cpa-today-plugin) — Go 实现参考（`channel:"main"`、轮询取回、模型选项）

> ⚠️ 非官方网关：上游随时可能收紧，账号风控由使用者自负；请只用自己的账号，别拿去做对外中转。

## 测试

```bash
go test ./...   # 含一个 mock 上游的端到端用例，覆盖三种入站协议
```

端到端用例自带 Today 上游替身（换票 / 发消息 / 轮询三段），因此不需要真账号。
