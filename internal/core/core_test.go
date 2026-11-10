package core

// This file covers the shared contracts of core: attribution determination, registry, model key protocol, error mapping,
// optional capability assertions, and common helpers.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
)

// fakeVendor is a minimal usable Vendor implementation (implementing only required capabilities).
//
// Content sniffing matches by marker: the registry is global and registrations accumulate between tests,
// so a fake provider that "always returns true" would pollute subsequent test cases.
type fakeVendor struct {
	Meta
	marker string
	models []pluginapi.ModelInfo
}

func (v *fakeVendor) Match(_ string, _ string, raw map[string]any) bool {
	return v.marker != "" && GetString(raw, "test_marker") == v.marker
}

func (v *fakeVendor) Parse(raw []byte, fileName string) (*Credential, error) {
	return &Credential{VendorID: v.ID(), FileID: fileName, Native: string(raw)}, nil
}

func (v *fakeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return v.models, nil
}

func (v *fakeVendor) Execute(context.Context, *Credential, *ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}

func (v *fakeVendor) ExecuteStream(context.Context, *Credential, *ExecuteRequest, StreamSink) error {
	return nil
}

func newTestRegistry(t *testing.T, vendors ...Vendor) *Registry {
	t.Helper()
	registry := NewRegistry()
	for _, vendor := range vendors {
		if errRegister := registry.Register(vendor); errRegister != nil {
			t.Fatalf("register %s: %v", vendor.ID(), errRegister)
		}
	}
	return registry
}

func TestResolveVendorPrefersVendorField(t *testing.T) {
	registry := newTestRegistry(t,
		&fakeVendor{Meta: Meta{VendorID: "testfield"}},
		&fakeVendor{Meta: Meta{VendorID: "testsniff"}, marker: "sniff"},
	)

	got, ok := registry.Resolve("whatever.json", "", map[string]any{VendorKey: "testfield"})
	if !ok || got.ID() != "testfield" {
		t.Fatalf("vendor field resolution = %v %v, want testfield", got, ok)
	}
}

func TestResolveVendorFallsBackToFileNameThenSniff(t *testing.T) {
	registry := newTestRegistry(t,
		&fakeVendor{Meta: Meta{VendorID: "testfile"}},
		&fakeVendor{Meta: Meta{VendorID: "testsniffonly"}, marker: "sniffonly"},
	)

	got, ok := registry.Resolve("testfile-abc.json", "", map[string]any{})
	if !ok || got.ID() != "testfile" {
		t.Fatalf("file name resolution = %v %v, want testfile", got, ok)
	}

	got, ok = registry.Resolve("acc.json", "", map[string]any{"test_marker": "sniffonly"})
	if !ok || got.ID() != "testsniffonly" {
		t.Fatalf("sniff resolution = %v %v, want testsniffonly", got, ok)
	}
}

func TestResolveVendorRejectsUnknown(t *testing.T) {
	registry := newTestRegistry(t)
	if _, ok := registry.Resolve("zzz.json", "", map[string]any{"foo": "bar"}); ok {
		t.Fatal("unknown credential must not resolve")
	}
}

func TestResolveVendorFilenamePrecedence(t *testing.T) {
	registry := newTestRegistry(t,
		&fakeVendor{Meta: Meta{VendorID: "cline"}},
		&fakeVendor{Meta: Meta{VendorID: "github"}},
		&fakeVendor{Meta: Meta{VendorID: "workbuddyglobal"}},
		&fakeVendor{Meta: Meta{VendorID: "workbuddy"}},
	)
	for _, tc := range []struct{ filename, explicit, want string }{
		{"cline-fixture@github.example.json", "", "cline"},
		{"github-fixture-cline.json", "", "github"},
		{"cline-fixture-workbuddyglobal.json", "", "cline"},
		{"cline-fixture.json", "github", "github"},
		{"workbuddyglobal-account.json", "", "workbuddyglobal"},
		{"legacy-cline.json", "", "cline"},
	} {
		got, ok := registry.Resolve(tc.filename, "", map[string]any{VendorKey: tc.explicit})
		if !ok || got.ID() != tc.want {
			t.Errorf("Resolve(%q, %q) = %v, %v; want %s", tc.filename, tc.explicit, got, ok, tc.want)
		}
	}
}

