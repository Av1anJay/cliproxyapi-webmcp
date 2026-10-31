package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// managementRequest mirrors pluginapi.ManagementRequest. Body arrives base64
// decoded by the SDK envelope, so it is plain bytes here.
type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url_Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

// url_Values is url.Values with a lenient decoder: the host sends a map of
// string slices, but a missing/null value must not fail the whole request.
type url_Values map[string][]string

func (v *url_Values) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	out := map[string][]string{}
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return errUnmarshal
	}
	*v = out
	return nil
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []map[string]any{
			{
				"Method":      http.MethodPost,
				"Path":        "/mcp",
				"Description": "MCP Streamable HTTP endpoint (JSON-RPC 2.0). Send X-Management-Key or Authorization: Bearer.",
			},
			{
				"Method":      http.MethodGet,
				"Path":        "/mcp",
				"Description": "Probe endpoint; MCP servers that do not offer a standalone SSE stream answer 405 here.",
			},
		},
		"resources": []map[string]any{
			{
				"Path":        "/console",
				"Menu":        "WebMCP Console",
				"Description": "Browser console that registers navigator.modelContext tools for CLIProxyAPI OAuth login.",
			},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
		}
	}
	switch {
	case strings.EqualFold(req.Method, http.MethodPost) && strings.HasSuffix(req.Path, "/mcp"):
		return okEnvelope(handleMCPPost(req))
	case strings.EqualFold(req.Method, http.MethodGet) && strings.HasSuffix(req.Path, "/mcp"):
		resp := managementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    http.Header{"Content-Type": []string{"application/json"}, "Allow": []string{"POST"}},
			Body:       []byte(`{"error":"standalone SSE stream is not offered; POST JSON-RPC to this URL"}`),
		}
		return okEnvelope(resp)
	case strings.EqualFold(req.Method, http.MethodGet):
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       []byte(consolePage(currentConfig().Providers)),
		})
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":"not found"}`),
		})
	}
}

func handleMCPPost(req managementRequest) managementResponse {
	cfg := currentConfig()
	headers := http.Header(req.Headers)
	body, errHandle := handleMCP(context.Background(), cfg, headers, req.Body)
	if errHandle != nil {
		message, _ := json.Marshal(errHandle.Error())
		return managementResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"jsonrpc":"2.0","error":{"code":-32600,"message":` + string(message) + `}}`),
		}
	}
	if body == nil {
		// Notification: 202 with no body.
		return managementResponse{StatusCode: http.StatusAccepted}
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":         []string{"application/json"},
			"Cache-Control":        []string{"no-store"},
			"MCP-Protocol-Version": []string{mcpProtocolVersion},
		},
		Body: body,
	}
}
