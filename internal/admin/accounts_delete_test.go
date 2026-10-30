package admin

// This file locks down all validations before "delete account".
//
// Real incident: EasyCLIProxyAPI configured the kernel's oauth.auth-dir as a relative path (default
// ../oauth, relative to the kernel working directory). CLIProxyAPI wrote the relative path into the auth attribute during disk scanning,
// host.auth.get / host.auth.list returned it as-is, so the deletion protection received
// `..\oauth\<name>.json` and judged it as a "non-absolute path" every time — on the Windows desktop app, clicking "Delete"
// would never delete the account.
//
// Path normalization is completed at the hostbridge boundary (see hostbridge.ResolveAuthPath), here we only pin down
// whether "this path can be deleted": relative paths can be deleted, missing files must throw an error (cannot falsely report success),
// non-.json files / symlinks / directories are strictly rejected, and target files belonging to another account are rejected.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"free-model-hub/internal/core"
)

// Minimal credential parsing for deleteTestVendor: vendor / uid fields are sufficient to support ownership comparison.
type deleteTestVendor struct{ fakeVendor }

func (v *deleteTestVendor) Parse(raw []byte, _ string) (*core.Credential, error) {
	var payload struct {
		Vendor string `json:"vendor"`
		UID    string `json:"uid"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &core.Credential{VendorID: payload.Vendor, UID: payload.UID}, nil
}

func newDeleteTestRegistry(t *testing.T) *core.Registry {
	t.Helper()
	registry := core.NewRegistry()
	for _, id := range []string{"traecn", "workbuddycn"} {
		if err := registry.Register(&deleteTestVendor{fakeVendor{Meta: core.Meta{VendorID: id}}}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return registry
}

func writeAuthFile(t *testing.T, dir, name string, payload map[string]any) string {
	t.Helper()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal credential: %v", errMarshal)
	}
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write credential: %v", errWrite)
	}
	return path
}

// TestSafeAuthFileForDeleteRequiresNormalizedPath locks down the boundary contract: deletion validation only accepts
// absolute paths already normalized by hostbridge. Relative path resolution is done at the host boundary (see
// hostbridge.ResolveAuthPath), and end-to-end behavior is covered by tests/integration.
func TestSafeAuthFileForDeleteRequiresNormalizedPath(t *testing.T) {
	dir := t.TempDir()
	absolute := writeAuthFile(t, dir, "traecn-u-1.json", map[string]any{"vendor": "traecn", "uid": "u-1"})
	entry := core.HostAuth{Name: "traecn-u-1.json", AuthIndex: "1"}
	hostRaw := []byte(`{"vendor":"traecn","uid":"u-1"}`)
	registry := newDeleteTestRegistry(t)

	got, errSafe := safeAuthFilePath(registry, entry, hostRaw, absolute)
	if errSafe != nil {
		t.Fatalf("Absolute path should be allowed for deletion: %v", errSafe)
	}
	if got != absolute {
		t.Fatalf("Returned path = %q, want %q", got, absolute)
	}

	if _, errRelative := safeAuthFilePath(registry, entry, hostRaw, filepath.Base(absolute)); errRelative == nil {
		t.Fatal("Unnormalized relative paths must be rejected (normalization is the responsibility of the host boundary)")
	}
}

// TestSafeAuthFileForDeleteRejectsMissingFile locks down "non-existent != deleted".
// The path provided by the host might point elsewhere due to different working directory bases, and silent success would make the page show
// "Deleted" while the account is actually still there.
func TestSafeAuthFileForDeleteRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	entry := core.HostAuth{Name: "traecn-u-1.json", AuthIndex: "1"}
	_, errSafe := safeAuthFilePath(newDeleteTestRegistry(t), entry,
		[]byte(`{"vendor":"traecn","uid":"u-1"}`), filepath.Join(dir, "traecn-u-1.json"))
	if errSafe == nil {
		t.Fatal("Must return an error when file does not exist, cannot be treated as successful deletion")
	}
}

// TestSafeAuthFileForDeleteRejectsRuntimeCredential runtime credentials have no file to delete.
func TestSafeAuthFileForDeleteRejectsRuntimeCredential(t *testing.T) {
	entry := core.HostAuth{Name: "traecn-u-1", AuthIndex: "1", RuntimeOnly: true}
	if _, errSafe := safeAuthFilePath(newDeleteTestRegistry(t), entry,
		[]byte(`{"vendor":"traecn","uid":"u-1"}`), ""); errSafe == nil {
		t.Fatal("Runtime credentials must be refused for deletion")
	}
}

// TestSafeAuthFileForDeleteRejectsNonCredentialFiles locks down that security checks are not relaxed:
// non-.json files, directories, and symlinks are strictly prohibited from being deleted.
func TestSafeAuthFileForDeleteRejectsNonCredentialFiles(t *testing.T) {
	registry := newDeleteTestRegistry(t)
	dir := t.TempDir()
	entry := core.HostAuth{Name: "traecn-u-1.txt", AuthIndex: "1"}
	hostRaw := []byte(`{"vendor":"traecn","uid":"u-1"}`)
	payload := map[string]any{"vendor": "traecn", "uid": "u-1"}

	textFile := writeAuthFile(t, dir, "traecn-u-1.txt", payload)
	if _, errSafe := safeAuthFilePath(registry, entry, hostRaw, textFile); errSafe == nil {
		t.Fatal("Non-.json files must be refused for deletion")
	}

	subdir := filepath.Join(dir, "traecn-dir.json")
	if errMkdir := os.Mkdir(subdir, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	if _, errSafe := safeAuthFilePath(registry, entry, hostRaw, subdir); errSafe == nil {
		t.Fatal("Directories must be refused for deletion")
	}

	target := writeAuthFile(t, dir, "traecn-target.json", payload)
	link := filepath.Join(dir, "traecn-link.json")
	if errLink := os.Symlink(target, link); errLink != nil {
		t.Skipf("symlink unavailable: %v", errLink)
	}
	if _, errSafe := safeAuthFilePath(registry, entry, hostRaw, link); errSafe == nil {
		t.Fatal("Symlinks must be refused for deletion")
	}
}

// TestSafeAuthFileForDeleteRejectsForeignCredential prevents deleting the wrong account: path normalization uses
// the process working directory, and when bases are inconsistent, files with the same name may belong to another account.
func TestSafeAuthFileForDeleteRejectsForeignCredential(t *testing.T) {
	registry := newDeleteTestRegistry(t)
	dir := t.TempDir()
	path := writeAuthFile(t, dir, "traecn-u-1.json", map[string]any{"vendor": "traecn", "uid": "u-other"})
	entry := core.HostAuth{Name: "traecn-u-1.json", AuthIndex: "1"}

	if _, errSafe := safeAuthFilePath(registry, entry,
		[]byte(`{"vendor":"traecn","uid":"u-1"}`), path); errSafe == nil {
		t.Fatal("Must refuse deletion when target file belongs to another account")
	}
}