func TestRegisterVendorRejectsDuplicate(t *testing.T) {
	registry := newTestRegistry(t, &fakeVendor{Meta: Meta{VendorID: "testdup"}})
	if errDuplicate := registry.Register(&fakeVendor{Meta: Meta{VendorID: "testdup"}}); errDuplicate == nil {
		t.Fatal("duplicate vendor registration must fail")
	}
}

func TestFileNameForAndSanitize(t *testing.T) {
	if got := FileNameFor("Vendor", "a/b c"); got != "vendor-a-b-c.json" {
		t.Fatalf("FileNameFor = %q", got)
	}
	if got := SanitizeFileComponent("../etc/passwd"); got == "" || got == "../etc/passwd" {
		t.Fatalf("SanitizeFileComponent must strip unsafe characters, got %q", got)
	}
}

// TestFileNameForFallsBackToStableFingerprint locks down "filename fallback when UID cannot be retrieved":
// it must be the credential fingerprint rather than a fixed "account", otherwise the second account would silently overwrite the first (#55).
func TestFileNameForFallsBackToStableFingerprint(t *testing.T) {
	first := FileNameFor("autoclawcn", "", "token-a")
	again := FileNameFor("autoclawcn", "", "token-a")
	other := FileNameFor("autoclawcn", "", "token-b")
	if first != again {
		t.Fatalf("Same credential generated different filenames twice: %q / %q (re-logging in will cause duplicate accounts)", first, again)
	}
	if first == other {
		t.Fatalf("Different credentials got the same filename: %q (later logged-in account will overwrite the previous one)", first)
	}
	if strings.Contains(first, "account") {
		t.Fatalf("FileNameFor returned a fixed placeholder name: %q", first)
	}
	if !strings.HasPrefix(first, "autoclawcn-t") {
		t.Fatalf("Fingerprint filename lacks the t prefix (will collide with real uid): %q", first)
	}
	// When an identifier is present, the seed is not involved, behaving identically to before the fallback was added.
	if got := FileNameFor("autoclawcn", "u1", "token-a"); got != "autoclawcn-u1.json" {
		t.Fatalf("When UID exists FileNameFor = %q, want autoclawcn-u1.json", got)
	}
	// When neither an identifier nor a seed is present, only uniqueness can be guaranteed: each account falls back to its own file without sharing a placeholder name.
	noneA, noneB := FileNameFor("autoclawcn", ""), FileNameFor("autoclawcn", "")
	if noneA == noneB || strings.Contains(noneA, "account") {
		t.Fatalf("Without identifier or seed it should fall back to a random string: %q / %q", noneA, noneB)
	}
}

// TestTokenFingerprint locks down fingerprint length and stability (filename suffixes depend on it).
func TestTokenFingerprint(t *testing.T) {
	if got := TokenFingerprint("   "); got != "" {
		t.Fatalf("Fingerprint of empty token = %q, want empty", got)
	}
	first := TokenFingerprint("token-a")
	if len(first) != 12 {
		t.Fatalf("Fingerprint length = %d, want 12 (%q)", len(first), first)
	}
	if first != TokenFingerprint(" token-a ") {
		t.Fatalf("Fingerprint should trim leading and trailing whitespace: %q", first)
	}
	if first == TokenFingerprint("token-b") {
		t.Fatal("Different tokens produced the same fingerprint")
	}
}

// TestFileIDFromFileNameTrimsExtensionCaseInsensitively locks down case-insensitive extension handling.
//
// On Windows, x.JSON and x.json are the same file; leaving the extension in the identity would cause the same account
// to be counted as two in deduplication, check-in records, and uid normalization.
func TestFileIDFromFileNameTrimsExtensionCaseInsensitively(t *testing.T) {
	cases := map[string]string{
		"zcode-AbC.json":     "zcode-AbC",
		"zcode-AbC.JSON":     "zcode-AbC",
		"  zcode-AbC.json  ": "zcode-AbC",
		"zcode-AbC":          "zcode-AbC",
		".json":              ".json",
		"":                   "",
	}
	for fileName, want := range cases {
		if got := FileIDFromFileName(fileName); got != want {
			t.Fatalf("FileIDFromFileName(%q) = %q, want %q", fileName, got, want)
		}
	}
}

