/* Today2API 控制台前端。无框架：一次 fetch 拉全量状态，切 tab 只切换显示。 */
"use strict";

const $ = (sel) => document.querySelector(sel);
const state = { data: null, view: "overview" };

const esc = (v) =>
  String(v == null ? "" : v).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function toast(msg, kind) {
  const el = document.createElement("div");
  el.className = "toast" + (kind ? " " + kind : "");
  el.textContent = msg;
  $("#toast").appendChild(el);
  setTimeout(() => el.remove(), 4200);
}

async function api(path, opts) {
  const res = await fetch(path, Object.assign({ headers: { "Content-Type": "application/json" } }, opts || {}));
  const text = await res.text();
  let body = {};
  try {
    body = text ? JSON.parse(text) : {};
  } catch {
    body = { error: { message: text } };
  }
  if (res.status === 401 && path.startsWith("/api/panel")) {
    showLogin(true);
    throw new Error("login required");
  }
  if (!res.ok) {
    const msg = (body.error && body.error.message) || text || res.statusText;
    throw new Error(msg);
  }
  return body;
}

function showLogin(on, hint) {
  $("#login").hidden = !on;
  if (hint) $("#loginHint").textContent = hint;
  if (on) $("#loginPassword").focus();
}

/* ---------- 渲染 ---------- */

function verTag(v) {
  return String(v || "dev").startsWith("v") ? v : "v" + v;
}

function badge(text, kind) {
  return `<span class="badge ${kind}">${esc(text)}</span>`;
}

function fmtTime(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d)) return "-";
  const pad = (n) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function renderOverview(s) {
  $("#statAccounts").textContent = s.stats.accounts;
  $("#statEnabled").textContent = s.stats.enabled;
  $("#statCooling").textContent = s.stats.cooling;
  const up = s.uptime;
  $("#statUptime").innerHTML = up >= 3600
    ? `${Math.floor(up / 3600)}<small>h</small>`
    : up >= 60 ? `${Math.floor(up / 60)}<small>m</small>` : `${up}<small>s</small>`;
  $("#listenMeta").textContent = s.listen + " · " + verTag(s.version);
  $("#footVersion").textContent = verTag(s.version);
  $("#logoutBtn").hidden = !s.config.panel_password_set;

  const models = new Set([s.config.default_model, "deepseek-v4.1-flash", "glm-5.3", "minimax-m31-flash"]);
  const sel = $("#probeModel");
  const prev = sel.value;
  sel.innerHTML = [...models].map((m) => `<option value="${esc(m)}">${esc(m)}</option>`).join("");
  sel.value = prev && models.has(prev) ? prev : s.config.default_model;

  renderLogs(s.logs.slice(0, 20), "#recentLogs");
  renderUpstream(s);
}

function renderLogs(logs, target) {
  const el = $(target);
  if (!logs.length) {
    el.innerHTML = '<div class="empty">暂无请求</div>';
    return;
  }
  el.innerHTML = `<table><thead><tr>
    <th>时间</th><th>路径</th><th>模型</th><th>账号</th><th>状态</th><th>耗时</th><th>输出</th><th>错误</th>
  </tr></thead><tbody>${logs.map((l) => `<tr>
    <td class="mono">${fmtTime(l.time)}</td>
    <td class="mono">${esc(l.path)}</td>
    <td class="mono">${esc(l.model || "-")}</td>
    <td>${esc(l.account || "-")}</td>
    <td>${l.error ? badge(String(l.status), "bad") : badge(String(l.status), "ok")}</td>
    <td class="mono">${l.duration_ms}ms</td>
    <td class="mono">${l.chars || 0}</td>
    <td class="sub">${esc(l.error || "")}</td>
  </tr>`).join("")}</tbody></table>`;
}

function renderAccounts(s) {
  const el = $("#accountTable");
  if (!s.accounts.length) {
    el.innerHTML = '<div class="empty">还没有账号，先在上面添加</div>';
    return;
  }
  el.innerHTML = `<table><thead><tr>
    <th>备注</th><th>区域</th><th>套餐</th><th>状态</th><th>最近使用</th><th>错误</th><th></th>
  </tr></thead><tbody>${s.accounts.map((a) => {
    const cooling = a.cooldown_until && new Date(a.cooldown_until) > new Date();
    const status = !a.enabled
      ? badge("已停用", "mute")
      : cooling ? badge("冷却中", "warn") : a.fails > 0 ? badge("失败 " + a.fails, "warn") : badge("可用", "ok");
    return `<tr>
      <td>${esc(a.label)}<div class="sub mono">${esc(a.id)}</div></td>
      <td>${a.realm === "cn" ? badge("国内", "mute") : badge("国际", "mute")}</td>
      <td class="mono">${esc(a.plan || "-")}</td>
      <td>${status}</td>
      <td class="mono">${fmtTime(a.last_used)}</td>
      <td class="sub">${esc(a.last_err || "")}</td>
      <td><div class="row">
        <button class="ghost" data-act="check" data-id="${esc(a.id)}">检查</button>
        <button class="ghost" data-act="toggle" data-id="${esc(a.id)}" data-on="${a.enabled ? "1" : "0"}">${a.enabled ? "停用" : "启用"}</button>
        <button class="danger" data-act="del" data-id="${esc(a.id)}">删除</button>
      </div></td>
    </tr>`;
  }).join("")}</tbody></table>`;
}

