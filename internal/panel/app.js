/* Today2API 控制台前端。无框架：一次 fetch 拉全量状态，切 tab 只切换显示。 */
"use strict";

const $ = (sel) => document.querySelector(sel);
const state = { data: null, view: "overview", growth: { account: "", rapport: null, tasks: null }, memory: { account: "", data: null } };

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
    <th>备注</th><th>套餐</th><th>状态</th><th>最近使用</th><th>错误</th><th></th>
  </tr></thead><tbody>${s.accounts.map((a) => {
    const cooling = a.cooldown_until && new Date(a.cooldown_until) > new Date();
    const status = !a.enabled
      ? badge("已停用", "mute")
      : cooling ? badge("冷却中", "warn") : a.fails > 0 ? badge("失败 " + a.fails, "warn") : badge("可用", "ok");
    return `<tr>
      <td>${esc(a.label)}<div class="sub mono">${esc(a.id)}</div></td>
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
  const ep = s.config.upstream || {};
  const rows = `<tr>
      <td class="mono">${esc(ep.app || "-")}</td>
      <td class="mono">${esc(ep.api || "-")}</td>
      <td class="mono">${esc(ep.auth || "-")}</td>
      <td class="mono">${esc(ep.audience || "-")}</td></tr>`;
  $("#upstreamTable").innerHTML =
    `<table><thead><tr><th>站点</th><th>API</th><th>认证</th><th>JWT 受众</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function renderSettings(s) {
  const c = s.config;
  $("#cfgModel").value = c.default_model;
  $("#cfgTimeout").value = c.reply_timeout_sec;
  $("#cfgPoll").value = c.poll_interval_ms;
  $("#cfgTurns").value = c.context_turns;
  $("#cfgCheckin").value = c.checkin && c.checkin.enabled ? "1" : "0";
  $("#cfgCheckinMin").value = (c.checkin && c.checkin.interval_minutes) || 720;
  $("#cfgPassword").value = "";
  $("#cfgPassword").placeholder = c.panel_password_set ? "已设置；不修改请留空" : "留空 = 仅本机可管理";
}

/* ---------- 记忆 ---------- */

function renderMemoryAccounts(s) {
  const sel = $("#memAccount");
  if (!sel) return;
  const prev = state.memory.account || sel.value;
  sel.innerHTML = s.accounts.length
    ? s.accounts.map((a) => `<option value="${esc(a.id)}">${esc(a.label)}</option>`).join("")
    : '<option value="">（无账号）</option>';
  if (prev && s.accounts.some((a) => a.id === prev)) sel.value = prev;
  state.memory.account = sel.value;
}

function renderMemoryPins() {
  const el = $("#memPins");
  const d = state.memory.data;
  if (!d) {
    el.innerHTML = '<div class="empty">先选择账号</div>';
    return;
  }
  if (!d.pins || !d.pins.length) {
    el.innerHTML = '<div class="empty">没有钉住的目录。下面钉一个，或先导入一条记忆。</div>';
    return;
  }
  el.innerHTML = d.pins.map((p) => {
    const files = (p.files || []).map((f) => `
      <div style="margin:10px 0 0">
        <div class="row" style="justify-content:space-between">
          <strong>${esc(f.path)}</strong>
          <span class="hint">${esc(f.contentType || "")}</span>
        </div>
        <pre class="log" style="white-space:pre-wrap;margin-top:6px">${esc(f.content || "（空）")}</pre>
      </div>`).join("");
    return `<div style="padding:12px 0;border-bottom:1px solid var(--line)">
      <div class="row" style="justify-content:space-between">
        <strong>${esc(p.path)}</strong>
        <span class="hint">${(p.files || []).length} 个文件 · ${esc(p.pinnedAt || "")}</span>
      </div>
      ${files || '<div class="empty">这个目录下没有文件</div>'}
    </div>`;
  }).join("");
}

async function loadMemory() {
  const id = $("#memAccount") ? $("#memAccount").value : "";
  state.memory.account = id;
  if (!id) return;
  try {
    state.memory.data = await api(`/api/panel/accounts/${encodeURIComponent(id)}/memories`);
  } catch (err) {
    state.memory.data = null;
    toast("记忆拉取失败：" + err.message, "bad");
  }
  renderMemoryPins();
}

function renderMemoryImport(out) {
  const el = $("#memImportResult");
  if (!out) {
    el.innerHTML = '<div class="empty">尚未导入</div>';
    return;
  }
  if (out.error) {
    el.innerHTML = `<div class="empty">失败：${esc(out.error)}</div>`;
    return;
  }
  const t = out.task || {};
  el.innerHTML = `<table><thead><tr><th>项</th><th>值</th></tr></thead><tbody>
    <tr><td>文件</td><td>${esc((out.file && out.file.filename) || "")} · ${esc((out.file && out.file.id) || "")}</td></tr>
    <tr><td>任务</td><td>${esc(t.id || "")}</td></tr>
    <tr><td>状态</td><td>${t.status === "completed" ? badge("已完成", "ok") : badge(esc(t.status || "queued"), "")}</td></tr>
    <tr><td>幂等</td><td>${out.idempotent ? "是（同一文件重复提交）" : "否"}</td></tr>
    <tr><td>摘要</td><td>${esc((t.completionSummary || "（上游未返回）").slice(0, 400))}</td></tr>
  </tbody></table>
  <div class="hint" style="margin-top:8px">任务跑完后点上面的「刷新」看落地结果。</div>`;
}

/* ---------- 签到 · 任务 ---------- */

function renderGrowthAccounts(s) {
  const sel = $("#growthAccount");
  const prev = state.growth.account || sel.value;
  sel.innerHTML = s.accounts.length
    ? s.accounts.map((a) => `<option value="${esc(a.id)}">${esc(a.label)}</option>`).join("")
    : '<option value="">（无账号）</option>';
  if (prev && s.accounts.some((a) => a.id === prev)) sel.value = prev;
  state.growth.account = sel.value;
}

function renderRapport() {
  const el = $("#rapportBody");
  const r = state.growth.rapport;
  if (!r) {
    el.innerHTML = '<div class="empty">先在上方选择账号</div>';
    return;
  }
  const rapport = r.rapport || {};
  const rules = rapport.rules || [];
  const done = rules.filter((x) => x.status === "completed").length;
  el.innerHTML = `
    <div class="cards" style="margin-bottom:14px">
      <div class="card"><div class="k">积分</div><div class="v">${esc(rapport.points ?? "-")}</div></div>
      <div class="card"><div class="k">等级</div><div class="v">${esc(rapport.levelKey || rapport.level || "-")}</div></div>
      <div class="card"><div class="k">已完成规则</div><div class="v">${done}<small>/${rules.length}</small></div></div>
    </div>
    <table><thead><tr><th>规则</th><th>说明</th><th>分值</th><th>状态</th></tr></thead><tbody>
    ${rules.length
      ? rules.map((x) => `<tr>
          <td>${esc(x.title || x.key)}<div class="sub mono">${esc(x.key)}</div></td>
          <td class="sub">${esc([x.description, x.note].filter(Boolean).join(" · "))}</td>
          <td class="mono">+${esc(x.points || 0)}</td>
          <td>${x.status === "completed" ? badge("已入账", "ok") : badge(esc(x.status || "待完成"), "mute")}</td>
        </tr>`).join("")
      : '<tr><td colspan="4" class="empty">这个账号还没有规则卡片</td></tr>'}
    </tbody></table>`;
}

function renderTasks() {
  const el = $("#taskTable");
  const rec = $("#taskRecommended");
  const t = state.growth.tasks;
  if (!t) {
    el.innerHTML = '<div class="empty">先在上方选择账号</div>';
    rec.innerHTML = "";
    return;
  }
  const tasks = t.tasks || [];
  el.innerHTML = tasks.length
    ? `<table><thead><tr><th>任务</th><th>类型</th><th>状态</th><th>创建</th><th></th></tr></thead><tbody>
      ${tasks.map((x) => `<tr>
        <td>${esc(x.title || x.id)}<div class="sub mono">${esc(x.id)}</div></td>
        <td class="mono">${esc(x.type || "-")}</td>
        <td>${badge(esc(x.status || "-"), x.status === "completed" ? "ok" : x.status === "failed" ? "bad" : "mute")}</td>
        <td class="mono">${fmtTime(x.createdAt)}</td>
        <td>${["running", "queued"].includes(x.status)
          ? `<button class="danger" data-task-stop="${esc(x.id)}">停止</button>`
          : ""}</td>
      </tr>`).join("")}</tbody></table>`
    : '<div class="empty">没有任务</div>';

  const recs = t.recommended || [];
  rec.innerHTML = recs.length
    ? `<p class="hint" style="margin-bottom:8px">推荐任务</p><div class="row">${recs
        .map((x) => `<button class="ghost" data-task-apply="${esc(x.id)}">${esc(x.title || x.id)}</button>`)
        .join("")}</div>`
    : "";
}

async function loadGrowth() {
  const id = $("#growthAccount") ? $("#growthAccount").value : "";
  state.growth.account = id;
  if (!id) return;
  try {
    const [rapport, tasks] = await Promise.all([
      api(`/api/panel/accounts/${encodeURIComponent(id)}/rapport`).catch((e) => ({ error: e.message })),
      api(`/api/panel/accounts/${encodeURIComponent(id)}/tasks`).catch((e) => ({ error: e.message })),
    ]);
    state.growth.rapport = rapport.error ? null : rapport;
    state.growth.tasks = tasks.error ? null : tasks;
    if (rapport.error) toast("积分拉取失败：" + rapport.error, "bad");
    if (tasks.error) toast("任务拉取失败：" + tasks.error, "bad");
  } catch (err) {
    toast(err.message, "bad");
  }
  renderRapport();
  renderTasks();
}

function renderCheckinRows(rows) {
  const el = $("#checkinResult");
  if (!rows || !rows.length) {
    el.innerHTML = '<div class="empty">没有启用中的账号</div>';
    return;
  }
  el.innerHTML = `<table><thead><tr><th>账号</th><th>结果</th><th>积分变化</th><th>说明</th></tr></thead><tbody>
    ${rows.map((r) => `<tr>
      <td>${esc(r.label || r.account)}</td>
      <td>${r.status === "ok" ? badge("已签到", "ok") : badge("失败", "bad")}</td>
      <td class="mono">${r.delta > 0 ? "+" + r.delta : r.delta === 0 ? "±0" : r.delta}</td>
      <td class="sub">${esc(r.detail || "")}</td>
    </tr>`).join("")}</tbody></table>`;
}

function renderAll() {
  const s = state.data;
  if (!s) return;
  renderOverview(s);
  renderAccounts(s);
  renderGrowthAccounts(s);
  renderMemoryAccounts(s);
  renderKeys(s);
  renderSettings(s);
  renderLogs(s.logs, "#logTable");
  // switchView 可能早于账号就绪，那时 #memAccount 还是空的，
  // 记忆页就停在「先选择账号」。等账号渲染完再补一次自动加载。
  if (state.view === "memory" && !state.memory.data) loadMemory();
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
  const names = { overview: "概览", accounts: "账号", growth: "签到 · 任务", memory: "记忆", keys: "密钥", settings: "设置", logs: "日志" };
  document.querySelectorAll("main > section").forEach((sec) => (sec.hidden = sec.id !== "view-" + view));
  $("#pageTitle").textContent = names[view] || view;
  if (location.hash !== "#" + view) history.replaceState(null, "", "#" + view);
  if (view === "growth" && state.data) loadGrowth();
  if (view === "memory" && state.data) loadMemory();
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

$("#addCookieBtn").addEventListener("click", async () => {
  const btn = $("#addCookieBtn");
  btn.disabled = true;
  btn.textContent = "校验中…";
  try {
    await api("/api/panel/accounts", {
      method: "POST",
      body: JSON.stringify({
        label: $("#addLabel").value,
        cookie: $("#addCredType").value === "cookie" ? $("#addCookieText").value : "",
        token: $("#addCredType").value === "token" ? $("#addCookieText").value : "",
      }),
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

document.addEventListener("click", async (e) => {
  const stop = e.target.closest("button[data-task-stop]");
  if (stop) {
    const id = state.growth.account;
    if (!confirm("终止这个任务？")) return;
    try {
      await api(`/api/panel/accounts/${encodeURIComponent(id)}/tasks/${encodeURIComponent(stop.dataset.taskStop)}/stop`, { method: "POST" });
      toast("已终止", "ok");
      await loadGrowth();
    } catch (err) {
      toast(err.message, "bad");
    }
    return;
  }
  const apply = e.target.closest("button[data-task-apply]");
  if (apply) {
    const id = state.growth.account;
    try {
      await api(`/api/panel/accounts/${encodeURIComponent(id)}/tasks/recommendations/${encodeURIComponent(apply.dataset.taskApply)}/apply`, { method: "POST" });
      toast("已采纳推荐任务", "ok");
      await loadGrowth();
    } catch (err) {
      toast(err.message, "bad");
    }
    return;
  }
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
    checkin: {
      enabled: $("#cfgCheckin").value === "1",
      interval_minutes: Number($("#cfgCheckinMin").value) || 0,
    },
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

$("#growthAccount").addEventListener("change", () => {
  state.growth.account = $("#growthAccount").value;
  loadGrowth();
});
$("#growthRefreshBtn").addEventListener("click", () => loadGrowth());

$("#checkinOneBtn").addEventListener("click", async () => {
  const id = state.growth.account;
  if (!id) return toast("先选账号", "bad");
  const btn = $("#checkinOneBtn");
  btn.disabled = true;
  btn.textContent = "签到中…";
  try {
    const r = await api(`/api/panel/accounts/${encodeURIComponent(id)}/checkin`, { method: "POST" });
    const res = r.result || {};
    toast(`签到完成 · 积分 ${res.delta > 0 ? "+" + res.delta : "±0"}`, "ok");
    renderCheckinRows([{ label: r.label, account: r.account, status: "ok", delta: res.delta || 0 }]);
    await loadGrowth();
  } catch (err) {
    toast(err.message, "bad");
  } finally {
    btn.disabled = false;
    btn.textContent = "单独签到";
  }
});

$("#checkinAllBtn").addEventListener("click", async () => {
  const btn = $("#checkinAllBtn");
  btn.disabled = true;
  btn.textContent = "签到中…";
  try {
    const r = await api("/api/panel/checkin", { method: "POST" });
    renderCheckinRows(r.rows || []);
    toast(`签到完成：${r.ok} 成功 / ${r.failed} 失败`, r.failed ? "bad" : "ok");
    await loadGrowth();
  } catch (err) {
    toast(err.message, "bad");
  } finally {
    btn.disabled = false;
    btn.textContent = "全部账号签到";
  }
});

$("#memRefreshBtn").addEventListener("click", () => loadMemory());

$("#memAccount").addEventListener("change", () => {
  state.memory.account = $("#memAccount").value;
  loadMemory();
});

$("#memPinBtn").addEventListener("click", async () => {
  const path = ($("#memPinPath").value || "").trim();
  const id = state.memory.account;
  if (!id) return toast("先选账号", "bad");
  if (!path) return toast("填个目录，例如 /memory/notes", "bad");
  try {
    await api(`/api/panel/accounts/${encodeURIComponent(id)}/memories/pin`, {
      method: "POST",
      body: JSON.stringify({ path }),
    });
    toast("已钉住 " + path, "ok");
    await loadMemory();
  } catch (err) {
    toast(err.message, "bad");
  }
});

$("#memUnpinBtn").addEventListener("click", async () => {
  const path = ($("#memPinPath").value || "").trim();
  const id = state.memory.account;
  if (!id) return toast("先选账号", "bad");
  if (!path) return toast("填要取消的目录", "bad");
  try {
    await api(`/api/panel/accounts/${encodeURIComponent(id)}/memories/unpin`, {
      method: "POST",
      body: JSON.stringify({ path }),
    });
    toast("已取消 " + path, "ok");
    await loadMemory();
  } catch (err) {
    toast(err.message, "bad");
  }
});

$("#memImportBtn").addEventListener("click", async () => {
  const btn = $("#memImportBtn");
  const id = state.memory.account;
  if (!id) return toast("先选账号", "bad");
  const content = ($("#memContent").value || "").trim();
  if (!content) return toast("内容不能为空", "bad");
  const wait = $("#memWait").checked ? 45 : 0;

  btn.disabled = true;
  btn.textContent = wait ? "导入中…" : "提交中…";
  try {
    const out = await api(`/api/panel/accounts/${encodeURIComponent(id)}/memories/import`, {
      method: "POST",
      body: JSON.stringify({
        filename: ($("#memFileName").value || "").trim() || "note.md",
        content,
        wait_sec: wait,
      }),
    });
    renderMemoryImport(out);
    if (out.terminal) {
      toast("已写入，刷新可见", "ok");
      await loadMemory();
    } else {
      toast("已排队，稍后点刷新", "");
    }
  } catch (err) {
    renderMemoryImport({ error: err.message });
    toast(err.message, "bad");
  } finally {
    btn.disabled = false;
    btn.textContent = "导入";
  }
});

/* ---------- 启动 ---------- */

switchView((location.hash || "#overview").slice(1));
load();
setInterval(() => {
  if (document.visibilityState === "visible" && state.data) load();
}, 5000);