func TestSplitModelIDAndModelKey(t *testing.T) {
	registry := newTestRegistry(t, &fakeVendor{Meta: Meta{VendorID: "testmodel"}})
	if prefix, bare := registry.SplitModelID("testmodel:glm-5.2"); prefix != "testmodel" || bare != "glm-5.2" {
		t.Fatalf("SplitModelID = %q %q", prefix, bare)
	}
	if prefix, bare := registry.SplitModelID("gpt-5.5:turbo"); prefix != "" || bare != "gpt-5.5:turbo" {
		t.Fatalf("unknown prefix must not split: %q %q", prefix, bare)
	}
	if got := registry.ModelKeyFor("testmodel", "testmodel:glm-5.2"); got != "testmodel:glm-5.2" {
		t.Fatalf("ModelKeyFor = %q", got)
	}
}

func TestParseCredentialFillsFileIDAndAttributes(t *testing.T) {
	registry := newTestRegistry(t, &noFileIDVendor{Meta: Meta{VendorID: "testnofileid"}})

	credential, ok := registry.ParseCredential([]byte(`{"vendor":"testnofileid"}`), "testnofileid-abc", map[string]string{"realm": "global"})
	if !ok {
		t.Fatal("credential should parse")
	}
	if got := credential.FileIDValue(); got != "testnofileid-abc" {
		t.Fatalf("FileID = %q", got)
	}
	if got := credential.RegionValue(); got != "global" {
		t.Fatalf("region override = %q", got)
	}
}

type noFileIDVendor struct{ Meta }

func (v *noFileIDVendor) Match(_ string, _ string, raw map[string]any) bool {
	return GetString(raw, "vendor") == "testnofileid"
}
func (v *noFileIDVendor) Parse(_ []byte, _ string) (*Credential, error) {
	return &Credential{VendorID: "testnofileid"}, nil
}
func (v *noFileIDVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *noFileIDVendor) Execute(context.Context, *Credential, *ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *noFileIDVendor) ExecuteStream(context.Context, *Credential, *ExecuteRequest, StreamSink) error {
	return nil
}

func TestMaskToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"short", "*****"},
		{"12345678", "********"},
		{"1234567890abcdef", "1234********cdef"},
	}
	for _, tc := range cases {
		if got := MaskToken(tc.in); got != tc.want {
			t.Fatalf("MaskToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJWTHelpers(t *testing.T) {
	// eyJ… payload = {"exp":1791529021,"user_id":"7591340"}
	token := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3OTE1MjkwMjEsInVzZXJfaWQiOiI3NTkxMzQwIn0.sig"
	if got := JWTExpiry(token); got != 1791529021 {
		t.Fatalf("JWTExpiry = %d", got)
	}
	if got := JWTUserID(token); got != "7591340" {
		t.Fatalf("JWTUserID = %q", got)
	}
	if got := JWTExpiry("not-a-jwt"); got != 0 {
		t.Fatalf("JWTExpiry(garbage) = %d", got)
	}
	if got := JWTUserID("not-a-jwt"); got != "" {
		t.Fatalf("JWTUserID(garbage) = %q", got)
	}
}

func TestJSONHelpers(t *testing.T) {
	payload := map[string]any{
		"a":          "  x  ",
		"b":          "",
		"c":          float64(1700000000000),
		"nested":     map[string]any{"k": "v"},
		"legacy_key": "y",
	}
	if got := GetString(payload, "missing", "b", "a"); got != "x" {
		t.Fatalf("GetString = %q", got)
	}
	if got := GetMap(payload, "nested"); got["k"] != "v" {
		t.Fatalf("GetMap = %v", got)
	}
	if got := GetInt64(payload, "c"); got != 1700000000000 {
		t.Fatalf("GetInt64 = %d", got)
	}
	if got := GetUnixSeconds(payload, "c"); got != 1700000000 {
		t.Fatalf("GetUnixSeconds = %d", got)
	}
	if got := FirstNonEmpty("", "  ", "v"); got != "v" {
		t.Fatalf("FirstNonEmpty = %q", got)
	}
}

func TestVendorFieldVerdict(t *testing.T) {
	if decided, claimed := VendorFieldVerdict(map[string]any{"vendor": "other"}, "mine"); !decided || claimed {
		t.Fatal("foreign vendor must be rejected")
	}
	if decided, claimed := VendorFieldVerdict(map[string]any{"vendor": "mine"}, "mine"); !decided || !claimed {
		t.Fatal("own vendor must be claimed")
	}
	if decided, _ := VendorFieldVerdict(map[string]any{"vendor": "family"}, "mine", "family"); decided {
		t.Fatal("alias must defer to other evidence")
	}
	if decided, _ := VendorFieldVerdict(map[string]any{}, "mine"); decided {
		t.Fatal("missing vendor must defer")
	}
}

// testUpstreamError is a classified error used for error mapping tests.
type testUpstreamError struct {
	status    int
	retryable bool
	kind      string
	message   string
}

func (e *testUpstreamError) Error() string        { return e.message }
func (e *testUpstreamError) HTTPStatus() int      { return e.status }
func (e *testUpstreamError) Retryable() bool      { return e.retryable }
func (e *testUpstreamError) UpstreamKind() string { return e.kind }

func TestToPluginError(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantRetry  bool
	}{
		{&testUpstreamError{status: 401, kind: "credential", message: "bad"}, 401, false},
		{&testUpstreamError{status: 429, retryable: true, kind: "rate_limit", message: "slow"}, 429, true},
		{&testUpstreamError{status: 503, retryable: true, kind: "transient", message: "down"}, 503, true},
		{errors.New("plain"), http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		err := ToPluginError(tc.err)
		pluginErr, okPlugin := err.(*PluginError)
		if !okPlugin {
			t.Fatalf("%v -> %T, want *PluginError", tc.err, err)
		}
		if pluginErr.HTTPStatus != tc.wantStatus || pluginErr.Retryable != tc.wantRetry {
			t.Fatalf("%v -> status=%d retry=%v", tc.err, pluginErr.HTTPStatus, pluginErr.Retryable)
		}
	}
	if ToPluginError(nil) != nil {
		t.Fatal("nil must map to nil")
	}
}

// quotaVendor implements only quota capabilities, used to verify capability assertions.
type quotaVendor struct{ fakeVendor }

func (v *quotaVendor) Quota(context.Context, *Credential) (*pluginapi.QuotaFetchResponse, error) {
	return &pluginapi.QuotaFetchResponse{}, nil
}

func TestUnsupportedErrors(t *testing.T) {
	vendor := &fakeVendor{Meta: Meta{VendorID: "x", VendorName: "X"}}
	quotaErr := QuotaUnsupported(vendor)
	if pluginErr, okPlugin := quotaErr.(*PluginError); !okPlugin || pluginErr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("quota unsupported = %v", quotaErr)
	}
	loginErr := LoginUnsupported(vendor)
	if pluginErr, okPlugin := loginErr.(*PluginError); !okPlugin || pluginErr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("login unsupported = %v", loginErr)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("empty payload = %d", got)
	}
	if got := EstimateTokens([]byte("123456")); got != 2 {
		t.Fatalf("estimate = %d", got)
	}
}

