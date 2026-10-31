package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fakeCPA is a stand-in for a real CLIProxyAPI management API. It records what
// the plugin asked for so tests can assert the exact request shape.
type fakeCPA struct {
	server    *httptest.Server
	requests  []recordedRequest
	statusSeq []string
	files     []map[string]any
	calls     atomic.Int64
}

// defaultFakeFiles mirrors the real /v8/management/credentials payload shape,
// including the id_token-nested plan type, cooldowns and codex SIWC fields.
var defaultFakeFiles = []map[string]any{
	{
		"name":                "codex-a@b.com-plus.json",
		"provider":            "codex",
		"type":                "codex",
		"status":              "active",
		"email":               "acct@example.com",
		"account":             "acct@example.com",
		"account_type":        "oauth",
		"priority":            7,
		"success":             12,
		"failed":              1,
		"id_token":            map[string]any{"plan_type": "plus"},
		"siwc_bind_supported": true,
		"siwc_bound":          false,
		"siwc_status":         "off",
		"use_siwc":            false,
		"cooldowns": []any{
			map[string]any{"scope": "model", "model_key": "gpt-5", "reason": "rate_limit", "remaining_seconds": 42},
		},
	},
	{
		"name":     "xai-b.json",
		"provider": "xai",
		"type":     "xai",
		"status":   "disabled",
		"disabled": true,
	},
}

type recordedRequest struct {
	Method string
	Path   string
	Query  map[string][]string
	Key    string
	Body   map[string]any
}

