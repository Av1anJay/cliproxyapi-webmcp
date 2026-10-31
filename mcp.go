package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	mcpProtocolVersion   = "2025-11-25"
	mcpManagementPath    = "/mcp"
	mcpResourcePath      = "/docs"
	mcpServerName        = "cpa-webmcp"
	mcpServerVersion     = pluginVersion
	mcpMaxBodyBytes      = 4 << 20
	mcpDefaultWaitSecond = 20
)

// jsonrpcRequest is the subset of JSON-RPC 2.0 the MCP Streamable HTTP
// transport needs. Notifications carry no ID and expect no response body.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// callerCredential extracts whatever management credential the MCP client sent,
// so the plugin never needs its own copy when the client already has one.
func callerCredential(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if raw := strings.TrimSpace(headers.Get("X-Management-Key")); raw != "" {
		return raw
	}
	auth := strings.TrimSpace(headers.Get("Authorization"))
	if auth == "" {
		return ""
	}
	if parts := strings.SplitN(auth, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	return auth
}

// handleMCP serves one JSON-RPC message. It returns (body, nil) for requests and
// (nil, nil) for notifications, which must produce an empty 202.
func handleMCP(ctx context.Context, cfg pluginConfig, headers http.Header, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty request body")
	}
	if len(raw) > mcpMaxBodyBytes {
		return nil, fmt.Errorf("request body too large")
	}

	// MCP clients may batch. Handle both shapes.
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var batch []jsonrpcRequest
		if errUnmarshal := json.Unmarshal(raw, &batch); errUnmarshal != nil {
			return encodeRPC(jsonrpcResponse{JSONRPC: "2.0", Error: &jsonrpcError{Code: -32700, Message: "parse error"}}), nil
		}
		out := make([]jsonrpcResponse, 0, len(batch))
		for _, item := range batch {
			if resp, ok := dispatchRPC(ctx, cfg, headers, item); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		encoded, errMarshal := json.Marshal(out)
		if errMarshal != nil {
			return nil, errMarshal
		}
		return encoded, nil
	}

	var req jsonrpcRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return encodeRPC(jsonrpcResponse{JSONRPC: "2.0", Error: &jsonrpcError{Code: -32700, Message: "parse error"}}), nil
	}
	resp, ok := dispatchRPC(ctx, cfg, headers, req)
	if !ok {
		return nil, nil
	}
	return encodeRPC(resp), nil
}

func encodeRPC(resp jsonrpcResponse) []byte {
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		return []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal error"}}`)
	}
	return raw
}

func dispatchRPC(ctx context.Context, cfg pluginConfig, headers http.Header, req jsonrpcRequest) (jsonrpcResponse, bool) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	reply := func(result any) (jsonrpcResponse, bool) {
		if isNotification {
			return jsonrpcResponse{}, false
		}
		return jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	}
	fail := func(code int, message string, data any) (jsonrpcResponse, bool) {
		if isNotification {
			return jsonrpcResponse{}, false
		}
		return jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &jsonrpcError{Code: code, Message: message, Data: data}}, true
	}

	switch req.Method {
	case "initialize":
		return reply(map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    mcpServerName,
				"version": mcpServerVersion,
			},
			"instructions": "CLIProxyAPI OAuth bridge. Call oauth_login_start to get a login URL, hand it to the user, then oauth_login_wait (or oauth_login_submit when the browser cannot reach the callback port).",
		})
	case "notifications/initialized", "notifications/cancelled":
		return jsonrpcResponse{}, false
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		return reply(map[string]any{"tools": toolDefinitions(cfg)})
	case "tools/call":
		return callTool(ctx, cfg, headers, req, reply, fail)
	case "resources/list":
		return reply(map[string]any{"resources": []any{}})
	case "prompts/list":
		return reply(map[string]any{"prompts": []any{}})
	default:
		return fail(-32601, "method not found: "+req.Method, nil)
	}
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func callTool(
	ctx context.Context,
	cfg pluginConfig,
	headers http.Header,
	req jsonrpcRequest,
	reply func(any) (jsonrpcResponse, bool),
	fail func(int, string, any) (jsonrpcResponse, bool),
) (jsonrpcResponse, bool) {
	var params toolCallParams
	if len(req.Params) > 0 {
		if errUnmarshal := json.Unmarshal(req.Params, &params); errUnmarshal != nil {
			return fail(-32602, "invalid params", errUnmarshal.Error())
		}
	}
	if strings.TrimSpace(params.Name) == "" {
		return fail(-32602, "tool name is required", nil)
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}

	if !bool(cfg.Enabled) {
		return reply(toolError("webmcp plugin is disabled in plugins.configs.webmcp.enabled"))
	}

	client := newCPAClient(cfg, callerCredential(headers))
	result, errRun := runTool(ctx, cfg, client, params.Name, args)
	if errRun != nil {
		if errIsUnauthorized(errRun) {
			return reply(toolError("management credential rejected: set plugins.configs.webmcp.management_key or send X-Management-Key / Authorization: Bearer from the MCP client"))
		}
		return reply(toolError(errRun.Error()))
	}
	out := toolResult(result)
	if resultFailed(result) {
		// Surface a failed login as a tool error so agents stop retrying blindly.
		out["isError"] = true
	}
	return reply(out)
}

