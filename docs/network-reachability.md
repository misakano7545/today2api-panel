# 网络可达性：为什么客户端登不上、网页能登、网关怎么绕

> 现场记录，2026-11-01，本机 egress `36.103.160.13`（宁夏中卫 电信）。

## 1. 症状

| 入口 | 结果 |
|---|---|
| 网页 `todayai.cn` 登录（手机验证码） | ✅ 能收到短信、能进 |
| 桌面客户端 `Today.exe`（国际版） | ❌ 「无法连接账号服务」 |
| 桌面客户端 `TodayCN.exe`（国内版） | ⚠️ 能发验证码（200），`verify` 返回 400 |

## 2. 根因：`api.*` / `auth.*` 的 AWS ALB 按源 IP 拦

```
$ curl -i https://api.todayai.cn/v1/config
HTTP/1.1 403 Forbidden
Server: awselb/2.0
Content-Type: text/html
→ <html><head><title>403 Forbidden</title>...

$ curl -i https://api.today.ai/v1/config      # 国际版一模一样
HTTP/1.1 403 Forbidden
Server: awselb/2.0
Content-Type: text/html
```

关键判据：**同一个 URL，换源 IP 就返回正常业务响应**。

| 请求方 | `GET https://api.todayai.cn/v1/config` |
|---|---|
| 本机直连 | `403` + `text/html`（ALB 拒绝页，带 6 行 `padding to disable MSIE`） |
| 经美国中继 | `401 Missing or invalid Authorization header`（这是应用层的真响应） |

所以 **403 是网络层的 IP 拦截，不是鉴权、不是客户端 bug、不是版本问题**。
把所有能想到的变量都排过了，全部还是 403：

- 方法：`GET` / `POST` / `OPTIONS` 全 403
- 路径：`/`、`/healthz`、`/v1/config`、`/v1/nonexistent-xyz` 全 403
- 头：完整 Chrome 头（含 `sec-ch-ua` / `sec-fetch-*` / `Origin` / `Referer`）→ 403
- 无 UA、HTTP/1.1、TLS1.2 → 403
- 逐 IP 直连（`--resolve` 到 4 个后端节点）→ 全 403
- IPv6：`api.todayai.cn` 没有 AAAA 记录，走不到另一条路

国际版与国内版**同时被拦**，两个区域是不同 AWS region（`cn-northwest-1` /
`us-west-2`），但共用同一套 IP 策略。

## 3. 为什么网页能登

因为网页登录**根本不碰被拦的域名**。

国内版登录页 `todayai.cn/login` 用的是手机验证码，全部同源：

```
POST https://todayai.cn/api/auth/phone-number/send-otp   → 200
POST https://todayai.cn/api/auth/phone-number/verify     → 200
GET  https://todayai.cn/api/auth/get-session             → 200
```

`todayai.cn` 挂在阿里 CDN/Tengine 后面（不是那个 AWS ALB），所以通。

顺带发现：网页版调 API 也走同源代理，不直连 `api.todayai.cn`。证据是
`apps/web/.next/routes-manifest.json` 里的 rewrite：

```json
{"source": "/api/cloud/:path*",    "destination": "https://api.todayai.cn/:path*"},
{"source": "/api/cloud-cn/:path*", "destination": "https://api.todayai.cn/:path*"}
```

这条 rewrite 是**服务端**转发，浏览器只跟 `todayai.cn` 说话。实测：

```
$ curl -i https://todayai.cn/api/cloud/v1/config
HTTP/1.1 401 Unauthorized        ← 应用层真响应
Missing or invalid Authorization header

$ curl -i https://todayai.cn/api/cloud/v1/config -H 'Authorization: Bearer x'
HTTP/1.1 401 Unauthorized
Token introspection failed       ← 走到了 JWT 校验
```

国际版同款也通：`https://today.ai/api/cloud/v1/config` → `401`。

**这就是绕过路径。**

## 4. 桌面客户端为什么绕不过

客户端把 API 域名硬编码在打包配置里：

```js
// dist/app/consts/cloud-public-config.mjs（国际版）
prod: {
  apiBaseUrl: 'https://api.today.ai',
  appUrl: 'https://today.ai',
  baseDomain: 'today.ai',
  betterAuthUrl: 'https://auth.today.ai',
  oidcAuthority: 'https://auth.today.ai',
  tokenAudience: 'https://api.today.ai',
}
```