func newFakeCPA(t *testing.T) *fakeCPA {
	t.Helper()
	fake := &fakeCPA{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v8/management/oauth/auth-url", func(w http.ResponseWriter, r *http.Request) {
		fake.record(r)
		if r.Header.Get("X-Management-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid management key"}`))
			return
		}
		provider := r.URL.Query().Get("provider")
		switch provider {
		case "codex":
			_, _ = w.Write([]byte(`{"status":"ok","state":"st-1","url":"https://auth.example/oauth?x=1"}`))
		case "xai":
			_, _ = w.Write([]byte(`{"status":"ok","state":"st-device","url":"https://accounts.example/device?user_code=AB-12","flow":"device","expires_in":1800}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"provider_not_found"}`))
		}
	})
	mux.HandleFunc("/v8/management/oauth/status", func(w http.ResponseWriter, r *http.Request) {
		fake.record(r)
		index := int(fake.calls.Add(1)) - 1
		status := "wait"
		if index < len(fake.statusSeq) {
			status = fake.statusSeq[index]
		}
		switch status {
		case "ok":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "error":
			_, _ = w.Write([]byte(`{"status":"error","error":"Failed to exchange authorization code for tokens"}`))
		default:
			_, _ = w.Write([]byte(`{"status":"wait"}`))
		}
	})
	mux.HandleFunc("/v8/management/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		fake.record(r)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v8/management/oauth/session", func(w http.ResponseWriter, r *http.Request) {
		fake.record(r)
		_, _ = w.Write([]byte(`{"status":"ok","cancelled":true}`))
	})
	mux.HandleFunc("/v8/management/credentials", func(w http.ResponseWriter, r *http.Request) {
		fake.record(r)
		files := fake.files
		if files == nil {
			files = defaultFakeFiles
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeCPA) record(r *http.Request) {
	entry := recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Key:    r.Header.Get("X-Management-Key"),
	}
	if r.Body != nil {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		entry.Body = body
	}
	f.requests = append(f.requests, entry)
}

func (f *fakeCPA) last() recordedRequest {
	if len(f.requests) == 0 {
		return recordedRequest{}
	}
	return f.requests[len(f.requests)-1]
}

func testConfig(t *testing.T, baseURL string) pluginConfig {
	t.Helper()
	cfg := defaultConfig()
	cfg.ManagementBaseURL = baseURL
	cfg.ManagementKey = "test-key"
	cfg.TimeoutSeconds = 5
	return cfg.normalized()
}

func TestOAuthLoginStartReturnsURLAndState(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	start, errStart := client.oauthStart(context.Background(), "codex")
	if errStart != nil {
		t.Fatalf("oauthStart: %v", errStart)
	}
	if start.State != "st-1" || !strings.Contains(start.URL, "auth.example") {
		t.Fatalf("unexpected start result: %+v", start)
	}
	got := fake.last()
	if got.Path != "/v8/management/oauth/auth-url" || got.Query["provider"][0] != "codex" {
		t.Fatalf("unexpected upstream call: %+v", got)
	}
	if got.Key != "test-key" {
		t.Fatalf("management key not forwarded, got %q", got.Key)
	}
}

func TestOAuthLoginStartPropagatesProviderError(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	if _, errStart := client.oauthStart(context.Background(), "nope"); errStart == nil {
		t.Fatal("expected provider_not_found to surface as an error")
	}
}

func TestCallerCredentialIsForwardedWhenConfigHasNoKey(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	cfg.ManagementKey = ""
	client := newCPAClient(cfg, "caller-key")

	if _, errStart := client.oauthStart(context.Background(), "codex"); errStart == nil {
		t.Fatal("expected unauthorized when the forwarded key is wrong")
	}
	if got := fake.last().Key; got != "caller-key" {
		t.Fatalf("caller key not forwarded, got %q", got)
	}
}

func TestUnauthorizedIsRecognisable(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	cfg.ManagementKey = "wrong"
	client := newCPAClient(cfg, "")

	_, errStart := client.oauthStart(context.Background(), "codex")
	if errStart == nil || !errIsUnauthorized(errStart) {
		t.Fatalf("expected errIsUnauthorized, got %v", errStart)
	}
}

func TestWaitPollsUntilCompletion(t *testing.T) {
	fake := newFakeCPA(t)
	fake.statusSeq = []string{"wait", "wait", "ok"}
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	status, errWait := client.waitForCompletion(context.Background(), "st-1", 10*time.Second)
	if errWait != nil {
		t.Fatalf("waitForCompletion: %v", errWait)
	}
	if status.Status != oauthStatusOK {
		t.Fatalf("status = %q, want ok", status.Status)
	}
	if fake.calls.Load() < 3 {
		t.Fatalf("polled %d times, want at least 3", fake.calls.Load())
	}
}

func TestWaitReportsHostError(t *testing.T) {
	fake := newFakeCPA(t)
	fake.statusSeq = []string{"error"}
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	status, errWait := client.waitForCompletion(context.Background(), "st-1", 5*time.Second)
	if errWait != nil {
		t.Fatalf("waitForCompletion: %v", errWait)
	}
	if status.Status != oauthStatusErr || !strings.Contains(status.Error, "exchange") {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestWaitTimesOutAsPending(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	status, errWait := client.waitForCompletion(context.Background(), "st-1", 1200*time.Millisecond)
	if errWait != nil {
		t.Fatalf("waitForCompletion: %v", errWait)
	}
	if status.Status != oauthStatusWait {
		t.Fatalf("status = %q, want wait", status.Status)
	}
}

func TestCallbackURLParsing(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	if _, errSubmit := client.oauthCallback(context.Background(), "", "st-9", "code-9", ""); errSubmit != nil {
		t.Fatalf("oauthCallback: %v", errSubmit)
	}
	got := fake.last()
	if got.Method != http.MethodPost || got.Body["code"] != "code-9" || got.Body["state"] != "st-9" {
		t.Fatalf("unexpected callback body: %+v", got)
	}
	if _, exists := got.Body["provider"]; exists {
		t.Fatalf("empty provider must be omitted, got %+v", got.Body)
	}
}

func TestToolSubmitAcceptsFullRedirectURL(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	out, errRun := runTool(context.Background(), cfg, client, "oauth_login_submit", map[string]any{
		"callback_url": "http://localhost:1455/auth/callback?code=abc&state=st-42",
	})
	if errRun != nil {
		t.Fatalf("runTool: %v", errRun)
	}
	payload, _ := out.(map[string]any)
	if payload["state"] != "st-42" {
		t.Fatalf("state not extracted from URL: %+v", payload)
	}
	got := fake.last()
	if got.Body["code"] != "abc" || got.Body["state"] != "st-42" {
		t.Fatalf("unexpected callback body: %+v", got.Body)
	}
}

func TestToolSubmitRejectsProviderError(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	_, errRun := runTool(context.Background(), cfg, client, "oauth_login_submit", map[string]any{
		"callback_url": "http://localhost:1455/auth/callback?error=access_denied&state=st-42",
	})
	if errRun == nil || !strings.Contains(errRun.Error(), "access_denied") {
		t.Fatalf("expected provider error to surface, got %v", errRun)
	}
}

func TestToolSubmitRequiresStateAndCode(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	if _, errRun := runTool(context.Background(), cfg, client, "oauth_login_submit", map[string]any{}); errRun == nil {
		t.Fatal("expected missing state to fail")
	}
	if _, errRun := runTool(context.Background(), cfg, client, "oauth_login_submit", map[string]any{"state": "s"}); errRun == nil {
		t.Fatal("expected missing code to fail")
	}
}

func TestListAccountsProjectsHealthFields(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	items, errList := client.accountsList(context.Background(), accountFilter{IncludeDisabled: true})
	if errList != nil {
		t.Fatalf("accountsList: %v", errList)
	}
	if len(items) != 2 {
		t.Fatalf("got %d accounts, want 2", len(items))
	}
	codex := items[0]
	if codex.Provider != "codex" || codex.Email != "acct@example.com" || codex.PlanType != "plus" {
		t.Fatalf("codex projection wrong: %+v", codex)
	}
	if !codex.Usable || codex.Disabled {
		t.Fatalf("codex should be usable: %+v", codex)
	}
	if codex.Priority == nil || *codex.Priority != 7 {
		t.Fatalf("priority not projected: %+v", codex.Priority)
	}
	if len(codex.Cooldowns) != 1 || codex.Cooldowns[0].Scope != "model" || codex.Cooldowns[0].RemainingSeconds != 42 {
		t.Fatalf("cooldowns not projected: %+v", codex.Cooldowns)
	}
	if codex.SIWC == nil || !codex.SIWC.Supported || codex.SIWC.Bound || codex.SIWC.Status != "off" {
		t.Fatalf("siwc not projected: %+v", codex.SIWC)
	}

	xai := items[1]
	if xai.Provider != "xai" || xai.Usable {
		t.Fatalf("xai should be disabled/unusable: %+v", xai)
	}
	if xai.SIWC != nil {
		t.Fatalf("non-codex accounts must not carry siwc state: %+v", xai.SIWC)
	}
}

func TestListAccountsFilters(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	byProvider, _ := client.accountsList(context.Background(), accountFilter{Provider: "xai", IncludeDisabled: true})
	if len(byProvider) != 1 || byProvider[0].Provider != "xai" {
		t.Fatalf("provider filter failed: %+v", byProvider)
	}

	noDisabled, _ := client.accountsList(context.Background(), accountFilter{IncludeDisabled: false})
	if len(noDisabled) != 1 || noDisabled[0].Provider != "codex" {
		t.Fatalf("include_disabled=false failed: %+v", noDisabled)
	}

	byStatus, _ := client.accountsList(context.Background(), accountFilter{Status: "disabled", IncludeDisabled: true})
	if len(byStatus) != 1 || byStatus[0].Provider != "xai" {
		t.Fatalf("status filter failed: %+v", byStatus)
	}
}

func TestSummarizeAccounts(t *testing.T) {
	totals := summarizeAccounts([]accountSummary{
		{Provider: "codex", Status: "active", Usable: true},
		{Provider: "codex", Status: "disabled", Disabled: true},
		{Provider: "xai", Status: "active", Unavailable: true},
	})
	if totals.Total != 3 || totals.Usable != 1 || totals.Disabled != 1 || totals.Unavailable != 1 {
		t.Fatalf("bad totals: %+v", totals)
	}
	if totals.ByProvider["codex"] != 2 || totals.ByStatus["active"] != 2 {
		t.Fatalf("bad group counts: %+v", totals)
	}
}

func TestToolListAccountsReturnsTotals(t *testing.T) {
	fake := newFakeCPA(t)
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	out, errRun := runTool(context.Background(), cfg, client, "list_accounts", map[string]any{})
	if errRun != nil {
		t.Fatalf("runTool: %v", errRun)
	}
	payload, _ := out.(map[string]any)
	if payload["count"] != 2 {
		t.Fatalf("count = %v, want 2", payload["count"])
	}
	totals, ok := payload["totals"].(accountTotals)
	if !ok || totals.ByProvider["codex"] != 1 {
		t.Fatalf("totals missing: %+v", payload["totals"])
	}
	if _, exists := payload["message"]; exists {
		t.Fatalf("message must only appear for an empty result: %+v", payload)
	}
}

func TestToolListAccountsEmptyIsNotAnError(t *testing.T) {
	fake := newFakeCPA(t)
	fake.files = []map[string]any{}
	cfg := testConfig(t, fake.server.URL)
	client := newCPAClient(cfg, "")

	out, errRun := runTool(context.Background(), cfg, client, "list_accounts", map[string]any{})
	if errRun != nil {
		t.Fatalf("runTool: %v", errRun)
	}
	payload, _ := out.(map[string]any)
	if payload["count"] != 0 {
		t.Fatalf("count = %v, want 0", payload["count"])
	}
	if _, exists := payload["message"]; !exists {
		t.Fatal("empty result should tell the agent how to add an account")
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	cfg := defaultConfig().normalized()
	body, errHandle := handleMCP(context.Background(), cfg, http.Header{}, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if errHandle != nil {
		t.Fatalf("handleMCP: %v", errHandle)
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(body, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.Result.ProtocolVersion != mcpProtocolVersion || resp.Result.ServerInfo.Name != mcpServerName {
		t.Fatalf("unexpected initialize result: %s", body)
	}

	body, _ = handleMCP(context.Background(), cfg, http.Header{}, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	var listed struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(body, &listed); errUnmarshal != nil {
		t.Fatalf("decode tools/list: %v", errUnmarshal)
	}
	if len(listed.Result.Tools) < 7 {
		t.Fatalf("expected at least 7 tools, got %d", len(listed.Result.Tools))
	}
	for _, tool := range listed.Result.Tools {
		if tool.Name == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %q has no object inputSchema", tool.Name)
		}
	}
}

func TestMCPNotificationReturnsNoBody(t *testing.T) {
	cfg := defaultConfig().normalized()
	body, errHandle := handleMCP(context.Background(), cfg, http.Header{}, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if errHandle != nil {
		t.Fatalf("handleMCP: %v", errHandle)
	}
	if body != nil {
		t.Fatalf("notification must not produce a body, got %s", body)
	}
}

func TestMCPBatchIsAnsweredInOrder(t *testing.T) {
	cfg := defaultConfig().normalized()
	body, errHandle := handleMCP(context.Background(), cfg, http.Header{},
		[]byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`))
	if errHandle != nil {
		t.Fatalf("handleMCP: %v", errHandle)
	}
	var out []struct {
		ID json.RawMessage `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(body, &out); errUnmarshal != nil {
		t.Fatalf("decode batch: %v", errUnmarshal)
	}
	if len(out) != 2 || string(out[0].ID) != "1" || string(out[1].ID) != "2" {
		t.Fatalf("unexpected batch order: %s", body)
	}
}

func TestMCPParseErrorIsJSONRPC(t *testing.T) {
	cfg := defaultConfig().normalized()
	body, _ := handleMCP(context.Background(), cfg, http.Header{}, []byte(`{not json`))
	var resp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.Error.Code != -32700 {
		t.Fatalf("code = %d, want -32700", resp.Error.Code)
	}
}

func TestMCPUnknownMethodIsMethodNotFound(t *testing.T) {
	cfg := defaultConfig().normalized()
	body, _ := handleMCP(context.Background(), cfg, http.Header{}, []byte(`{"jsonrpc":"2.0","id":7,"method":"resources/read"}`))
	var resp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &resp)
	if resp.Error.Code != -32601 {
		t.Fatalf("code = %d, want -32601", resp.Error.Code)
	}
}

func TestMCPDisabledPluginReportsToolError(t *testing.T) {
	cfg := defaultConfig().normalized()
	cfg.Enabled = flexBool(false)
	body, errHandle := handleMCP(context.Background(), cfg, http.Header{},
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_providers"}}`))
	if errHandle != nil {
		t.Fatalf("handleMCP: %v", errHandle)
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(body, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if !resp.Result.IsError {
		t.Fatalf("disabled plugin must return a tool error, got %s", body)
	}
}

func TestMCPWaitFailureSetsIsError(t *testing.T) {
	fake := newFakeCPA(t)
	fake.statusSeq = []string{"error"}
	cfg := testConfig(t, fake.server.URL)
	raw := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"oauth_login_wait","arguments":{"state":"st-1","timeout_seconds":1}}}`)
	body, errHandle := handleMCP(context.Background(), cfg, http.Header{}, raw)
	if errHandle != nil {
		t.Fatalf("handleMCP: %v", errHandle)
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatalf("failed login must set isError, got %s", body)
	}
}

func TestConfigParsesLooseYAML(t *testing.T) {
	raw := []byte(`
