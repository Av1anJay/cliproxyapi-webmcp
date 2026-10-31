package main

// toolDefinitions is the MCP tools/list payload. Descriptions are written for an
// agent that has no idea it is talking to a proxy: say what to do next, not just
// what the tool does.
func toolDefinitions(cfg pluginConfig) []map[string]any {
	providerEnum := append([]string(nil), cfg.Providers...)
	return []map[string]any{
		{
			"name":        "oauth_login_start",
			"description": "Start an OAuth login for a provider and return a login URL plus an opaque state. Show login_url to the user, then call oauth_login_wait with the returned state. Providers that use the device flow also return a code the user types at the URL.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{
						"type":        "string",
						"enum":        providerEnum,
						"description": "Provider to log in. Defaults to the configured default_provider.",
					},
				},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": false, "openWorldHint": true},
		},
		{
			"name":        "oauth_login_wait",
			"description": "Long-poll a started OAuth login until it completes, fails, or the timeout elapses. Returns status=ok (credential saved), status=error, or status=wait (call again). Always use the state from oauth_login_start.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"state": map[string]any{
						"type":        "string",
						"description": "Opaque state returned by oauth_login_start.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "How long to wait before returning status=wait. Defaults to 20.",
					},
				},
				"required":             []string{"state"},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        "oauth_login_submit",
			"description": "Finish a login when the browser cannot reach the provider's localhost callback port. Pass either the full pasted redirect URL as callback_url, or the code and state separately. Call oauth_login_wait afterwards to confirm.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"callback_url": map[string]any{
						"type":        "string",
						"description": "Full redirect URL from the browser address bar; state and code are extracted from it.",
					},
					"code": map[string]any{
						"type":        "string",
						"description": "Authorization code, when callback_url is not used.",
					},
					"state": map[string]any{
						"type":        "string",
						"description": "State from oauth_login_start, when callback_url is not used.",
					},
					"provider": map[string]any{
						"type":        "string",
						"description": "Optional provider override; normally inferred.",
					},
				},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": false, "openWorldHint": true},
		},
		{
			"name":        "oauth_login_cancel",
			"description": "Cancel a pending OAuth login by state so no credential is saved.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"state": map[string]any{"type": "string", "description": "State from oauth_login_start."},
				},
				"required":             []string{"state"},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true},
		},
		{
			"name":        "list_providers",
			"description": "List the OAuth provider names this CLIProxyAPI instance accepts, plus the default provider.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        "list_credentials",
			"description": "List saved credentials (name, provider, status, plan, expiry). Never returns tokens.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        "cpa_status",
			"description": "Report plugin and host schema versions, the loopback management base URL, and where the management credential comes from. Use this first when a call fails with an authorization error.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			"annotations": map[string]any{"readOnlyHint": true},
		},
	}
}