function renderKeys(s) {
  const el = $("#keyTable");
  if (!s.keys.length) {
    el.innerHTML = '<div class="empty">还没有密钥：不配密钥时网关不校验鉴权</div>';
  } else {
    el.innerHTML = `<table><thead><tr><th>名称</th><th>前缀</th><th>创建</th><th></th></tr></thead><tbody>
      ${s.keys.map((k) => `<tr>
        <td>${esc(k.name)}</td>
        <td class="mono">${esc(k.prefix)}</td>
        <td class="mono">${fmtTime(k.created)}</td>
        <td><button class="danger" data-key="${esc(k.name)}">删除</button></td>
      </tr>`).join("")}</tbody></table>`;
  }
  const base = location.origin;
  $("#usageSnippet").textContent = [
    "# OpenAI / Codex",
    `export OPENAI_BASE_URL=${base}/v1`,
    "export OPENAI_API_KEY=<上一步创建的密钥>",
    "",
    "# Claude Code",
    `export ANTHROPIC_BASE_URL=${base}`,
    "export ANTHROPIC_API_KEY=<上一步创建的密钥>",
    "",
    `# curl ${base}/v1/chat/completions -H "Authorization: Bearer <key>" \\`,
    `#   -d '{"model":"${s.config.default_model}","messages":[{"role":"user","content":"你好"}],"stream":true}'`,
  ].join("\n");
}

