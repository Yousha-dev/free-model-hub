package admin

// This file is the single entry point for credential files:
//   - saveAuthFile: disk persistence (adding accounts, pasting callback login, enable/disable);
//   - safeAuthFilePath: all validations prior to modification or deletion.
//
// Both paths ("Add API Key account" and "Paste localhost callback to complete login") must pass
// a credential to the host. Only by going through here can we avoid duplicating "host write + fallback write"
// logic. Enable/disable and deletion work the same way: path normalization happens at the hostbridge boundary, and this only decides "can this path be modified".

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// authFileDirs is the fallback disk persistence directory when the host is unavailable:
// /root/.cli-proxy-api in containers, or the process working directory for local direct execution.
var authFileDirs = []string{"/root/.cli-proxy-api", "."}

// saveAuthFile persists a credential to disk.
//
// Prefers host.auth.save: it writes to the auths directory and hot-updates the host's in-memory registry. Only when this
// callback is unavailable does it write directly to disk (same reason as credential refresh disk persistence fallback). Failure requires both to fail—
// the caller must faithfully inform the user of "failed to save", avoiding pages showing success while nothing is on disk.
func saveAuthFile(callbackID, fileName string, raw []byte) error {
	name := strings.TrimSpace(fileName)
	if name == "" || len(raw) == 0 {
		return fmt.Errorf("credential is missing file name or content")
	}

	// hostbridge.SaveAuth is also responsible for UID case normalization: the disk fallback must write the exact same content,
	// otherwise the UID on disk would mismatch the host registration key, causing duplicate accounts on Windows.
	aligned, errSave := hostbridge.SaveAuth(callbackID, name, raw)
	if errSave == nil {
		logger.Info("saved auth file %s via host.auth.save", name)
		return nil
	}
	logger.Debug("host.auth.save failed for %s: %v", name, errSave)

	for _, dir := range authFileDirs {
		if fi, errStat := os.Stat(dir); errStat != nil || !fi.IsDir() {
			continue
		}
		if errWrite := os.WriteFile(filepath.Join(dir, name), aligned, 0o600); errWrite == nil {
			logger.Info("saved auth file %s on disk", name)
			return nil
		}
	}
	return fmt.Errorf("host.auth.save is unavailable and disk write failed (%s)", name)
}

// safeAuthFilePath validates whether a credential file belongs to this plugin and is safe to modify or delete,
// returning the absolute path.
//
// The path parameter must already be a path normalized by hostbridge (both ListAuths and AuthJSONAndPath
// outputs do this); no conversion is done here, only validation—doing the same thing at both layers
// will sooner or later lead to divergence where "some call sites forget to normalize".
//
// The order of the four checks ranges from "not worth trying" to "must actually check the disk":
//  1. The credential provided by the host must belong to this plugin; runtime-only credentials have no files (RuntimeOnly / empty path)
//     -> reject immediately;
//  2. The path must be absolute; the file must exist - **non-existence cannot be treated as success**: silently returning nil would
//     make the page show "Deleted" while the account actually still exists;
//  3. Only standard `.json` files are accepted: symlinks, directories, and other extensions are strictly rejected;
//  4. The credential inside the target file must belong to the same account (vendor + UID) as the one given by the host - path
//     normalization uses the process working directory, and files with the same name might belong to another account if bases differ.
//     Only identity fields are compared; tokens are neither read nor logged.
func safeAuthFilePath(registry *core.Registry, entry core.HostAuth, raw []byte, path string) (string, error) {
	hostCred, okHost := registry.ParseCredential(raw, hostbridge.AccountIdentity(entry), nil)
	if !okHost {
		return "", fmt.Errorf("this credential does not belong to this plugin, deletion rejected")
	}
	target := strings.TrimSpace(path)
	if entry.RuntimeOnly || target == "" {
		return "", fmt.Errorf("this account has no corresponding credential file (runtime credential), cannot delete")
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("credential path not normalized at the host boundary: %s", target)
	}
	if !strings.HasSuffix(strings.ToLower(target), ".json") {
		return "", fmt.Errorf("refusing to delete non-credential file: %s", filepath.Base(target))
	}
	info, errStat := os.Lstat(target)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return "", fmt.Errorf("credential file does not exist, may have been removed by the host: %s", filepath.Base(target))
		}
		return "", fmt.Errorf("failed to check file: %w", errStat)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing to delete symbolic link: %s", filepath.Base(target))
	}
	if info.IsDir() {
		return "", fmt.Errorf("refusing to delete directory: %s", filepath.Base(target))
	}

	onDisk, errRead := os.ReadFile(target)
	if errRead != nil {
		return "", fmt.Errorf("failed to read credential file: %w", errRead)
	}
	fileCred, okFile := registry.ParseCredential(onDisk, strings.TrimSuffix(filepath.Base(target), ".json"), nil)
	if !okFile || !sameCredentialIdentity(hostCred, fileCred) {
		return "", fmt.Errorf("target file belongs to another account, deletion rejected: %s", filepath.Base(target))
	}
	return target, nil
}

// sameCredentialIdentity determines whether two credentials point to the same account.
//
// Skip the dimension if vendor or UID cannot be retrieved (some provider credentials lack a UID): better to let it pass
// than to block a normal deletion. Only identity fields are compared; tokens are not involved.
func sameCredentialIdentity(hostCred, fileCred *core.Credential) bool {
	if hostVendor, fileVendor := hostCred.VendorIDValue(), fileCred.VendorIDValue(); hostVendor != "" && fileVendor != "" && hostVendor != fileVendor {
		return false
	}
	hostUID, fileUID := hostCred.UIDValue(), fileCred.UIDValue()
	return hostUID == "" || fileUID == "" || hostUID == fileUID
}

// setCredentialDisabled sets or clears the host's disabled flag on the credential JSON.
//
// When scanning disk, the host only checks the "disabled" field in the credential file (see file synthesizer): setting it to true
// excludes the credential from routing, and deleting the field restores it. Therefore, enabling/disabling a plugin must rely solely on this field,
// with no alternative channel.
//
// Use map[string]json.RawMessage instead of map[string]any: only add or remove one key, while the rest of the fields
// preserve their bytes as-is, and float64 round-trips will not cause integers exceeding 2^53 (upstream UID) to lose precision.
func setCredentialDisabled(raw []byte, disabled bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil || payload == nil {
		return nil, fmt.Errorf("credential is not a valid JSON object")
	}
	if disabled {
		payload["disabled"] = json.RawMessage("true")
	} else {
		delete(payload, "disabled")
	}
	updated, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return nil, errMarshal
	}
	return updated, nil
}
