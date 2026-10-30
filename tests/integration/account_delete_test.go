package tests

// This file locks down compatibility for "delete account" with the host's relative paths.
//
// Real incident: EasyCLIProxyAPI configured the kernel's oauth.auth-dir as a relative path (defaulting
// to ../oauth, relative to the cpa-core working directory). When CLIProxyAPI scans the disk, it uses
// filepath.Join(auth-dir, filename) to construct the credential path without calling filepath.Abs, so
// host.auth.get / host.auth.list returns `..\oauth\<name>.json`. The plugin's original deletion
// guard rejected it outright using filepath.IsAbs, so on the Windows desktop app, clicking "Delete" only resulted
// in "Refusing to delete non-absolute path", and accounts could never be deleted.
//
// Path normalization is now uniformly handled at the hostbridge boundary (hostbridge.ResolveAuthPath).
// Here, we use the real layout of "working directory / oauth peer directory" to reproduce the host's relative auth-dir.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// traeCredentialJSON is a credential parseable by the Trae provider (ownership must be verified before deletion).
const traeCredentialJSON = `{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`

// accountDeleteResult is the response shape for POST /accounts/delete.
type accountDeleteResult struct {
	OK      bool `json:"ok"`
	Deleted int  `json:"deleted"`
	Results []struct {
		AuthID  string `json:"auth_id"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"results"`
}

// relativeAuthFile reproduces the host's relative auth-dir layout:
//
//	<root>/core    ← Process working directory (location of host cpa-core)
//	<root>/oauth   ← auth-dir (configured as ../oauth)
//
// Returns the relative path the host actually provides (`../oauth/<name>`) and the absolute path.
func relativeAuthFile(t *testing.T, name, content string) (relative, absolute string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "core")
	oauth := filepath.Join(root, "oauth")
	for _, dir := range []string{work, oauth} {
		if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
			t.Fatalf("mkdir %s: %v", dir, errMkdir)
		}
	}
	absolute = filepath.Join(oauth, name)
	if errWrite := os.WriteFile(absolute, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	chdirForTest(t, work)
	return filepath.Join("..", "oauth", name), absolute
}

// chdirForTest switches the working directory and restores it when the test ends: path normalization relies on the process working directory.
func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	previous, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("getwd: %v", errGetwd)
	}
	if errChdir := os.Chdir(dir); errChdir != nil {
		t.Fatalf("chdir %s: %v", dir, errChdir)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

// TestAccountDeleteWithAbsoluteHostPath covers standard CLIProxyAPI deployments: auth-dir defaults to
// `~/.cli-proxy-api`, which expands to an absolute path, and deletion must work as usual (before #14 this worked fine,
// but normalization changes must not break it).
func TestAccountDeleteWithAbsoluteHostPath(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "delete-absolute-1.json"
	dir := t.TempDir()
	absolute := filepath.Join(dir, name)
	if errWrite := os.WriteFile(absolute, []byte(traeCredentialJSON), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", absolute)

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/delete = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK || payload.Deleted != 1 || len(payload.Results) != 1 || !payload.Results[0].OK {
		t.Fatalf("Failed to delete absolute path: %s", response.Body)
	}
	if _, errStat := os.Stat(absolute); !os.IsNotExist(errStat) {
		t.Fatalf("Credential file should have been deleted, actual stat result: %v", errStat)
	}
}

// TestAccountDeleteResolvesHostRelativePath reproduces the incident: deletion must succeed when the host returns a relative path.
func TestAccountDeleteResolvesHostRelativePath(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "delete-relative-1.json"
	relative, absolute := relativeAuthFile(t, name, traeCredentialJSON)
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", relative)

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/delete = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK || payload.Deleted != 1 || len(payload.Results) != 1 || !payload.Results[0].OK {
		t.Fatalf("Failed to delete relative path: %s", response.Body)
	}
	if _, errStat := os.Stat(absolute); !os.IsNotExist(errStat) {
		t.Fatalf("Credential file should have been deleted, actual stat result: %v", errStat)
	}
}

// TestAccountDeleteKeepsRelativePathGuards confirms security checks remain effective after resolving relative paths:
// Non-.json files are not allowed to be deleted, preventing user files from being mistakenly deleted due to changes in host path format.
func TestAccountDeleteKeepsRelativePathGuards(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "delete-guard-1.txt"
	relative, absolute := relativeAuthFile(t, name, traeCredentialJSON)
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", relative)

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/delete = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if payload.Deleted != 0 || len(payload.Results) != 1 || payload.Results[0].OK {
		t.Fatalf("Non-.json files should not be deleted: %s", response.Body)
	}
	if _, errStat := os.Stat(absolute); errStat != nil {
		t.Fatalf("Files refused for deletion should not disappear: %v", errStat)
	}
}

// TestAccountDeleteReportsMissingFile locks down "file does not exist != already deleted".
// If the host path points elsewhere due to a different working directory base, silent success would cause the UI to display "Deleted"
// while the account still exists.
func TestAccountDeleteReportsMissingFile(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	root := t.TempDir()
	work := filepath.Join(root, "core")
	if errMkdir := os.MkdirAll(work, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	chdirForTest(t, work)
	name := "delete-missing-1.json"
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", filepath.Join("..", "oauth", name))

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if payload.Deleted != 0 || len(payload.Results) != 1 || payload.Results[0].OK {
		t.Fatalf("Should not report successful deletion when file does not exist: %s", response.Body)
	}
}