enabled: "false"
management_base_url: "http://127.0.0.1:9000/"
management_key: ""
timeout_seconds: "45"
default_provider: "XAI"
allow_callback_url: "no"
max_wait_seconds: 30
providers: ["codex", " claude ", "codex"]
`)
	cfg := defaultConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	normalized := cfg.normalized()
	if bool(normalized.Enabled) {
		t.Fatal("quoted false must disable the plugin")
	}
	if normalized.ManagementBaseURL != "http://127.0.0.1:9000" {
		t.Fatalf("trailing slash not trimmed: %q", normalized.ManagementBaseURL)
	}
	if int(normalized.TimeoutSeconds) != 45 {
		t.Fatalf("quoted integer not parsed: %d", normalized.TimeoutSeconds)
	}
	if normalized.DefaultProvider != "xai" {
		t.Fatalf("provider not lowercased: %q", normalized.DefaultProvider)
	}
	if bool(normalized.AllowCallbackURL) {
		t.Fatal("quoted no must disable callback_url")
	}
	if len(normalized.Providers) != 2 || normalized.Providers[0] != "codex" || normalized.Providers[1] != "claude" {
		t.Fatalf("providers not normalized/deduped: %v", normalized.Providers)
	}
}

func TestConfigDefaultsWhenEmpty(t *testing.T) {
	cfg := pluginConfig{}.normalized()
	if cfg.ManagementBaseURL != defaultManagementBaseURL {
		t.Fatalf("base url = %q", cfg.ManagementBaseURL)
	}
	if int(cfg.TimeoutSeconds) != defaultTimeoutSeconds {
		t.Fatalf("timeout = %d", cfg.TimeoutSeconds)
	}
	if len(cfg.Providers) == 0 {
		t.Fatal("default provider list must not be empty")
	}
}

func TestSchemaNegotiationCapsAtSDKVersion(t *testing.T) {
	if got := negotiateSchema(3); got != 3 {
		t.Fatalf("host 3 -> %d, want 3", got)
	}
	if got := negotiateSchema(0); got != 1 {
		t.Fatalf("host 0 -> %d, want 1", got)
	}
	if got := negotiateSchema(9999); got != currentSchemaVersion() {
		t.Fatalf("host 9999 must cap at the SDK version")
	}
}

func TestManagementRegistrationAdvertisesMCPRoute(t *testing.T) {
	reg := managementRegistration()
	routes, _ := reg["routes"].([]map[string]any)
	var found bool
	for _, route := range routes {
		if route["Method"] == http.MethodPost && route["Path"] == "/mcp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("POST /mcp route missing: %+v", routes)
	}
}

func TestHandleManagementServesConsoleAndMCP(t *testing.T) {
	raw, _ := json.Marshal(managementRequest{Method: http.MethodGet, Path: "/v0/management/mcp/console"})
	body, errHandle := handleManagement(raw)
	if errHandle != nil {
		t.Fatalf("handleManagement: %v", errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(body, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("console envelope: %s", body)
	}
	var resp managementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), "navigator.modelContext") {
		t.Fatalf("console page missing modelContext registration")
	}
	if strings.Contains(string(resp.Body), "PROVIDER_LIST_PLACEHOLDER") {
		t.Fatal("provider placeholder was not replaced")
	}
}

func TestHandleManagementGetMCPIs405(t *testing.T) {
	raw, _ := json.Marshal(managementRequest{Method: http.MethodGet, Path: "/v0/management/mcp"})
	body, _ := handleManagement(raw)
	var env envelope
	_ = json.Unmarshal(body, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
