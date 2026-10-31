package main

import (
	"encoding/json"
	"strings"
)

// consolePage is the browser resource served at
// /v0/resource/plugins/webmcp/console. It registers navigator.modelContext tools
// so a WebMCP-capable agent driving this tab can start and finish CLIProxyAPI
// OAuth logins without a localhost callback port being reachable.
//
// The page is intentionally dependency-free: it must work from file://, from the
// CPA origin, and inside a WebMCP polyfill.
func consolePage(providers []string) string {
	encoded, errMarshal := json.Marshal(providers)
	if errMarshal != nil || len(providers) == 0 {
		encoded = []byte(`["codex"]`)
	}
	page := strings.ReplaceAll(consoleTemplate, "PROVIDER_LIST_PLACEHOLDER", string(encoded))
	return page
}

const consoleTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WebMCP · CLIProxyAPI OAuth</title>
<style>
  :root { color-scheme: light dark; --fg:#111; --bg:#fff; --mut:#666; --line:#ddd; --ok:#0a7; --err:#c33; }
  @media (prefers-color-scheme: dark) { :root { --fg:#eee; --bg:#161616; --mut:#999; --line:#333; --ok:#3d9; --err:#f66; } }
  body { font: 15px/1.5 ui-sans-serif, system-ui, sans-serif; margin: 0; padding: 24px; color: var(--fg); background: var(--bg); }
  main { max-width: 780px; margin: 0 auto; }
  h1 { font-size: 18px; margin: 0 0 4px; }
  p.sub { margin: 0 0 20px; color: var(--mut); font-size: 13px; }
  .row { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 12px; }
  button, select, input { font: inherit; padding: 8px 12px; border: 1px solid var(--line); border-radius: 8px; background: transparent; color: inherit; }
  button { cursor: pointer; }
  button.primary { border-color: var(--ok); color: var(--ok); }
  input { flex: 1 1 320px; }
  pre { border: 1px solid var(--line); border-radius: 8px; padding: 12px; overflow: auto; max-height: 340px; font-size: 12px; }
  .ok { color: var(--ok); } .err { color: var(--err); } .mut { color: var(--mut); }
  a.url { word-break: break-all; }
</style>
</head>
<body>
<main>
  <h1>WebMCP · CLIProxyAPI OAuth</h1>
  <p class="sub">Registers OAuth login tools on <code>navigator.modelContext</code>. Use the buttons by hand, or let an agent call the same tools.</p>

  <div class="row">
    <input id="key" type="password" placeholder="Management key (X-Management-Key) — stored in this browser only">
    <button id="save">Save key</button>
  </div>
  <div class="row">
    <select id="provider"></select>
    <button class="primary" id="start">Start login</button>
    <button id="wait">Wait for completion</button>
  </div>
  <div class="row">
    <input id="callback" placeholder="Paste the full redirect URL if the browser cannot reach the callback port">
    <button id="submit">Submit callback</button>
  </div>
  <p id="hint" class="mut">No login in progress.</p>
  <pre id="log"></pre>
</main>
<script>
(function () {
  "use strict";

  var ENDPOINT = "/v0/management/mcp";
  var PROVIDERS = PROVIDER_LIST_PLACEHOLDER;
  var state = null;

  var logEl = document.getElementById("log");
  var hintEl = document.getElementById("hint");
  var selectEl = document.getElementById("provider");

  function log(kind, payload) {
    var line = "[" + new Date().toLocaleTimeString() + "] " + kind + "\n" + JSON.stringify(payload, null, 2) + "\n\n";
    logEl.textContent = line + logEl.textContent;
  }

  function setHint(text, cls) {
    hintEl.textContent = text;
    hintEl.className = cls || "mut";
  }

  function storedKey() {
    try { return window.localStorage.getItem("cpa-mgmt-key") || ""; } catch (e) { return ""; }
  }

  function rpc(method, params) {
    var id = Math.floor(Math.random() * 1e9);
    var headers = { "Content-Type": "application/json", "MCP-Protocol-Version": "2025-11-25" };
    var key = storedKey();
    if (key) { headers["X-Management-Key"] = key; }
    return fetch(ENDPOINT, {
      method: "POST",
      headers: headers,
      credentials: "include",
      body: JSON.stringify({ jsonrpc: "2.0", id: id, method: method, params: params || {} })
    }).then(function (res) {
      if (res.status === 401 || res.status === 403) {
        throw new Error("management key missing or rejected — paste it above and press Save key");
      }
      return res.text().then(function (text) {
        if (!text) { return { ok: res.ok, empty: true, status: res.status }; }
        var parsed = JSON.parse(text);
        if (parsed.error) { throw new Error(parsed.error.message || "rpc error"); }
        return parsed.result;
      });
    });
  }

  function callTool(name, args) {
    return rpc("tools/call", { name: name, arguments: args || {} }).then(function (result) {
      var structured = result && result.structuredContent ? result.structuredContent : null;
      var text = result && result.content && result.content[0] ? result.content[0].text : "";
      if (result && result.isError) { throw new Error(text || "tool failed"); }
      if (structured) { return structured; }
      try { return JSON.parse(text); } catch (e) { return { text: text }; }
    });
  }

  function start(provider) {
    return callTool("oauth_login_start", { provider: provider }).then(function (out) {
      state = out.state;
      setHint("Login started for " + out.provider + " — state " + out.state, "ok");
      if (out.login_url) {
        window.open(out.login_url, "_blank", "noopener");
        logEl.textContent = "Login URL:\n" + out.login_url + "\n\n" + logEl.textContent;
      }
      return out;
    });
  }

  function wait(seconds) {
    if (!state) { return Promise.reject(new Error("start a login first")); }
    return callTool("oauth_login_wait", { state: state, timeout_seconds: seconds || 20 });
  }

  function submit(url) {
    var args = { callback_url: url };
    if (state) { args.state = state; }
    return callTool("oauth_login_submit", args);
  }

  document.getElementById("save").addEventListener("click", function () {
    var value = document.getElementById("key").value.trim();
    try {
      if (value) { window.localStorage.setItem("cpa-mgmt-key", value); } else { window.localStorage.removeItem("cpa-mgmt-key"); }
      setHint(value ? "Key saved in this browser." : "Key cleared.", "ok");
    } catch (e) { setHint("localStorage unavailable: " + e.message, "err"); }
  });

  var keyInput = document.getElementById("key");
  if (storedKey()) { keyInput.placeholder = "Management key saved (type to replace)"; }

  document.getElementById("start").addEventListener("click", function () {
    start(selectEl.value).then(function (out) { log("oauth_login_start", out); }).catch(function (err) { setHint(err.message, "err"); log("error", { message: err.message }); });
  });
  document.getElementById("wait").addEventListener("click", function () {
    wait(20).then(function (out) { log("oauth_login_wait", out); setHint(out.status === "ok" ? "Credential saved." : out.message, out.status === "ok" ? "ok" : "mut"); }).catch(function (err) { setHint(err.message, "err"); });
  });
  document.getElementById("submit").addEventListener("click", function () {
    var url = document.getElementById("callback").value.trim();
    if (!url) { setHint("Paste the redirect URL first.", "err"); return; }
    submit(url).then(function (out) { log("oauth_login_submit", out); setHint("Callback accepted.", "ok"); }).catch(function (err) { setHint(err.message, "err"); });
  });

  PROVIDERS.forEach(function (name) {
    var opt = document.createElement("option");
    opt.value = name; opt.textContent = name;
    selectEl.appendChild(opt);
  });

  // WebMCP: register the same tools an agent can call. Feature-detected so the
  // page still works in browsers without the API.
  var modelContext = navigator.modelContext;
  if (modelContext && typeof modelContext.provideContext === "function") {
    modelContext.provideContext({
      tools: [
        {
          name: "cpa_oauth_login_start",
          description: "Start a CLIProxyAPI OAuth login for a provider and return a login URL plus state.",
          inputSchema: { type: "object", properties: { provider: { type: "string", enum: PROVIDERS } }, required: ["provider"] },
          execute: function (args) { return start((args && args.provider) || "codex").then(function (out) { return { content: [{ type: "text", text: JSON.stringify(out) }] }; }); }
        },
        {
          name: "cpa_oauth_login_wait",
          description: "Wait for the started CLIProxyAPI OAuth login to complete.",
          inputSchema: { type: "object", properties: { timeout_seconds: { type: "integer", minimum: 1 } } },
          execute: function (args) { return wait((args && args.timeout_seconds) || 20).then(function (out) { return { content: [{ type: "text", text: JSON.stringify(out) }] }; }); }
        },
        {
          name: "cpa_oauth_login_submit",
          description: "Submit the redirect URL (or code) when the browser cannot reach the provider callback port.",
          inputSchema: { type: "object", properties: { callback_url: { type: "string" } }, required: ["callback_url"] },
          execute: function (args) { return submit((args && args.callback_url) || "").then(function (out) { return { content: [{ type: "text", text: JSON.stringify(out) }] }; }); }
        }
      ]
    });
    log("webmcp", { registered: 3 });
  } else {
    log("webmcp", { registered: 0, reason: "navigator.modelContext unavailable; buttons still work" });
  }

  rpc("initialize", { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "webmcp-console", version: "0.1.0" } })
    .then(function () { return rpc("tools/list", {}); })
    .then(function (out) { log("tools/list", (out.tools || []).map(function (t) { return t.name; })); })
    .catch(function (err) { log("error", { message: err.message }); setHint("Management credential required: open this page from the CPA origin or send X-Management-Key.", "err"); });
})();
</script>
</body>
</html>`
