# cliproxyapi-webmcp

An MCP server that lives **inside** [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
as a native plugin. It exposes the proxy's OAuth login flow as MCP tools, so an
agent can start a login, hand the user a URL, receive the callback, and confirm
the credential was saved — without anyone touching the management UI.

Two delivery surfaces, same code:

- **MCP over HTTP** at `POST /v0/management/mcp` (JSON-RPC 2.0, MCP
  `2025-11-25`, stateless Streamable HTTP — no SSE stream, no session header).
- **A browser console** at `/v0/resource/plugins/webmcp/console` that registers
  `navigator.modelContext` tools (WebMCP) and also works with plain buttons.

```
agent ──tools/call oauth_login_start──► webmcp plugin ──GET  /v8/management/oauth/auth-url──► CPA
      ◄──────── login_url + state ─────┘
user opens login_url, signs in, provider redirects to localhost:1455 (unreachable?)
agent ──tools/call oauth_login_submit─► webmcp plugin ──POST /v8/management/oauth/callback──► CPA
agent ──tools/call oauth_login_wait───► webmcp plugin ──GET  /v8/management/oauth/status──► CPA
      ◄──────── status=ok (credential saved) ─────────────────────────────────────────────┘
```

## Why a plugin and not a separate MCP server

The plugin runs in the host process, so `127.0.0.1:<port>` is always reachable
and the OAuth session table is the host's own. Nothing is reimplemented: the
plugin is a thin, credential-forwarding MCP front end over the management API
that already exists. No PKCE duplication, no second credential store, no port
of its own.

## Tools

| Tool | What it does |
| --- | --- |
| `oauth_login_start` | `GET /v8/management/oauth/auth-url?provider=…` → `login_url`, `state`, `flow`. Device-flow providers also return a user code inside the URL. |
| `oauth_login_wait` | Long-polls `GET /v8/management/oauth/status` until `ok` / `error` / timeout. Returns `status=wait` when the budget runs out so the agent can call again. |
| `oauth_login_submit` | Posts to `/v8/management/oauth/callback`. Accepts either `callback_url` (a full pasted redirect URL; `state` and `code` are extracted) or `state` + `code`. |
| `oauth_login_cancel` | `DELETE /v8/management/oauth/session` so no credential is saved. |
| `list_providers` | The advertised provider names plus the default. |
| `list_credentials` | Saved credentials: name, provider, status, plan, expiry. **Never returns tokens.** |
| `cpa_status` | Plugin/host schema versions, loopback base URL, where the management credential came from. First thing to call when authorization fails. |

A failed login (`status=error`) is returned with `isError: true` as well as the
message, so an agent stops retrying instead of parsing payloads.

## Install

1. Download the archive for your platform and drop the `.so` in `plugins.dir`:

   ```bash
   tar -xzf webmcp_0.1.0_linux_amd64.tar.gz -C ./plugins/
   ls ./plugins/webmcp.so
   ```

   **The file name is the plugin ID.** It must be `webmcp.so`; a name like
   `webmcp-go.so` registers a second, separate plugin called `webmcp-go` that
   ignores your `webmcp:` config block.

2. Configure it (see `config.example.yaml`):

   ```yaml
   plugins:
     enabled: true
     dir: "./plugins"
     configs:
       webmcp:
         enabled: true
         priority: 10
         management_base_url: "http://127.0.0.1:8317"   # the port CPA listens on
         management_key: ""                              # empty = forward the caller's key
   ```

3. Restart CPA and confirm registration:

   ```bash
   curl -s localhost:8317/v8/management/plugins -H "X-Management-Key: $KEY" \
     | jq '.plugins[] | select(.id=="webmcp") | {id, registered, effective_enabled}'
   ```

## Wiring it into an MCP client

The endpoint is plain Streamable HTTP with no SSE and no session, so any client
that supports a static URL works:

```json
{
  "mcpServers": {
    "cpa-oauth": {
      "type": "http",
      "url": "http://127.0.0.1:8317/v0/management/mcp",
      "headers": { "X-Management-Key": "<your management key>" }
    }
  }
}
```

The credential can come from three places, in this order: the caller's
`X-Management-Key` / `Authorization: Bearer` header, the plugin's
`management_key` config, or nothing (in which case calls fail with a clear
message). For a remote client, `allow-remote: true` plus a management key is
required — the plugin itself adds no authentication of its own.

### The callback-port problem, and how this solves it

Codex redirects to `localhost:1455`, Claude to `localhost:54545`, Antigravity to
`localhost:51121`. If the browser is not on the same machine as CPA, that
redirect dead-ends. `oauth_login_submit` exists exactly for this: paste the
whole redirect URL (even the browser's error page URL) and the plugin extracts
`state`/`code` and posts them to the host's callback endpoint.

```
oauth_login_submit { "callback_url": "http://localhost:1455/auth/callback?code=...&state=..." }
```

## Verified behaviour

Every row below was run against a real `cli-proxy-api` 8.0.46 binary with the
published `.so` loaded, not against mocks.

| Scenario | Result |
| --- | --- |
| Plugin load | `plugin registered plugin_id=webmcp version=0.1.0`; `/v8/management/plugins` shows `registered: true` |
| `initialize` | `protocolVersion: 2025-11-25`, `serverInfo.name: cpa-webmcp` |
| `tools/list` | 7 tools, every one with an object `inputSchema` |
| `notifications/initialized` | HTTP 202, empty body |
| Wrong `X-Management-Key` | HTTP 401 from the host before the plugin runs |
| `oauth_login_start` (codex) | returns a live `auth.openai.com/oauth/authorize` URL + state |
| `oauth_login_start` (xai) | `flow: device`, device URL with `user_code`, `expires_in: 1800` |
| `oauth_login_wait` while pending | `status: wait`, "still pending after 3s" |
| `oauth_login_submit` with a bad code | accepted, then `oauth_login_wait` → `status: error` with the provider's `token_expired` body, `isError: true` |
| `oauth_login_submit` with `callback_url` | `state`/`code` extracted from the URL, POST body carries both |
| `oauth_login_cancel` (xai device) | `cancelled: true`, later `wait` → `unknown or expired state` |
| `list_providers` | 8 providers; `anthropic`/`grok`/`openai` correctly absent (host 404s them) |
| `list_credentials` on an empty instance | `count: 0` |
| Console resource | HTTP 200 `text/html`, `navigator.modelContext` registration present, provider list injected |
| `GET /v0/management/mcp` | HTTP 405 + `Allow: POST` (no standalone SSE stream) |

`go test -race` covers the loopback client, callback-URL parsing, wait
semantics, JSON-RPC framing (initialize, notifications, batching, parse errors,
unknown methods), tool-error propagation, and loose YAML decoding.

## Limitations

- **No authentication of its own.** It reuses the host management key; if
  `allow-remote` is on, anyone with that key can start logins. Treat the MCP URL
  as a management endpoint.
- **Stateless HTTP only.** No SSE stream, no `Mcp-Session-Id`. That is valid for
  MCP `2025-11-25`, but a client that insists on opening a GET SSE stream will
  see 405.
- **Loopback assumptions.** `management_base_url` must be reachable *from the
  CPA process*. That is why it defaults to `127.0.0.1`; pointing it at a public
  URL works but adds a network hop and a second auth surface.
- **Provider list is a snapshot.** `providers` defaults to the names a live
  8.0.46 host answers; a newer host may add providers. `oauth_login_start` passes
  the name through, so an unlisted provider still works if the host knows it.
- **Device-flow codes are not scraped.** For `xai`/`kimi`/`meta` the user code is
  inside the returned URL; the plugin does not parse it into a separate field.
- **`list_credentials` is a projection.** Only name/provider/status/plan/expiry
  are exposed. Tokens stay in the host auth dir.
- **No refresh or quota tooling.** Those are separate host endpoints; this plugin
  deliberately stays on the login path.

## Build

```bash
make linux     # bin/webmcp.so
make check     # gofmt + vet + go test -race
```

Needs Go 1.26+ and a C toolchain (cgo, `-buildmode=c-shared`). CI builds
linux/amd64 and linux/arm64 (`CC=aarch64-linux-gnu-gcc`). The release workflow
packages each platform so the archive always contains a file named `webmcp.so`.

## License

MIT