// resultFailed reports whether a tool result carries status=error, so agents do
// not have to parse the payload to notice a failed login.
func resultFailed(payload any) bool {
	fields, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	status, _ := fields["status"].(string)
	return status == oauthStatusErr
}

func errIsUnauthorized(err error) bool {
	return err != nil && strings.Contains(err.Error(), errManagementUnauthorized.Error())
}

func runTool(ctx context.Context, cfg pluginConfig, client *cpaClient, name string, args map[string]any) (any, error) {
	switch name {
	case "oauth_login_start":
		provider := providerArg(cfg, args)
		start, errStart := client.oauthStart(ctx, provider)
		if errStart != nil {
			return nil, errStart
		}
		out := map[string]any{
			"provider":  provider,
			"state":     start.State,
			"login_url": start.URL,
			"flow":      firstNonEmpty(start.Flow, "callback"),
			"next":      "Open login_url, finish sign-in, then call oauth_login_wait with this state.",
		}
		if start.Flow == "device" {
			out["next"] = "Open login_url and enter the displayed code, then call oauth_login_wait with this state."
			if start.ExpiresIn > 0 {
				out["expires_in"] = start.ExpiresIn
			}
		}
		if start.Flow != "device" {
			out["callback_hint"] = "The provider redirects to a localhost port. If that port is unreachable from the browser, copy the full redirect URL and pass it to oauth_login_submit as callback_url."
		}
		return out, nil

	case "oauth_login_wait":
		state := strings.TrimSpace(stringArg(args, "state"))
		if state == "" {
			return nil, fmt.Errorf("state is required")
		}
		seconds := intArg(args, "timeout_seconds", mcpDefaultWaitSecond)
		if seconds <= 0 {
			seconds = mcpDefaultWaitSecond
		}
		if seconds > int(cfg.MaxWaitSeconds) {
			seconds = int(cfg.MaxWaitSeconds)
		}
		status, errStatus := client.waitForCompletion(ctx, state, time.Duration(seconds)*time.Second)
		if errStatus != nil {
			return nil, errStatus
		}
		out := map[string]any{"state": state, "status": status.Status}
		switch status.Status {
		case oauthStatusOK:
			out["message"] = "Login completed; credential saved by CLIProxyAPI."
		case oauthStatusErr:
			out["message"] = firstNonEmpty(status.Error, "login failed")
		default:
			out["message"] = fmt.Sprintf("still pending after %ds; call oauth_login_wait again", seconds)
		}
		return out, nil

	case "oauth_login_submit":
		state := strings.TrimSpace(stringArg(args, "state"))
		code := strings.TrimSpace(stringArg(args, "code"))
		redirect := strings.TrimSpace(stringArg(args, "callback_url"))
		provider := strings.TrimSpace(stringArg(args, "provider"))
		if redirect != "" {
			if !bool(cfg.AllowCallbackURL) {
				return nil, fmt.Errorf("callback_url is disabled by allow_callback_url=false")
			}
			parsed, errParse := url.Parse(redirect)
			if errParse != nil {
				return nil, fmt.Errorf("invalid callback_url: %w", errParse)
			}
			query := parsed.Query()
			if state == "" {
				state = strings.TrimSpace(query.Get("state"))
			}
			if code == "" {
				code = strings.TrimSpace(query.Get("code"))
			}
			if provider == "" {
				provider = strings.TrimSpace(query.Get("provider"))
			}
			if errParam := strings.TrimSpace(query.Get("error")); errParam != "" {
				return nil, fmt.Errorf("provider returned error: %s", errParam)
			}
		}
		if state == "" {
			return nil, fmt.Errorf("state is required (or pass callback_url containing state)")
		}
		if code == "" {
			return nil, fmt.Errorf("code is required (or pass callback_url containing code)")
		}
		payload, errSubmit := client.oauthCallback(ctx, provider, state, code, "")
		if errSubmit != nil {
			return nil, errSubmit
		}
		return map[string]any{
			"status":  firstNonEmpty(stringArg(payload, "status"), "ok"),
			"state":   state,
			"message": "Callback accepted; call oauth_login_wait to confirm the credential was saved.",
		}, nil

	case "oauth_login_cancel":
		state := strings.TrimSpace(stringArg(args, "state"))
		if state == "" {
			return nil, fmt.Errorf("state is required")
		}
		payload, errCancel := client.oauthCancel(ctx, state)
		if errCancel != nil {
			return nil, errCancel
		}
		return map[string]any{
			"status":    firstNonEmpty(stringArg(payload, "status"), "ok"),
			"state":     state,
			"cancelled": payload["cancelled"],
		}, nil

	case "list_providers":
		return map[string]any{
			"providers":        cfg.Providers,
			"default_provider": cfg.DefaultProvider,
			"note":             "Provider names accepted by GET /v8/management/oauth/auth-url on this host.",
		}, nil

	case "list_accounts":
		filter := accountFilter{
			Provider:        strings.ToLower(strings.TrimSpace(stringArg(args, "provider"))),
			Status:          strings.ToLower(strings.TrimSpace(stringArg(args, "status"))),
			IncludeDisabled: boolArg(args, "include_disabled", true),
		}
		items, errList := client.accountsList(ctx, filter)
		if errList != nil {
			return nil, errList
		}
		out := map[string]any{
			"count":    len(items),
			"totals":   summarizeAccounts(items),
			"accounts": items,
		}
		if len(items) == 0 {
			out["message"] = "No accounts match. Call oauth_login_start to add one."
		}
		return out, nil

	case "list_credentials":
		items, errList := client.accountsList(ctx, accountFilter{IncludeDisabled: true})
		if errList != nil {
			return nil, errList
		}
		// Backwards-compatible shape: the older tool only promised a flat list.
		legacy := make([]map[string]any, 0, len(items))
		for _, item := range items {
			legacy = append(legacy, map[string]any{
				"name":      item.Name,
				"provider":  item.Provider,
				"status":    item.Status,
				"disabled":  item.Disabled,
				"plan_type": item.PlanType,
				"expired":   item.NextRetryAfter,
			})
		}
		return map[string]any{
			"count":       len(items),
			"credentials": legacy,
			"note":        "Deprecated: use list_accounts for health, cooldowns and totals.",
		}, nil

	case "cpa_status":
		return map[string]any{
			"plugin":                pluginID,
			"plugin_version":        pluginVersion,
			"plugin_schema_version": currentSchemaVersion(),
			"host_schema_version":   hostSchemaVersion(),
			"management_base_url":   cfg.ManagementBaseURL,
			"management_key_source": managementKeySource(cfg),
			"enabled":               bool(cfg.Enabled),
		}, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func managementKeySource(cfg pluginConfig) string {
	if strings.TrimSpace(cfg.ManagementKey) != "" {
		return "plugin config"
	}
	return "caller header"
}

func providerArg(cfg pluginConfig, args map[string]any) string {
	provider := strings.ToLower(strings.TrimSpace(stringArg(args, "provider")))
	if provider == "" {
		provider = cfg.DefaultProvider
	}
	return provider
}

func stringArg(args map[string]any, name string) string {
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func intArg(args map[string]any, name string, fallback int) int {
	value, ok := args[name]
	if !ok || value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(typed))
		if errParse != nil {
			return fallback
		}
		return parsed
	default:
		return fallback
	}
}

func boolArg(args map[string]any, name string, fallback bool) bool {
	value, ok := args[name]
	if !ok || value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "yes", "1", "on":
			return true
		case "false", "no", "0", "off":
			return false
		}
		return fallback
	case float64:
		return typed != 0
	default:
		return fallback
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// toolResult renders a successful tool call. structuredContent keeps the raw
// object available to clients that want to branch on it.
func toolResult(payload any) map[string]any {
	raw, errMarshal := json.MarshalIndent(payload, "", "  ")
	text := "{}"
	if errMarshal == nil {
		text = string(raw)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": payload,
		"isError":           false,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}
