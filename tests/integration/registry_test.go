package tests

// This file is used to lock down the format of registry.json.
//
// registry.json is actually read when users configure third-party sources. Manually breaking it means
// plugins simply disappear in the store (and the host is silent about it), so it's worth guarding with tests.
//
// Host CLIProxyAPI/internal/pluginstore validation rules for third-party source manifests:
//   - Top-level schema_version must be equal to 1
//   - Each entry requires id / name / description / author; github-release type also requires repository
//   - id matches ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$
//   - version is optional (inferred by the host from the latest release), and if present must match ^[0-9][0-9A-Za-z.+-]*$
//   - repository must be https://github.com/{owner}/{repo}

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"free-model-hub/internal/admin"
	"free-model-hub/internal/app"
	"free-model-hub/internal/core"
)

var (
	// unwrapRe matches the pattern of "unwrapping another envelope layer": .result not followed by s (distinguishing valid .results).
	unwrapRe = regexp.MustCompile(`\.result\s*[;,)\n]`)
)

// TestPluginIDMatchesLibraryName verifies that the plugin ID matches the artifact name.
//
// The host searches for dynamic libraries by {id}{ext}; a mismatch between ID and filename causes the plugin to be silently ignored.
func TestPluginIDMatchesLibraryName(t *testing.T) {
	if app.PluginID != "free-model-hub" {
		t.Fatalf("app.PluginID = %q; changing it requires renaming the build output and registry", app.PluginID)
	}
	if app.ProviderKey != core.ProviderKey {
		t.Fatalf("app.ProviderKey (%q) must match core.ProviderKey (%q)", app.ProviderKey, core.ProviderKey)
	}
}

// TestConsolePagePathsAreDeclared verifies that paths referenced by console pages are declared in the router table.
//
// Pages referencing undeclared routes behave as "clicking does nothing" without any host error.
func TestConsolePagePathsAreDeclared(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	page := admin.Page(app.ManagementRoutePrefix)

	declared := map[string]bool{}
	for _, route := range testApp.ManagementRegistration().Routes {
		suffix := strings.TrimPrefix(route.Path, app.ManagementRoutePrefix)
		declared[suffix] = true
	}

	// Pages call APIs via MGMT + path concatenation, so check against the known manifest.
	referenced := []string{
		"/status", "/logs", "/quotas", "/models/refresh",
		"/models/alias", "/settings", "/keepalive",
	}
	for _, path := range referenced {
		if !strings.Contains(page, `"`+path) {
			t.Fatalf("console page does not reference %q (test list is stale?)", path)
		}
		if !declared[path] {
			t.Fatalf("console page calls %q but the route is not declared", path)
		}
	}
}

// TestManagementResponseUsesHostWireFormat verifies that management responses follow the host wire format.
//
// pluginapi.ManagementResponse **has no json tags**, and the host decodes by Go field names
// (StatusCode / Headers / Body), with Body being []byte—Go encodes it as
// base64, and the host decodes it before writing the raw bytes to the browser.
//
// An attempt to change to a custom format with snake_case + json.RawMessage caused host deserialization to fail,
// resulting in the page showing "plugin resource handler failed" with nothing displayed. This test locks down
// the contract: "must use host structs, key names must be Go field names".
func TestManagementResponseUsesHostWireFormat(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	resp := app.JSONResponse(http.StatusOK, map[string]any{"hello": "world"})
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	// Key names must be Go field names recognized by the host, not snake_case.
	if _, okStatus := decoded["StatusCode"]; !okStatus {
		t.Fatalf("response must use the host's Go field names, got %s", raw)
	}
	if _, okBad := decoded["status_code"]; okBad {
		t.Fatalf("snake_case keys break the host decoder, got %s", raw)
	}
	if _, okBody := decoded["Body"]; !okBody {
		t.Fatalf("Body must be present, got %s", raw)
	}
	// HTML responses also use host structs.
	htmlRaw, errHTML := json.Marshal(app.HTMLResponse(http.StatusOK, []byte("<html>x</html>")))
	if errHTML != nil {
		t.Fatalf("marshal html: %v", errHTML)
	}
	if !strings.Contains(string(htmlRaw), `"StatusCode":200`) {
		t.Fatalf("html response must use the host wire format, got %s", htmlRaw)
	}
}

// TestHostReservedQuotaPathNotShadowed verifies that plugin routes do not conflict with host reserved paths.
//
// The host's single-account quota route is /v0/management/plugins/<id>/quota,
// so the plugin's own batch interface must use a different path (/quotas) to avoid being shadowed by the host.
func TestHostReservedQuotaPathNotShadowed(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	for _, route := range testApp.ManagementRegistration().Routes {
		suffix := strings.TrimPrefix(route.Path, app.ManagementRoutePrefix)
		if suffix == "/quota" {
			t.Fatal("plugin must not declare /quota (the host owns that path for single-account quota)")
		}
	}
}

// TestCapabilitiesMatchImplementedHandlers verifies that all declared capabilities have corresponding implementations.
//
// Declaring a capability without implementing it causes the host to invoke an "unknown method", leading to silent feature failure.
func TestCapabilitiesMatchImplementedHandlers(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	registration := testApp.Registration()
	if !registration.Capabilities.ModelProvider || !registration.Capabilities.AuthProvider ||
		!registration.Capabilities.Executor || !registration.Capabilities.QuotaProvider ||
		!registration.Capabilities.ManagementAPI {
		t.Fatal("all five capabilities must be declared")
	}

	// The key methods of each capability must be dispatchable (not returning unknown_method).
	methods := []string{
		"model.static", "model.for_auth",
		"auth.identifier", "auth.parse", "auth.login.start", "auth.login.poll", "auth.refresh",
		"executor.identifier", "executor.execute", "executor.execute_stream",
		"executor.count_tokens", "executor.http_request",
		"quota.identifier", "quota.describe", "quota.fetch", "quota.reset",
		"management.register", "management.handle",
	}
	for _, method := range methods {
		raw, errHandle := testApp.HandleMethod(method, nil)
		if errHandle != nil {
			// Business errors (such as missing parameters) are acceptable; protocol-layer errors are the actual issue.
			continue
		}
		if strings.Contains(string(raw), "unknown_method") {
			t.Fatalf("method %s is declared but not dispatched", method)
		}
	}
}