func TestModelsForFallbackAndRefreshFallback(t *testing.T) {
	models := []pluginapi.ModelInfo{{ID: "m1"}}
	vendor := &fakeVendor{Meta: Meta{VendorID: "fallback"}, models: models}

	// Falls back to StaticModels when ModelsProvider is not implemented.
	got, errModels := ModelsFor(vendor, context.Background(), &Credential{})
	if errModels != nil || len(got) != 1 {
		t.Fatalf("ModelsFor fallback = %v %v", got, errModels)
	}
	// Empty credentials do not register models.
	if gotNil, _ := ModelsFor(vendor, context.Background(), nil); gotNil != nil {
		t.Fatalf("nil credential must yield nil models")
	}
	// Unimplemented Refresher is considered verified.
	cred := &Credential{}
	updated, refreshed, errRefresh := RefreshVendor(vendor, context.Background(), cred)
	if errRefresh != nil || refreshed || updated != cred {
		t.Fatalf("RefreshVendor fallback = %v %v %v", updated, refreshed, errRefresh)
	}
}

func TestFormatRate(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"x0.71 credits", "0.71X"},
		{"x0.05", "0.05X"},
		{"0.08", "0.08X"},
		{"0.08X", "0.08X"},
		{"0.5x", "0.50X"},
		{"1", "1.00X"},
		{"免费", "免费"},
		{"订阅", "订阅"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := FormatRate(tc.input); got != tc.want {
			t.Errorf("FormatRate(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCapabilityAssertions(t *testing.T) {
	plain := &fakeVendor{Meta: Meta{VendorID: "plain"}}
	if _, okQuota := As[QuotaProvider](plain); okQuota {
		t.Fatal("plain vendor must not implement quota")
	}

	withQuota := &quotaVendor{fakeVendor{Meta: Meta{VendorID: "withquota"}}}
	if _, okQuota := As[QuotaProvider](withQuota); !okQuota {
		t.Fatal("quota vendor must implement quota")
	}
}
