package admin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
	"free-model-hub/internal/tasks"
)

type stubDeps struct {
	store    *state.Store
	registry *core.Registry
	manager  *Catalog

	callbackResult *core.CallbackSubmitResult
	callbackErr    error

	loginStart    *pluginapi.AuthLoginStartResponse
	loginStartErr error
	captcha       core.CaptchaConfig
	captchaErr    error
}

func (s stubDeps) Identity() Identity {
	return Identity{ID: "free-model-hub", DisplayName: "FreeTier 2API", ProviderKey: "freehub", Version: "test"}
}
func (s stubDeps) Config() PluginConfig { return DefaultPluginConfig() }
func (s stubDeps) State() state.State   { return s.store.Snapshot() }
func (s stubDeps) MutateState(fn func(*state.State)) {
	s.store.Mutate(fn)
}
func (s stubDeps) Model() *Catalog { return s.manager }
func (s stubDeps) ResolveCredential(context.Context, string, []byte, string, map[string]string) (*core.Credential, core.Vendor, error) {
	return nil, nil, context.Canceled
}
func (s stubDeps) RefreshModels(context.Context) (int, error) { return 0, nil }
func (s stubDeps) Reconfigure() error                         { return nil }

// callbackResult / callbackErr allow test cases to drive the return branch of "paste callback URL".
func (s stubDeps) SendLoginSMS(context.Context, string, string) (string, error) {
	return "", nil
}

func (s stubDeps) VerifyLoginSMS(context.Context, string, string, string, string) (string, string, string, error) {
	return "", "", "", nil
}

func (s stubDeps) SubmitLoginCallback(context.Context, string, string) (*core.CallbackSubmitResult, error) {
	if s.callbackErr != nil {
		return nil, s.callbackErr
	}
	return s.callbackResult, nil
}

// loginStart / loginCaptcha allow test cases to drive the two new in-page login entry points.
func (s stubDeps) StartLogin(context.Context, string, map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	if s.loginStartErr != nil {
		return nil, s.loginStartErr
	}
	return s.loginStart, nil
}

func (s stubDeps) LoginCaptchaConfig(context.Context, string) (core.CaptchaConfig, error) {
	if s.captchaErr != nil {
		return core.CaptchaConfig{}, s.captchaErr
	}
	return s.captcha, nil
}

func newStubDeps(t *testing.T) stubDeps {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	return stubDeps{store: store, registry: registry, manager: NewCatalog(store, registry)}
}

func newService(t *testing.T) *Service {
	t.Helper()
	return newServiceWithDeps(t, newStubDeps(t))
}

func newServiceWithDeps(t *testing.T, deps stubDeps) *Service {
	t.Helper()
	pool := auth.NewPool(deps.registry, deps.ResolveCredential)
	return New(deps, deps.registry, pool, tasks.New(stubTaskDeps{deps: deps, pool: pool}))
}

func TestRegistrationDeclaresRoutes(t *testing.T) {
	service := newService(t)
	registration := service.Registration()
	found := false
	for _, route := range registration.Routes {
		if route.Path == "/plugins/free-model-hub/status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("status route missing: %#v", registration.Routes)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/console" {
		t.Fatalf("resources = %#v", registration.Resources)
	}
}

func TestHandleStatusAndUnknownRoute(t *testing.T) {
	service := newService(t)

	raw, err := service.Handle([]byte(`{"method":"GET","path":"/v0/management/plugins/free-model-hub/status"}`))
	if err != nil {
		t.Fatalf("handle status: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !strings.Contains(string(raw), `"StatusCode":200`) && !strings.Contains(string(raw), `"status_code":200`) {
		t.Fatalf("unexpected envelope: %s", raw)
	}

	raw, err = service.Handle([]byte(`{"method":"GET","path":"/plugins/free-model-hub/nope"}`))
	if err != nil {
		t.Fatalf("handle unknown: %v", err)
	}
	var notFound struct {
		Result struct {
			StatusCode int
		}
	}
	if err := json.Unmarshal(raw, &notFound); err != nil {
		t.Fatalf("decode not-found: %v", err)
	}
	if notFound.Result.StatusCode != 404 {
		t.Fatalf("unknown route should 404: %s", raw)
	}
}

type stubTaskDeps struct {
	deps stubDeps
	pool *auth.Pool
}

func (s stubTaskDeps) Registry() *core.Registry { return s.deps.registry }
func (s stubTaskDeps) Pool() *auth.Pool         { return s.pool }

// Config projects the global configuration into the minimum shape required by the task engine (isomorphic with the composition root taskConfig).
func (s stubTaskDeps) Config() tasks.Config {
	cfg := s.deps.Config()
	return tasks.Config{
		EnabledRealms: cfg.EnabledRealms,
	}
}

func (s stubTaskDeps) SaveRefreshedCredential(context.Context, string, core.Vendor, core.HostAuth, []byte, *core.Credential, string) {
}
func (s stubTaskDeps) State() state.State                { return s.deps.State() }
func (s stubTaskDeps) MutateState(fn func(*state.State)) { s.deps.MutateState(fn) }
func (s stubTaskDeps) Now() time.Time                    { return time.Now() }

// TestSettingsPayloadKeysMatchConsole locks in the consistency between "backend field names ↔ page field names".
//
// Real incident: The field names in `/settings` and the keys submitted by the page once drifted (the backend was renamed, but the page was not updated),
// resulting in changes on the page **not being saved at all** without any error being thrown.
//
// The compile stage already ensures consistency on the Go side, but HTML is a string which the compiler cannot see. Here, we pin
// every json tag of settingsUpdateRequest to the page to prevent drifting again.
func TestSettingsPayloadKeysMatchConsole(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	// Fixed fields for backward compatibility with older pages/scripts: still accepted by the backend, but no longer sent by the current page.
	deprecated := map[string]bool{
		"checkin_hours": true, "travel_hours": true, "activity_hours": true,
		"keepalive_hours": true, "blackcat_hours": true,
	}

	var req settingsUpdateRequest
	typ := reflect.TypeOf(req)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		jsonTag := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonTag == "" || jsonTag == "-" || deprecated[jsonTag] {
			continue
		}
		if !strings.Contains(page, jsonTag) {
			t.Errorf("POST /settings field %q (%s) does not appear on the console page, page changes will not be received by the backend",
				jsonTag, field.Name)
		}
	}

	// Reverse: The /status keys read by the page must truly exist in the backend payload.
	for _, key := range []string{"version", "settings", "prompt_mode"} {
		if !strings.Contains(page, key) {
			t.Errorf("/status key %q read by the page does not appear on the page", key)
		}
	}
}
