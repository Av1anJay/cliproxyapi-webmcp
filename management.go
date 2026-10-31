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

// credentialSummary is a deliberately narrow projection of
// GET /v8/management/credentials: no tokens, no emails beyond what the host
// already exposes in the file name.
type credentialSummary struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status,omitempty"`
	Disabled bool   `json:"disabled"`
	PlanType string `json:"plan_type,omitempty"`
	Expired  string `json:"expired,omitempty"`
}

func (c *cpaClient) credentialsList(ctx context.Context) ([]credentialSummary, error) {
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
	out := make([]credentialSummary, 0, len(payload.Files))
	for _, file := range payload.Files {
		item := credentialSummary{
			Name:     stringField(file, "name", "id"),
			Provider: stringField(file, "provider", "type"),
			Type:     stringField(file, "type"),
			Status:   stringField(file, "status"),
			Disabled: boolField(file, "disabled"),
			PlanType: stringField(file, "plan_type"),
		}
		if expiry, ok := file["expired"].(string); ok {
			item.Expired = expiry
		}
		out = append(out, item)
	}
	return out, nil
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
