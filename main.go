package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginID      = "webmcp"
	pluginVersion = "0.2.1"
	// minSchemaVersion gates on management routes plus raw management JSON.
	minSchemaVersion = 6
)

var (
	stateMu sync.RWMutex
	state   = defaultConfig()

	// schemaMu guards the schema version agreed with the host. A host older than
	// the SDK this plugin was built against rejects any registration advertising a
	// newer schema, so the plugin must echo back the host's own version.
	schemaMu       sync.RWMutex
	negotiatedVer  = pluginabi.SchemaVersion
	hostSchemaSeen uint32
)

// negotiateSchema records the schema version to advertise. The host passes its
// own compiled version in the lifecycle request; advertising anything higher is
// rejected with "plugin schema version N is not supported".
func negotiateSchema(hostVersion uint32) uint32 {
	if hostVersion == 0 {
		hostVersion = 1
	}
	agreed := hostVersion
	if pluginabi.SchemaVersion < agreed {
		agreed = pluginabi.SchemaVersion
	}
	schemaMu.Lock()
	negotiatedVer = agreed
	hostSchemaSeen = hostVersion
	schemaMu.Unlock()
	return agreed
}

func currentSchemaVersion() uint32 {
	schemaMu.RLock()
	defer schemaMu.RUnlock()
	return negotiatedVer
}

func hostSchemaVersion() uint32 {
	schemaMu.RLock()
	defer schemaMu.RUnlock()
	return hostSchemaSeen
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ManagementAPI bool `json:"management_api"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	stateMu.Lock()
	state = defaultConfig()
	stateMu.Unlock()
}

func currentConfig() pluginConfig {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if req.SchemaVersion < minSchemaVersion {
		return fmt.Errorf("webmcp requires host schema version %d or newer (got %d); upgrade CLIProxyAPI", minSchemaVersion, req.SchemaVersion)
	}
	negotiateSchema(req.SchemaVersion)
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return fmt.Errorf("parse plugin config: %w", errUnmarshal)
		}
	}
	stateMu.Lock()
	state = cfg.normalized()
	stateMu.Unlock()
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: currentSchemaVersion(),
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "Av1anJay",
			GitHubRepository: "https://github.com/AvianJay/cliproxyapi-webmcp",
			ConfigFields:     configFields(),
		},
		Capabilities: registrationCapability{ManagementAPI: true},
	}
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name:        "management_base_url",
			Type:        pluginapi.ConfigFieldTypeString,
			Default:     defaultManagementBaseURL,
			Description: "Loopback base URL of this CPA instance, used to call /v8/management/*.",
		},
		{
			Name:        "management_key",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Optional fixed management key. When empty the caller's own credential is forwarded.",
		},
		{
			Name:        "timeout_seconds",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Default:     defaultTimeoutSeconds,
			Description: "Timeout for each loopback management call (1-120).",
		},
		{
			Name:        "default_provider",
			Type:        pluginapi.ConfigFieldTypeString,
			Default:     defaultProvider,
			Description: "Provider used when a tool call omits one.",
		},
		{
			Name:        "allow_callback_url",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Default:     true,
			Description: "Let oauth_callback accept a pasted full redirect URL instead of state+code.",
		},
		{
			Name:        "max_wait_seconds",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Default:     300,
			Description: "Upper bound for a single oauth_wait long poll (1-900).",
		},
		{
			Name:        "providers",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "Advertised provider names; defaults to claude, codex, antigravity, kimi, kimi-ai, xai, devin, meta.",
		},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