它直连 `api.*` / `auth.*`，所以必然撞墙。

### 试过但失败的改法（已还原）

把 `resources/app.asar` 解出来的目录放到 `resources/app/`，改配置后启动：

- Electron 的加载顺序是 **`app.asar` 优先于 `app/` 目录**，目录被忽略；
- 把 `app.asar` 改名让开之后配置确实生效了（日志里出现
  `POST https://today.ai/api/cloud/v1/client/logs → 201`），但启动器报
  **「today 无法启动，请重置再试」** ——它校验了 `app.asar` 的存在/完整性。

**已完整还原**：`app.asar.orig` 改回 `app.asar`，`resources/app/` 目录删除，
客户端恢复正常启动。不要重复这个改法。

### 最终采用的改法：本地 TLS 中继（已实现，`cmd/relay`）

`api.todayai.cn` / `auth.todayai.cn` → `127.0.0.1`（hosts），本地起 TLS 服务，
按 SNI 把 API 请求补上 `/api/cloud` 前缀转发到前端站点，Auth 请求原样转发；
自签 CA 装进当前用户根证书库，客户端用 undici 也就认了。

改的只有 hosts + 一个用户级根证书 + 一个常驻进程，全部可回滚
（`rollback.cmd` 删 hosts 段、删根证书、清 `NODE_EXTRA_CA_CERTS`）。

中继额外做了三件事，都是实测逼出来的：

- **换票合成**：桌面端调 `GET {auth}/api/auth/token` + Bearer，前端站点那条路径是
  OAuth2 端点（GET 回 405）。中继把 Bearer 翻成会话 Cookie 去打 `POST {app}/api/token`，
  响应原样回传；
- **WS 垫层**：真 WS 在 ALB 后面，Vercel 又建不了 Upgrade。中继自己完成 101 握手并回 pong，
  客户端的重连循环停下来；业务数据本来就都走 HTTP；
- **`/memory` 只读 DAV 垫层**：见 `docs/reverse-engineering.md` 第 9 节。

### 出口 IP 的补充事实

同一套 hosts + 中继，把 `api.*` / `auth.*` 指回来之后，客户端登录链路是完整的：

```
phone-number/verify 200 → /api/auth/token 200 → users/me 200
  → registry/devices 201 → ws 101
```

也就是说 ALB 拦的只是**源 IP**，不是账号、不是客户端。
换网络（手机热点）也能直接通，不需要中继。


## 5. 网关怎么绕（已实现并验证）

网关可以**完全不用桌面客户端**，直接走同源代理。配置：

```json
"upstream": {
  "app": "https://todayai.cn",
  "auth": "https://todayai.cn",
  "api": "https://todayai.cn/api/cloud",
  "audience": "https://api.todayai.cn"
}
```

两个要点：

- `api` 指向 `/api/cloud` 前缀，实际调用变成 `{app}/api/cloud/v1/...`；
- **`audience` 必须留在官方 API 域名**。换成代理地址的话，签出来的 JWT 受众
  不匹配，上游会以 `Token introspection failed` 拒绝。代码里 `Bases.Audience`
  和 `Bases.API` 就是为此拆开的。

实测（假 Cookie，验证链路真的到了上游）：

```
$ curl -X POST http://127.0.0.1:8692/api/panel/accounts/acc_cn/check
{"error":{"code":502,
 "message":"today auth: HTTP 401 {\"error\":\"Unauthorized\"}"}}
```

`401 {"error":"Unauthorized"}` 是 better-auth 的真响应 —— 说明换票请求真的
打到了 `todayai.cn/api/token` 后面的鉴权服务，而不是被 ALB 挡在门外。

## 6. 拿到 Cookie 的最快路径

网页登录是通的，所以：

1. 浏览器打开 `https://todayai.cn/login`，手机号 + 验证码登录；
2. F12 → Application → Cookies → `https://todayai.cn`
   复制 `__Secure-better-auth.session_token` 的值（或整段 Cookie 头）；
3. 粘到网关面板「账号」页；
4. 网关用它换 JWT → 走 `todayai.cn/api/cloud` → 全功能可用。

只需要一次粘贴：`__Secure-better-auth.session_token` 的有效期是 **60 天**
（登录时 `Set-Cookie` 的 `Max-Age=5184000`），而 JWT 只有 1 小时 ——
网关存 Cookie，每次提前 60 秒自动换新 JWT，你不用管。
