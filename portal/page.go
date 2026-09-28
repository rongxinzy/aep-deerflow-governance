package main

import (
	"html/template"
	"net"
	"net/http"
)

// The /chat entry page: the platform's single login + employee-picker
// surface. It exists so the reused DeerFlow frontend needs zero login UI —
// the browser arrives at the chat app already holding the portal session,
// CSRF, and selected-employee cookies (cookies ignore ports, so they flow
// from this origin to the chat UI origin on the same host).
const chatEntryHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>数字员工 · 对话入口</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
         max-width: 560px; margin: 48px auto; padding: 0 16px; line-height: 1.6; }
  h1 { font-size: 1.25rem; }
  input { width: 100%; box-sizing: border-box; padding: 8px 10px; margin: 4px 0 12px;
          border: 1px solid #888; border-radius: 6px; background: inherit; color: inherit; }
  button { padding: 8px 16px; border: 1px solid #888; border-radius: 6px; cursor: pointer;
           background: inherit; color: inherit; font-size: .95rem; }
  button.employee { display: block; width: 100%; text-align: left; margin: 8px 0; padding: 12px 14px; }
  button.employee small { display: block; color: #888; }
  .muted { color: #888; font-size: .9rem; }
  .error { color: #c0392b; min-height: 1.4em; margin: 8px 0; }
  header { display: flex; justify-content: space-between; align-items: baseline; }
</style>
</head>
<body>
<header><h1>数字员工对话</h1><button id="logout" hidden>退出登录</button></header>
<p class="error" id="error"></p>

<section id="login" hidden>
  <p>使用企业账号（AEP）登录后选择要对话的数字员工。</p>
  <form id="login-form">
    <label>用户名<input id="username" autocomplete="username" required></label>
    <label>密码<input id="password" type="password" autocomplete="current-password" required></label>
    <button type="submit">登录</button>
  </form>
</section>

<section id="picker" hidden>
  <p>选择数字员工开始对话：</p>
  <div id="employees"><p class="muted">加载中…</p></div>
  <p class="muted" id="hint"></p>
</section>

<script>
const CHAT_URL = {{.ChatURL}};
// Read the csrf cookie fresh on every call: the #token= handoff (and the
// login form) mint a NEW session whose csrf cookie lands after page load —
// a load-time const would carry a stale/empty value into the very next POST
// and 403 with "csrf token missing or mismatched" (Console jump → autoSelect).
const csrfToken = () => { const m = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/); return m ? decodeURIComponent(m[1]) : ""; };

const $ = (id) => document.getElementById(id);

async function api(path, options = {}) {
  const init = { credentials: "same-origin", headers: {}, ...options };
  if (init.method && init.method !== "GET") init.headers["X-CSRF-Token"] = csrfToken();
  if (init.body) init.headers["Content-Type"] = "application/json";
  const res = await fetch(path, init);
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.status);
  return res.json();
}

// The platform (Admin Console) hands over an AEP access token in the URL
// fragment — fragments are never sent to servers and never appear in
// Referer. Exchange it for the portal session, wiping it from the address
// bar immediately whether the exchange succeeds or not.
async function consumeFragmentToken() {
  const match = /^#token=(.+)$/.exec(location.hash);
  if (!match) return;
  history.replaceState(null, "", location.pathname + location.search);
  try {
    await api("/api/v1/session", {
      method: "POST",
      body: JSON.stringify({ token: decodeURIComponent(match[1]) }),
    });
  } catch {
    // Fall through: the login form below takes over.
  }
}

async function boot() {
  await consumeFragmentToken();
  try {
    await api("/api/v1/session");
    $("login").hidden = true;
    $("picker").hidden = false;
    $("logout").hidden = false;
    await loadEmployees();
    autoSelect();
  } catch {
    $("login").hidden = false;
  }
}

// ?employee= from a platform link preselects and jumps straight to the chat.
function autoSelect() {
  const name = new URLSearchParams(location.search).get("employee");
  if (!name) return;
  for (const btn of document.querySelectorAll("#employees button.employee")) {
    if (btn.dataset.name === name) {
      void select(name, btn);
      return;
    }
  }
}

async function loadEmployees() {
  const { employees } = await api("/api/v1/employees");
  const box = $("employees");
  box.replaceChildren();
  if (!employees.length) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.textContent = "还没有可用的数字员工（需 owner 或管理员，且实例 Ready）。";
    box.appendChild(empty);
    return;
  }
  for (const e of employees) {
    const btn = document.createElement("button");
    btn.className = "employee";
    btn.dataset.name = e.name;
    const title = document.createElement("span");
    title.textContent = e.displayName || e.name;
    const sub = document.createElement("small");
    sub.textContent = e.name + " · " + (e.phase || "");
    btn.append(title, sub);
    btn.onclick = () => select(e.name, btn);
    box.appendChild(btn);
  }
}

async function select(name, btn) {
  btn.disabled = true;
  $("error").textContent = "";
  try {
    const out = await api("/api/v1/session/employee", { method: "POST", body: JSON.stringify({ name }) });
    if (!out.chatURL) {
      $("error").textContent = "已选择 " + name + "，但门户未配置对话界面地址（PORTAL_CHAT_UI_URL）。";
      return;
    }
    window.location.href = out.chatURL;
  } catch (err) {
    $("error").textContent = String(err.message || err);
    btn.disabled = false;
  }
}

$("login-form").onsubmit = async (ev) => {
  ev.preventDefault();
  $("error").textContent = "";
  try {
    await api("/api/v1/session", {
      method: "POST",
      body: JSON.stringify({ username: $("username").value, password: $("password").value }),
    });
    location.reload();
  } catch (err) {
    $("error").textContent = "登录失败：" + (err.message || err);
  }
};

$("logout").onclick = async () => {
  try { await api("/api/v1/session", { method: "DELETE" }); } catch {}
  location.reload();
};

boot();
</script>
</body>
</html>
`

var chatEntryTemplate = template.Must(template.New("chat-entry").Parse(chatEntryHTML))

// chatUIURL resolves where the reused DeerFlow frontend is served. Default
// derives from the request's own host (the chat UI runs on the same host,
// NodePort 30195): whichever hostname the user typed keeps portal and chat
// UI on one hostname — cookies ignore ports, not hosts — and no
// environment-specific address is baked anywhere. PORTAL_CHAT_UI_URL
// overrides for split deployments.
func (p *portal) chatUIURL(r *http.Request) string {
	if p.cfg.chatUIURL != "" {
		return p.cfg.chatUIURL
	}
	host := r.Host
	if withoutPort, _, err := net.SplitHostPort(r.Host); err == nil {
		host = withoutPort
	}
	return "http://" + host + ":30195/workspace"
}

// handleChatEntry serves the login + employee-picker page. Unauthenticated
// on purpose: the page itself carries no data, everything loads through the
// authenticated /api/v1/* endpoints.
func (p *portal) handleChatEntry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = chatEntryTemplate.Execute(w, map[string]any{"ChatURL": p.chatUIURL(r)})
}
