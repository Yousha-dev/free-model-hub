package hostbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"free-model-hub/internal/core"
)

// TestListHostAuthsClaimsHijackedCredentials verifies that credentials hijacked by preceding plugins are still listed.
//
// When the host iterates through plugins to resolve credentials, it stops the loop once any plugin returns an error or claims them, and
// falls back to filling the provider with the "identifier of the currently queried plugin". A preceding plugin with an overly broad
// ownership check would hijack this plugin's files (the provider becomes the other one), and filtering solely by provider would make these
// accounts vanish from the page out of nowhere. Since the filename still follows this plugin's convention, it falls back to claiming by filename.
func TestListHostAuthsClaimsHijackedCredentials(t *testing.T) {
	registry := core.NewRegistry()
	if !fileNameBelongsToPlugin("workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", registry) {
		t.Fatal("workbuddy-<uuid>.json must be recognized by file name")
	}
	if !fileNameBelongsToPlugin("my-workbuddy.json", registry) {
		t.Fatal("file name containing the workbuddy segment must be recognized")
	}
	// Another party's filename must not be claimed, otherwise someone else's account would be treated as one's own.
	for _, foreign := range []string{"qoder-login-x.json", "claude-1.json", "codex-a@b.com.json", "auth.json"} {
		if fileNameBelongsToPlugin(foreign, registry) {
			t.Fatalf("foreign file %q must not be claimed", foreign)
		}
	}

	// Provider was hijacked (qoder) but the filename belongs to this plugin: it must still be listed.
	entries := filterPluginAuths([]core.HostAuth{
		{Name: "workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", Provider: "qoder", AuthIndex: "1"},
		{Name: "workbuddy-110693a0-d32d-4d81-8a0a-7a3445069f96.json", Provider: "workbuddy", AuthIndex: "2"},
		{Name: "qoder-login-f34c07f7.json", Provider: "qoder", AuthIndex: "3"},
	}, registry)
	if len(entries) != 2 {
		t.Fatalf("want 2 workbuddy credentials (one hijacked), got %d: %+v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Provider == "qoder" && strings.Contains(entry.Name, "qoder-login") {
			t.Fatalf("foreign credential leaked in: %+v", entry)
		}
	}
}

// TestResolveAuthPathNormalizesHostPath locks host path conversion to once at the boundary.
//
// The host does not necessarily provide an absolute path: EasyCLIProxyAPI configures oauth.auth-dir as ../oauth,
// and the relative paths obtained from disk scanning are returned as-is via host.auth.list / host.auth.get. All path consumers
// (deletion, stat, file read fallback, refresh write-back) must use the result here without converting it individually again.
func TestResolveAuthPathNormalizesHostPath(t *testing.T) {
	cwd, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("getwd: %v", errGetwd)
	}
	absolute := filepath.Join(cwd, "workbuddy-1.json")
	relative := filepath.Join("..", "oauth", "traecn-1.json")
	resolved := filepath.Clean(filepath.Join(cwd, "..", "oauth", "traecn-1.json"))

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"绝对路径原样保留", absolute, filepath.Clean(absolute)},
		{"相对路径按工作目录解析", relative, resolved},
		{"空路径仍为空", "  ", ""},
		{"首尾空白被去掉", " " + absolute + "\n", filepath.Clean(absolute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveAuthPath(tc.in)
			if got != tc.want {
				t.Fatalf("ResolveAuthPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
