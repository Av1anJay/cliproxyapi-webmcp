package main

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultManagementBaseURL = "http://127.0.0.1:8317"
	defaultTimeoutSeconds    = 20
	defaultProvider          = "codex"
)

// pluginConfig is the YAML under plugins.configs.webmcp.
type pluginConfig struct {
	// Enabled is the in-plugin pass-through switch. plugins.configs.<id>.enabled
	// is a host-side gate; this one lets the tools stay registered but inert.
	Enabled flexBool `yaml:"enabled"`
	// ManagementBaseURL points at this CPA instance's own HTTP listener.
	ManagementBaseURL string `yaml:"management_base_url"`
	// ManagementKey overrides the credential forwarded from the MCP caller.
	ManagementKey string `yaml:"management_key"`
	// TimeoutSeconds bounds every loopback management call.
	TimeoutSeconds flexInt `yaml:"timeout_seconds"`
	// DefaultProvider is used when a tool call omits provider.
	DefaultProvider string `yaml:"default_provider"`
	// AllowCallbackURL lets oauth_callback accept a full pasted redirect URL.
	AllowCallbackURL flexBool `yaml:"allow_callback_url"`
	// MaxWaitSeconds caps oauth_wait.
	MaxWaitSeconds flexInt `yaml:"max_wait_seconds"`
	// Providers overrides the advertised provider list.
	Providers []string `yaml:"providers"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Enabled:           flexBool(true),
		ManagementBaseURL: defaultManagementBaseURL,
		TimeoutSeconds:    flexInt(defaultTimeoutSeconds),
		DefaultProvider:   defaultProvider,
		AllowCallbackURL:  true,
		MaxWaitSeconds:    300,
	}
}

func (c pluginConfig) normalized() pluginConfig {
	out := c
	out.ManagementBaseURL = strings.TrimRight(strings.TrimSpace(out.ManagementBaseURL), "/")
	if out.ManagementBaseURL == "" {
		out.ManagementBaseURL = defaultManagementBaseURL
	}
	if !strings.HasPrefix(out.ManagementBaseURL, "http://") && !strings.HasPrefix(out.ManagementBaseURL, "https://") {
		out.ManagementBaseURL = "http://" + out.ManagementBaseURL
	}
	out.ManagementKey = strings.TrimSpace(out.ManagementKey)
	if int(out.TimeoutSeconds) <= 0 {
		out.TimeoutSeconds = flexInt(defaultTimeoutSeconds)
	}
	if int(out.TimeoutSeconds) > 120 {
		out.TimeoutSeconds = 120
	}
	out.DefaultProvider = strings.ToLower(strings.TrimSpace(out.DefaultProvider))
	if out.DefaultProvider == "" {
		out.DefaultProvider = defaultProvider
	}
	if int(out.MaxWaitSeconds) <= 0 {
		out.MaxWaitSeconds = 300
	}
	if int(out.MaxWaitSeconds) > 900 {
		out.MaxWaitSeconds = 900
	}
	out.Providers = normalizeProviderList(out.Providers)
	if len(out.Providers) == 0 {
		out.Providers = defaultProviders()
	}
	return out
}

func defaultProviders() []string {
	// Verified against a live 8.0.46 instance: these all answer
	// GET /v8/management/oauth/auth-url with a login URL.
	return []string{"claude", "codex", "antigravity", "kimi", "kimi-ai", "xai", "devin", "meta"}
}

func normalizeProviderList(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// flexBool/flexInt tolerate the loose YAML the CPA management UI writes
// (quoted numbers, empty strings, nulls) without discarding plugin defaults.
type flexBool bool

func (b *flexBool) UnmarshalYAML(node *yaml.Node) error {
	if b == nil || yamlNodeEmpty(node) {
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected boolean, got YAML kind %d", node.Kind)
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return nil
	}
	// The CPA management UI writes yes/no/on/off as well as true/false.
	switch strings.ToLower(raw) {
	case "yes", "on", "y", "enabled":
		*b = flexBool(true)
		return nil
	case "no", "off", "n", "disabled":
		*b = flexBool(false)
		return nil
	}
	value, errParse := strconv.ParseBool(raw)
	if errParse != nil {
		return fmt.Errorf("invalid boolean %q", raw)
	}
	*b = flexBool(value)
	return nil
}

type flexInt int

func (i *flexInt) UnmarshalYAML(node *yaml.Node) error {
	if i == nil || yamlNodeEmpty(node) {
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected integer, got YAML kind %d", node.Kind)
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return nil
	}
	value, errParse := strconv.Atoi(raw)
	if errParse != nil {
		asFloat, errFloat := strconv.ParseFloat(raw, 64)
		if errFloat != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		value = int(asFloat)
	}
	*i = flexInt(value)
	return nil
}

func yamlNodeEmpty(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return true
	}
	return false
}
