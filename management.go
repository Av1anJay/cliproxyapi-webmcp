package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cpaClient talks to this CPA instance's own /v8/management/* surface over
// loopback. The plugin runs inside the host process, so 127.0.0.1 is always
// reachable and no extra transport is involved.
type cpaClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func newCPAClient(cfg pluginConfig, callerKey string) *cpaClient {
	key := strings.TrimSpace(cfg.ManagementKey)
	if key == "" {
		key = strings.TrimSpace(callerKey)
	}
	return &cpaClient{
		baseURL: cfg.ManagementBaseURL,
		key:     key,
		http: &http.Client{
			Timeout: time.Duration(int(cfg.TimeoutSeconds)) * time.Second,
		},
	}
}

var errManagementUnauthorized = errors.New("management credential rejected by the local CPA instance")

func (c *cpaClient) do(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return 0, nil, fmt.Errorf("encode request body: %w", errMarshal)
		}
		reader = bytes.NewReader(raw)
	}
	req, errReq := http.NewRequestWithContext(ctx, method, target, reader)
	if errReq != nil {
		return 0, nil, fmt.Errorf("build management request: %w", errReq)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-Management-Key", c.key)
	}
	resp, errDo := c.http.Do(req)
	if errDo != nil {
		return 0, nil, fmt.Errorf("call local CPA management API: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if errRead != nil {
		return resp.StatusCode, nil, fmt.Errorf("read management response: %w", errRead)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, raw, errManagementUnauthorized
	}
	return resp.StatusCode, raw, nil
}