function renderUpstream(s) {
  const rows = ["cn", "global"].map((realm) => {
    const ep = (s.config.upstream || {})[realm] || {};
    const name = realm === "cn" ? "国内版" : "国际版";
    return `<tr><td>${name}</td>
      <td class="mono">${esc(ep.app || "-")}</td>
      <td class="mono">${esc(ep.api || "-")}</td>
      <td class="mono">${esc(ep.auth || "-")}</td></tr>`;
  }).join("");
  $("#upstreamTable").innerHTML = `<table><thead><tr><th>区域</th><th>站点</th><th>API</th><th>认证</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function renderSettings(s) {
  const c = s.config;
  $("#cfgModel").value = c.default_model;
  $("#cfgTimeout").value = c.reply_timeout_sec;
  $("#cfgPoll").value = c.poll_interval_ms;
  $("#cfgTurns").value = c.context_turns;
  $("#cfgPassword").value = "";
  $("#cfgPassword").placeholder = c.panel_password_set ? "已设置；不修改请留空" : "留空 = 仅本机可管理";
}

function renderAll() {
  const s = state.data;
  if (!s) return;
  renderOverview(s);
  renderAccounts(s);
  renderKeys(s);
  renderSettings(s);
  renderLogs(s.logs, "#logTable");
}

/* ---------- 数据 ---------- */

async function load() {
  try {
    const data = await api("/api/panel/state");
    const logs = await api("/api/panel/logs").catch(() => ({ logs: [] }));
    data.logs = logs.logs || [];
    state.data = data;
    showLogin(false);
    renderAll();
  } catch (err) {
    if (err.message === "login required") return;
    if (!state.data) showLogin(true, err.message);
    else toast(err.message, "bad");
  }
}

/* ---------- 事件 ---------- */

function switchView(view) {
  state.view = view;
  document.querySelectorAll(".nav a").forEach((a) => a.classList.toggle("active", a.dataset.view === view));
  const names = { overview: "概览", accounts: "账号", keys: "密钥", settings: "设置", logs: "日志" };
  document.querySelectorAll("main > section").forEach((sec) => (sec.hidden = sec.id !== "view-" + view));
  $("#pageTitle").textContent = names[view] || view;
  if (location.hash !== "#" + view) history.replaceState(null, "", "#" + view);
}

document.querySelectorAll(".nav a").forEach((a) =>
  a.addEventListener("click", (e) => {
    e.preventDefault();
    switchView(a.dataset.view);
  }));

$("#refreshBtn").addEventListener("click", () => load());

$("#loginForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("/api/panel/login", { method: "POST", body: JSON.stringify({ password: $("#loginPassword").value }) });
    $("#loginPassword").value = "";
    await load();
    toast("已登录", "ok");
  } catch (err) {
    $("#loginHint").textContent = err.message;
  }
});

$("#logoutBtn").addEventListener("click", async () => {
  await api("/api/panel/logout", { method: "POST" }).catch(() => {});
  state.data = null;
  showLogin(true, "已退出登录");
});

document.querySelectorAll("[data-addmode]").forEach((btn) =>
  btn.addEventListener("click", () => {
    document.querySelectorAll("[data-addmode]").forEach((b) => b.classList.toggle("on", b === btn));
    const otp = btn.dataset.addmode === "otp";
    $("#addCookie").hidden = otp;
    $("#addOtp").hidden = !otp;
  }));

$("#addCookieBtn").addEventListener("click", async () => {
  const btn = $("#addCookieBtn");
  btn.disabled = true;
  btn.textContent = "校验中…";
  try {
    await api("/api/panel/accounts", {
      method: "POST",
      body: JSON.stringify({ realm: $("#addRealm").value, label: $("#addLabel").value, cookie: $("#addCookieText").value }),
    });
    $("#addCookieText").value = "";
    $("#addLabel").value = "";
    toast("账号已添加", "ok");
    await load();
  } catch (err) {
    toast(err.message, "bad");
  } finally {
    btn.disabled = false;
    btn.textContent = "校验并添加";
  }
});

$("#otpSendBtn").addEventListener("click", async () => {
  try {
    await api("/api/panel/otp/send", {
      method: "POST",
      body: JSON.stringify({ realm: $("#otpRealm").value, email: $("#otpEmail").value }),
    });
    toast("验证码已发送", "ok");
  } catch (err) {
    toast(err.message, "bad");
  }
});

$("#otpVerifyBtn").addEventListener("click", async () => {
  try {
    await api("/api/panel/otp/verify", {
      method: "POST",
      body: JSON.stringify({ realm: $("#otpRealm").value, email: $("#otpEmail").value, code: $("#otpCode").value }),
    });
    toast("登录并添加成功", "ok");
    $("#otpCode").value = "";
    await load();
  } catch (err) {
    toast(err.message, "bad");
  }
});

document.addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-act]");
  if (btn) {
    const id = btn.dataset.id;
    try {
      if (btn.dataset.act === "check") {
        btn.disabled = true;
        btn.textContent = "检查中…";
        const r = await api(`/api/panel/accounts/${id}/check`, { method: "POST" });
        toast(`账号正常${r.plan ? " · 套餐 " + r.plan : ""}`, "ok");
      } else if (btn.dataset.act === "toggle") {
        await api(`/api/panel/accounts/${id}`, {
          method: "PATCH",
          body: JSON.stringify({ enabled: btn.dataset.on !== "1" }),
        });
      } else if (btn.dataset.act === "del") {
        if (!confirm("删除这个账号？")) return;
        await api(`/api/panel/accounts/${id}`, { method: "DELETE" });
        toast("已删除", "ok");
      }
      await load();
    } catch (err) {
      toast(err.message, "bad");
      btn.disabled = false;
      btn.textContent = "检查";
    }
    return;
  }
  const kill = e.target.closest("button[data-key]");
  if (kill) {
    if (!confirm(`删除密钥 ${kill.dataset.key}？`)) return;
    try {
      await api(`/api/panel/keys/${encodeURIComponent(kill.dataset.key)}`, { method: "DELETE" });
      toast("已删除", "ok");
      await load();
    } catch (err) {
      toast(err.message, "bad");
    }
  }
});

$("#keyCreateBtn").addEventListener("click", async () => {
  try {
    const r = await api("/api/panel/keys", { method: "POST", body: JSON.stringify({ name: $("#keyName").value }) });
    $("#keyName").value = "";
    $("#keyOut").hidden = false;
    $("#keyOut").textContent = "密钥（只显示这一次）：\n" + r.key;
    toast("密钥已创建", "ok");
    await load();
  } catch (err) {
    toast(err.message, "bad");
  }
});

$("#cfgSaveBtn").addEventListener("click", async () => {
  const body = {
    default_model: $("#cfgModel").value,
    reply_timeout_sec: Number($("#cfgTimeout").value),
    poll_interval_ms: Number($("#cfgPoll").value),
    context_turns: Number($("#cfgTurns").value),
  };
  const pwd = $("#cfgPassword").value;
  if (pwd) body.panel_password = pwd;
  try {
    await api("/api/panel/config", { method: "PUT", body: JSON.stringify(body) });
    toast("已保存", "ok");
    await load();
  } catch (err) {
    toast(err.message, "bad");
  }
});

$("#probeBtn").addEventListener("click", async () => {
  const out = $("#probeOut");
  out.textContent = "请求中…（上游首字 3~10 秒）";
  try {
    const r = await api("/api/panel/probe", {
      method: "POST",
      body: JSON.stringify({ model: $("#probeModel").value, prompt: "用一句话介绍你自己。" }),
    });
    out.textContent =
      `模型 ${r.model} · 账号 ${r.account || "-"} · ${r.duration_ms}ms\n\n` + (r.reply || "") + (r.error ? "\n\n错误：" + r.error : "");
  } catch (err) {
    out.textContent = "失败：" + err.message;
  }
  load();
});

/* ---------- 启动 ---------- */

switchView((location.hash || "#overview").slice(1));
load();
setInterval(() => {
  if (document.visibilityState === "visible" && state.data) load();
}, 5000);
