package tests

import (
	"encoding/json"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
)

// TestRegisterReturnsFullContract verifies that the registration response satisfies the host contract.
func TestRegisterReturnsFullContract(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodPluginRegister, app.LifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	var reg app.RegistrationInfo
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}

	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Version == "" {
		t.Fatal("metadata.version must not be empty")
	}
	capabilities := reg.Capabilities
	if !capabilities.ModelProvider || !capabilities.AuthProvider || !capabilities.Executor ||
		!capabilities.QuotaProvider || !capabilities.ManagementAPI {
		t.Fatalf("all five capabilities must be declared: %+v", capabilities)
	}
	// model.for_auth is only called by the host when executor_model_scope contains oauth/both;
	// a wrong declaration will silently disable realm isolation.
	if capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeBoth) {
		t.Fatalf("executor_model_scope = %q, want %q (model.for_auth depends on it)",
			capabilities.ExecutorModelScope, pluginapi.ExecutorModelScopeBoth)
	}
	// Declare chat-completions only: multi-protocols are translated by the host.
	if len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != app.FormatChatCompletions {
		t.Fatalf("executor_input_formats = %v, want [%s]", capabilities.ExecutorInputFormats, app.FormatChatCompletions)
	}
}

// TestReconfigureKeepsBackgroundRunning verifies that hot reload returns the full registration response without restarting the background worker.
func TestReconfigureKeepsBackgroundRunning(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	callMethod(t, pluginabi.MethodPluginRegister, app.LifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	if !testApp.BackgroundRunning() {
		t.Fatal("background work must start on register")
	}

	result := callMethod(t, pluginabi.MethodPluginReconfigure, app.LifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	var reg app.RegistrationInfo
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode reconfigure response: %v", errUnmarshal)
	}
	if reg.Metadata.Name == "" {
		t.Fatal("reconfigure must return the full registration shape")
	}
	if !testApp.BackgroundRunning() {
		t.Fatal("reconfigure must not stop the background work")
	}
}

// TestUnknownMethodReturnsEnvelopeNotError verifies that unknown methods return an envelope-wrapped error.
func TestUnknownMethodReturnsEnvelopeNotError(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := testApp.HandleMethod("no.such.method", nil)
	if errHandle != nil {
		t.Fatalf("unknown method must not return a Go error: %v", errHandle)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("want unknown_method envelope, got %s", raw)
	}
}

// TestAuthParseRecognizesOwnCredential verifies the ownership determination of credentials.
func TestAuthParseRecognizesOwnCredential(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	cases := []struct {
		name       string
		fileName   string
		provider   string
		body       string
		wantHandle bool
	}{
		{"nested credential", "workbuddy-abc.json", "", sampleCredentialJSON("cn", "abc"), true},
		// Real production form: workbuddy-<uuid>.json, containing only auth+account keys,
		// without type or device_token fields. This is the file users actually place into auths/.
		{"real world uuid file", "workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", "",
			`{"account":{"enterpriseId":"","nickname":"x","uid":"79fdc1fc-de43-40da-b6f0-fe05d9e4367b"},
			  "auth":{"accessToken":"at","domain":"www.codebuddy.cn","expiresAt":1792274401,
			          "realm":"cn","refreshToken":"rt"}}`, true},
		// Flat structure relies on filename conventions for ownership (top-level token is a generic field, not a valid criterion).
		{"flat credential with proper name", "workbuddy-wb.json", "", `{"accessToken":"at","uid":"u1"}`, true},
		{"provider hint", "other.json", app.ProviderKey, `{"accessToken":"at"}`, true},
		// Neither filename nor structure points to this plugin: must reject, otherwise it will mistakenly swallow other plugins' credentials.
		{"ambiguous flat file", "wb.json", "", `{"accessToken":"at","uid":"u1"}`, false},
		{"foreign provider", "claude.json", "anthropic", `{"access_token":"x","refresh_token":"y"}`, false},
		{"not json", "notes.json", "", "this is not json", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				Provider: testCase.provider,
				FileName: testCase.fileName,
				RawJSON:  []byte(testCase.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if response.Handled != testCase.wantHandle {
				t.Fatalf("handled = %v, want %v", response.Handled, testCase.wantHandle)
			}
			if !testCase.wantHandle {
				return
			}
			if response.Auth.Provider != app.ProviderKey {
				t.Fatalf("provider = %q, want %q", response.Auth.Provider, app.ProviderKey)
			}
			if response.Auth.FileName != testCase.fileName {
				t.Fatalf("file_name = %q, want %q", response.Auth.FileName, testCase.fileName)
			}
		})
	}
}

// TestAuthParsePutsRefreshTokenInMetadata verifies the contract required for host-side refreshing.
//
// The host determines whether "this credential can be refreshed" by checking the refresh_token in Metadata.
// Without it, an OAuth account's access token will never be refreshed once expired.
func TestAuthParsePutsRefreshTokenInMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		FileName: "workbuddy-x.json",
		RawJSON:  []byte(sampleCredentialJSON("cn", "x")),
	})
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if response.Auth.Metadata["refresh_token"] == nil {
		t.Fatal("metadata.refresh_token is required for host-driven refresh")
	}
	if response.Auth.Metadata["realm"] != "cn" {
		t.Fatalf("metadata.realm = %v, want cn", response.Auth.Metadata["realm"])
	}
	// Account tokens must never be echoed back to metadata/attributes (only allowed in StorageJSON).
	for _, key := range []string{"access_token", "token", "device_token"} {
		if _, okKey := response.Auth.Metadata[key]; okKey {
			t.Fatalf("metadata must not leak %q", key)
		}
	}
	if _, okKey := response.Auth.Attributes["access_token"]; okKey {
		t.Fatal("attributes must not leak access_token")
	}
}