// oauthStart mirrors GET /v8/management/oauth/auth-url.
type oauthStartResult struct {
	Provider  string `json:"provider"`
	State     string `json:"state"`
	URL       string `json:"url"`
	Flow      string `json:"flow,omitempty"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}

func (c *cpaClient) oauthStart(ctx context.Context, provider string) (oauthStartResult, error) {
	status, raw, errDo := c.do(ctx, http.MethodGet, "/v8/management/oauth/auth-url",
		url.Values{"provider": {provider}}, nil)
	if errDo != nil {
		return oauthStartResult{}, errDo
	}
	var payload struct {
		Status    string `json:"status"`
		State     string `json:"state"`
		URL       string `json:"url"`
		Flow      string `json:"flow"`
		ExpiresIn int    `json:"expires_in"`
		Error     string `json:"error"`
	}
	_ = json.Unmarshal(raw, &payload)
	if status != http.StatusOK || strings.TrimSpace(payload.URL) == "" {
		message := strings.TrimSpace(payload.Error)
		if message == "" {
			message = fmt.Sprintf("management API returned status %d", status)
		}
		return oauthStartResult{}, fmt.Errorf("%s", message)
	}
	return oauthStartResult{
		Provider:  provider,
		State:     payload.State,
		URL:       payload.URL,
		Flow:      payload.Flow,
		ExpiresIn: payload.ExpiresIn,
	}, nil
}

// oauthStatus mirrors GET /v8/management/oauth/status.
type oauthStatusResult struct {
	State  string `json:"state"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

const (
	oauthStatusWait = "wait"
	oauthStatusOK   = "ok"
	oauthStatusErr  = "error"
)

func (c *cpaClient) oauthStatus(ctx context.Context, state string) (oauthStatusResult, error) {
	status, raw, errDo := c.do(ctx, http.MethodGet, "/v8/management/oauth/status",
		url.Values{"state": {state}}, nil)
	if errDo != nil {
		return oauthStatusResult{}, errDo
	}
	var payload struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(raw, &payload)
	out := oauthStatusResult{State: state, Status: strings.TrimSpace(payload.Status), Error: strings.TrimSpace(payload.Error)}
	if status != http.StatusOK {
		out.Status = oauthStatusErr
		if out.Error == "" {
			out.Error = fmt.Sprintf("management API returned status %d", status)
		}
	}
	if out.Status == "" {
		out.Status = oauthStatusWait
	}
	return out, nil
}

// oauthCallback mirrors POST /v8/management/oauth/callback.
func (c *cpaClient) oauthCallback(ctx context.Context, provider, state, code, redirectURL string) (map[string]any, error) {
	body := map[string]any{}
	if provider != "" {
		body["provider"] = provider
	}
	if state != "" {
		body["state"] = state
	}
	if code != "" {
		body["code"] = code
	}
	if redirectURL != "" {
		body["redirect_url"] = redirectURL
	}
	status, raw, errDo := c.do(ctx, http.MethodPost, "/v8/management/oauth/callback", nil, body)
	if errDo != nil {
		return nil, errDo
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if status != http.StatusOK {
		message, _ := out["error"].(string)
		if strings.TrimSpace(message) == "" {
			message = fmt.Sprintf("management API returned status %d", status)
		}
		return out, fmt.Errorf("%s", message)
	}
	return out, nil
}

// oauthCancel mirrors DELETE /v8/management/oauth/session.
func (c *cpaClient) oauthCancel(ctx context.Context, state string) (map[string]any, error) {
	status, raw, errDo := c.do(ctx, http.MethodDelete, "/v8/management/oauth/session",
		url.Values{"state": {state}}, nil)
	if errDo != nil {
		return nil, errDo
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if status != http.StatusOK {
		message, _ := out["error"].(string)
		if strings.TrimSpace(message) == "" {
			message = fmt.Sprintf("management API returned status %d", status)
		}
		return out, fmt.Errorf("%s", message)
	}
	return out, nil
}

// accountCooldown is a local retry restriction the host reports per account.
type accountCooldown struct {
	Scope            string `json:"scope"`
	ModelKey         string `json:"model_key,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RetryAt          string `json:"retry_at,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds,omitempty"`
}

// accountSIWC is the Codex "sign in with ChatGPT" binding state, which decides
// whether an account can serve requests through that path.
type accountSIWC struct {
	Supported bool   `json:"supported"`
	Bound     bool   `json:"bound"`
	Status    string `json:"status,omitempty"`
	Use       bool   `json:"use,omitempty"`
}

// accountSummary is a deliberately narrow projection of
// GET /v8/management/credentials. Tokens, id_token claims, file paths and
// quota internals are dropped; what remains is what a human or agent needs to
// decide which account to use or re-login.
type accountSummary struct {
	Name           string            `json:"name"`
	Provider       string            `json:"provider,omitempty"`
	Account        string            `json:"account,omitempty"`
	Email          string            `json:"email,omitempty"`
	AccountType    string            `json:"account_type,omitempty"`
	PlanType       string            `json:"plan_type,omitempty"`
	Label          string            `json:"label,omitempty"`
	Status         string            `json:"status,omitempty"`
	StatusMessage  string            `json:"status_message,omitempty"`
	Disabled       bool              `json:"disabled"`
	Unavailable    bool              `json:"unavailable"`
	Usable         bool              `json:"usable"`
	RuntimeOnly    bool              `json:"runtime_only,omitempty"`
	Priority       *int              `json:"priority,omitempty"`
	Note           string            `json:"note,omitempty"`
	Success        int               `json:"success"`
	Failed         int               `json:"failed"`
	CreatedAt      string            `json:"created_at,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
	LastRefresh    string            `json:"last_refresh,omitempty"`
	NextRetryAfter string            `json:"next_retry_after,omitempty"`
	Cooldowns      []accountCooldown `json:"cooldowns,omitempty"`
	SIWC           *accountSIWC      `json:"siwc,omitempty"`
}

// accountFilter narrows the host's credential list. The host only filters by
// name and auth_index, so everything else is applied here.
type accountFilter struct {
	Provider        string
	Status          string
	IncludeDisabled bool
}

func (c *cpaClient) accountsList(ctx context.Context, filter accountFilter) ([]accountSummary, error) {
	status, raw, errDo := c.do(ctx, http.MethodGet, "/v8/management/credentials", nil, nil)
	if errDo != nil {
		return nil, errDo
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("management API returned status %d", status)
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode credentials response: %w", errUnmarshal)
	}
	out := make([]accountSummary, 0, len(payload.Files))
	for _, file := range payload.Files {
		item := projectAccount(file)
		if filter.Provider != "" && !strings.EqualFold(item.Provider, filter.Provider) {
			continue
		}
		if filter.Status != "" && !strings.EqualFold(item.Status, filter.Status) {
			continue
		}
		if !filter.IncludeDisabled && item.Disabled {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func projectAccount(file map[string]any) accountSummary {
	item := accountSummary{
		Name:           stringField(file, "name", "id"),
		Provider:       stringField(file, "provider", "type"),
		Account:        stringField(file, "account"),
		Email:          stringField(file, "email"),
		AccountType:    stringField(file, "account_type"),
		PlanType:       stringField(file, "plan_type"),
		Label:          stringField(file, "label"),
		Status:         stringField(file, "status"),
		StatusMessage:  stringField(file, "status_message"),
		Disabled:       boolField(file, "disabled"),
		Unavailable:    boolField(file, "unavailable"),
		RuntimeOnly:    boolField(file, "runtime_only"),
		Note:           stringField(file, "note"),
		Success:        intField(file, "success"),
		Failed:         intField(file, "failed"),
		CreatedAt:      stringField(file, "created_at"),
		UpdatedAt:      stringField(file, "updated_at", "modtime"),
		LastRefresh:    stringField(file, "last_refresh"),
		NextRetryAfter: stringField(file, "next_retry_after"),
		Priority:       intPtrField(file, "priority"),
	}
	// The host nests the ChatGPT plan inside the id_token claims; a flat
	// plan_type only appears on uploads and older builds.
	if item.PlanType == "" {
		if idToken, ok := file["id_token"].(map[string]any); ok {
			item.PlanType = stringField(idToken, "plan_type")
		}
	}
	item.Usable = !item.Disabled && !item.Unavailable &&
		!strings.EqualFold(item.Status, "disabled") &&
		!strings.EqualFold(item.Status, "error")

	if cooldowns, ok := file["cooldowns"].([]any); ok {
		for _, entry := range cooldowns {
			fields, okFields := entry.(map[string]any)
			if !okFields {
				continue
			}
			item.Cooldowns = append(item.Cooldowns, accountCooldown{
				Scope:            stringField(fields, "scope"),
				ModelKey:         stringField(fields, "model_key"),
				Reason:           stringField(fields, "reason"),
				RetryAt:          stringField(fields, "retry_at"),
				RemainingSeconds: int64(intField(fields, "remaining_seconds")),
			})
		}
	}

	// SIWC fields are only emitted for codex accounts.
	if _, present := file["siwc_bind_supported"]; present {
		item.SIWC = &accountSIWC{
			Supported: boolField(file, "siwc_bind_supported"),
			Bound:     boolField(file, "siwc_bound"),
			Status:    stringField(file, "siwc_status"),
			Use:       boolField(file, "use_siwc"),
		}
	}
	return item
}

// accountTotals is the aggregate an agent should read before paging through
// individual accounts.
type accountTotals struct {
	Total       int            `json:"total"`
	Usable      int            `json:"usable"`
	Disabled    int            `json:"disabled"`
	Unavailable int            `json:"unavailable"`
	ByProvider  map[string]int `json:"by_provider"`
	ByStatus    map[string]int `json:"by_status"`
}

func summarizeAccounts(items []accountSummary) accountTotals {
	totals := accountTotals{
		Total:      len(items),
		ByProvider: map[string]int{},
		ByStatus:   map[string]int{},
	}
	for _, item := range items {
		if item.Usable {
			totals.Usable++
		}
		if item.Disabled {
			totals.Disabled++
		}
		if item.Unavailable {
			totals.Unavailable++
		}
		provider := item.Provider
		if provider == "" {
			provider = "unknown"
		}
		totals.ByProvider[provider]++
		status := item.Status
		if status == "" {
			status = "unknown"
		}
		totals.ByStatus[status]++
	}
	return totals
}

func stringField(payload map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := payload[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boolField(payload map[string]any, name string) bool {
	switch value := payload[name].(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		return errParse == nil && parsed
	default:
		return false
	}
}

func intField(payload map[string]any, name string) int {
	switch value := payload[name].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		parsed, errParse := value.Int64()
		if errParse == nil {
			return int(parsed)
		}
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	}
	return 0
}

func intPtrField(payload map[string]any, name string) *int {
	if _, ok := payload[name]; !ok {
		return nil
	}
	value := intField(payload, name)
	return &value
}

// waitForCompletion long-polls the host's own OAuth session until it completes,
// fails, or the caller's budget runs out.
func (c *cpaClient) waitForCompletion(ctx context.Context, state string, timeout time.Duration) (oauthStatusResult, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, errStatus := c.oauthStatus(ctx, state)
		if errStatus != nil {
			return oauthStatusResult{}, errStatus
		}
		if status.Status != oauthStatusWait {
			return status, nil
		}
		if time.Now().After(deadline) {
			status.Status = oauthStatusWait
			status.Error = ""
			return status, nil
		}
		select {
		case <-ctx.Done():
			return oauthStatusResult{}, ctx.Err()
		case <-time.After(750 * time.Millisecond):
		}
	}
}
