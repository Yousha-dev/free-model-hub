package hostbridge

// This file encapsulates the credential callbacks provided by the host (host.auth.list / host.auth.get).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

// ---- Credential Write-Back (Host Boundary) ----

// SaveAuth hands a credential over to the host for hot-reloading (host.auth.save), returning the actually written JSON.
//
// **This is the sole path for credential write-back**: UID normalization also takes place here, and callers can simply use the return value for disk persistence,
// ensuring that the content "given to the host" and "written to disk" are identical (discrepancies would cause the host to register
// them separately by filename and UID).
func SaveAuth(callbackID, name string, storageJSON []byte) ([]byte, error) {
	payload := AlignCredentialUID(storageJSON, name)
	_, errCall := Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: payload,
	})
	if errCall != nil {
		return payload, errCall
	}
	return payload, nil
}

// AlignCredentialUID normalizes the case of the UID in the credential JSON to match the spelling of the credential filename.
//
// On Windows (where NTFS is case-insensitive), the host simultaneously registers accounts by **filename** and by the **UID** in the JSON.
// When they differ only by case (zcode-eyJh---1KC8.json vs uid zcode-eyjh---1kc8), they are treated as two separate records:
// duplicate cards appear in the console, usage is split across two auth_indexes, and the issue persists after restarts.
// auth_index is split, and still exists after restart.
//
// Only UIDs that are "case-insensitively equal but literally different" are modified—this is precisely the criterion by which the host
// derives its second spelling from the UID. Provider-native upstream UIDs (unrelated to filenames) are left untouched. Returns unmodified on parse failure:
// normalization is best-effort and must not block the write-back.
//
// Once the host-side fix (router-for-me/EasyCLIProxyAPI#517) is deployed, this function and its call in
// SaveAuth can be removed.
func AlignCredentialUID(storageJSON []byte, fileName string) []byte {
	fileID := core.FileIDFromFileName(fileName)
	if fileID == "" || len(storageJSON) == 0 {
		return storageJSON
	}
	// Use RawMessage instead of map[string]any: only replace the UID, while preserving numerical values and escapes
	// of all other fields (round-tripping float64 would cause integers exceeding 2^53 to lose precision).
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(storageJSON, &payload); errUnmarshal != nil || payload == nil {
		return storageJSON
	}
	var current string
	if errUnmarshal := json.Unmarshal(payload["uid"], &current); errUnmarshal != nil {
		return storageJSON
	}
	current = strings.TrimSpace(current)
	// Do nothing if the literal strings are already identical: EqualFold returning true only indicates "case equivalence",
	// not that the spelling is identical, so this check cannot be omitted.
	if current == "" || current == fileID || !strings.EqualFold(current, fileID) {
		return storageJSON
	}
	aligned, errMarshal := json.Marshal(fileID)
	if errMarshal != nil {
		return storageJSON
	}
	payload["uid"] = aligned
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Disable HTML escaping: the marshaler otherwise writes < > & as \u003c etc., and no fields in the credential
	// should be altered simply due to a UID normalization.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(payload); errEncode != nil {
		return storageJSON
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
}

// listHostAuths lists all credentials under this plugin name.
//
// **Must tolerate credentials hijacked by preceding plugins**: when the host iterates over plugins to parse credential files,
// it aborts the loop as soon as a plugin reports an error or claims them, and defaults the current plugin's identifier
// into the provider. Therefore, preceding plugins with overly broad ownership criteria (such as qoder2api claiming
// whenever `provider == "qoder"`) will hijack this plugin's files, causing the provider for those accounts
// to become the other party.
//
// To make matters worse, plugin priorities may be overwritten by the host: when installing or upgrading from the plugin store, the host
// rewrites the entire plugins.configs.<id> block, erasing manually added priorities and
// reverting the order back to ascending ID (qoder2api < workbuddy2api). Therefore, **priority alone cannot be relied upon**.
//
// Fallback basis: the filenames of these credential files are still workbuddy-*.json (this plugin's naming convention),
// so they belong to this plugin by name as well and are therefore claimed—otherwise the user's legitimate accounts would mysteriously vanish from the UI.
func ListAuths(ctx context.Context, callbackID string, registry *core.Registry) ([]core.HostAuth, error) {
	raw, errCall := Call(callbackID, pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, fmt.Errorf("host.auth.list: %w", errCall)
	}
	// The host may return an array or a wrapped object; both are accepted.
	var entries []core.HostAuth
	if errUnmarshal := json.Unmarshal(raw, &entries); errUnmarshal == nil {
		return filterPluginAuths(entries, registry), nil
	}
	var wrapper struct {
		Auths []core.HostAuth `json:"auths"`
		Files []core.HostAuth `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", errUnmarshal)
	}
	// Auths returned by the host are in-memory runtime instances, whereas Files are physical files on disk.
	// Merge file attributes from Files (such as ModTime and Path) into Auths by filename to avoid displaying the same credential repeatedly.
	entries = mergeAuthsAndFiles(wrapper.Auths, wrapper.Files)
	return filterPluginAuths(entries, registry), nil
}

// ResolveAuthPath normalizes the credential file path given by the host into an absolute path.
//
// The host does not necessarily provide an absolute path: EasyCLIProxyAPI configures the kernel's oauth.auth-dir as a relative path
// (default ../oauth, relative to the kernel working directory), and CLIProxyAPI writes relative paths into auth attributes when scanning the disk,
// which host.auth.list / host.auth.get then return as-is. The plugin and host share the same process and working
// directory, so the file resolved by filepath.Abs is the exact file the host actually reads and writes; this is also how the host's own
// management API DELETE /v0/management/auth-files operates.
//
// Normalization is placed strictly at the **host boundary**: Path is the common input for all path consumers in this plugin (deletion, stat for modification time,
// fallback file reading, refresh and write-back), so recalculating it independently would lead to discrepancies where "some paths are absolute
// and some are relative". Empty values are returned as-is, leaving callers to handle them as "no file".
func ResolveAuthPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	abs, errAbs := filepath.Abs(trimmed)
	if errAbs != nil {
		return trimmed
	}
	return abs
}

// mergeAuthsAndFiles merges host Auths and Files to avoid duplicate entries for the same credential.
func mergeAuthsAndFiles(auths, files []core.HostAuth) []core.HostAuth {
	if len(files) == 0 {
		return auths
	}
	if len(auths) == 0 {
		return files
	}

	out := make([]core.HostAuth, len(auths))
	copy(out, auths)
	authIdxByName := make(map[string]int, len(auths))
	for i, a := range out {
		if key := normalizeAuthKey(a.Name); key != "" {
			authIdxByName[key] = i
		}
		if key := normalizeAuthKey(a.ID); key != "" {
			authIdxByName[key] = i
		}
	}

	for _, file := range files {
		key := normalizeAuthKey(file.Name)
		if key == "" {
			key = normalizeAuthKey(file.ID)
		}
		if idx, found := authIdxByName[key]; found {
			mergeHostAuthEntry(&out[idx], file)
		} else {
			out = append(out, file)
		}
	}
	return out
}

// filterPluginAuths keeps only credentials belonging to this plugin, and deduplicates/merges duplicate entries (e.g., appearing in both Auths and Files).
//
// There are two layers of criteria, matching either is accepted:
//  1. The provider is this plugin (normal case);
//  2. The filename matches this plugin's convention (workbuddy-*.json) — used to catch credentials
//     hijacked by a preceding plugin. Relying solely on the provider would make these accounts disappear from the page without any way for the user to know why.
//
// Neither layer will mistakenly swallow other plugins' credentials: other plugins' filenames do not contain workbuddy.
func filterPluginAuths(entries []core.HostAuth, registry *core.Registry) []core.HostAuth {
	out := make([]core.HostAuth, 0, len(entries))
	indexByKey := make(map[string]int)

	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		belongs := (provider != "" && strings.EqualFold(provider, core.ProviderKey)) || fileNameBelongsToPlugin(entry.Name, registry)
		if !belongs {
			continue
		}
		entry.Path = ResolveAuthPath(entry.Path)

		// Normalized deduplication key: preferentially use Name without .json, falling back to ID
		key := normalizeAuthKey(entry.Name)
		if key == "" {
			key = normalizeAuthKey(entry.ID)
		}
		if key == "" {
			key = strings.TrimSpace(entry.AuthIndex)
		}

		if idx, exists := indexByKey[key]; exists && key != "" {
			mergeHostAuthEntry(&out[idx], entry)
			continue
		}

		if key != "" {
			indexByKey[key] = len(out)
		}
		out = append(out, entry)
	}
	return out
}

// logHostCallFailure records a host callback failure (for fallback path usage).
func logHostCallFailure(method string, err error) {
	if err == nil {
		return
	}
	logger.Debug("host callback %s failed: %v", method, err)
}

// accountIdentity returns a **unique authoritative identity** for a host account entry.
//
// This is the sole integration point for "state-by-account": check-in records, task history, and quota caches must all go through it,
// otherwise the same account would calculate different identities across different paths.
//
// Why it is needed: The host provides multiple identifiers for the same account record (ID, Name, AuthIndex),
// which are not equal (AuthIndex is a runtime index, whereas Name corresponds to the disk filename). In earlier code,
// some paths passed AuthIndex while others passed Name, so providers with "UID derived from filename"
// (Qoder/ZCode) ended up with two identities in check-in and display, where check-ins wrote to A and the page queried B,
// forever displaying "Unchecked-in". Once consolidated into this function, such discrepancies can no longer occur.
//
// Priority order: Name (disk filename, generated upon login persistence, readable and stable) → ID (host
// stable identifier, account has no Name at runtime) → AuthIndex (final fallback, guaranteeing non-emptiness only).
func AccountIdentity(entry core.HostAuth) string {
	for _, candidate := range []string{entry.Name, entry.ID, entry.AuthIndex} {
		if trimmed := core.FileIDFromFileName(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeAuthKey(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".json")
	return s
}

func mergeHostAuthEntry(target *core.HostAuth, source core.HostAuth) {
	if target.AuthIndex == "" && source.AuthIndex != "" {
		target.AuthIndex = source.AuthIndex
	}
	if target.Path == "" && source.Path != "" {
		target.Path = source.Path
	}
	if target.ModTime.IsZero() && !source.ModTime.IsZero() {
		target.ModTime = source.ModTime
	}
	if target.Label == "" && source.Label != "" {
		target.Label = source.Label
	}
	if target.Status == "" && source.Status != "" {
		target.Status = source.Status
	}
	if target.RuntimeOnly && !source.RuntimeOnly {
		target.RuntimeOnly = false
	}
}

// fileNameBelongsToPlugin determines whether a filename matches this plugin's naming convention.
//
// The criteria come from the provider registry instead of a hardcoded prefix: each provider instance's ID is its
// credential filename prefix (workbuddycn / qoderglobal / cline / opencodezen).
// It takes effect automatically when adding a new provider, eliminating the need to keep a synchronized list here.
//
// Compatibility with old prefix workbuddy: historical credential files are workbuddy-<uid>.json, which predated
// the distinction between cn and global.
func fileNameBelongsToPlugin(name string, registry *core.Registry) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".json"))
	if base == "" {
		return false
	}
	if base == "workbuddy" {
		return true
	}
	for _, vendor := range registry.Vendors() {
		id := strings.ToLower(strings.TrimSpace(vendor.ID()))
		if id == "" {
			continue
		}
		if base == id {
			return true
		}
		for _, separator := range []string{"-", "_", "."} {
			if strings.HasPrefix(base, id+separator) ||
				strings.Contains(base, separator+id) {
				return true
			}
		}
	}
	// Old prefix workbuddy (pre-partition era): workbuddy-<uid>.json and my-workbuddy.json.
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, "workbuddy"+separator) ||
			strings.Contains(base, separator+"workbuddy") {
			return true
		}
	}
	return false
}

// fetchAuthJSON retrieves the credential JSON by auth_index.
//
// The host's host.auth.get only recognizes auth_index (querying by id will fail),
// therefore callers starting from an id must first map the id to an index via the list.
func FetchAuthJSON(ctx context.Context, callbackID, authID string, registry *core.Registry) ([]byte, error) {
	index := strings.TrimSpace(authID)
	if index == "" {
		return nil, fmt.Errorf("auth id is empty")
	}
	// First try querying directly by index (most callers pass in the index).
	if raw, okFound := AuthJSON(ctx, callbackID, index); okFound {
		return raw, nil
	}
	// Fallback: map id to index and then query.
	entries, errList := ListAuths(ctx, callbackID, registry)
	if errList != nil {
		return nil, errList
	}
	for _, entry := range entries {
		if entry.ID != index && entry.Name != index {
			continue
		}
		if raw, okFound := AuthJSON(ctx, callbackID, entry.AuthIndex); okFound {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("auth %s not found", authID)
}

// getAuthJSONAndPathByIndex retrieves the credential JSON and physical file path by auth_index.
func AuthJSONAndPath(ctx context.Context, callbackID, authIndex string) ([]byte, string, bool) {
	trimmed := strings.TrimSpace(authIndex)
	if trimmed == "" {
		return nil, "", false
	}
	raw, errCall := Call(callbackID, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: trimmed})
	if errCall != nil {
		logHostCallFailure(pluginabi.MethodHostAuthGet, errCall)
		return nil, "", false
	}
	var response pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, "", false
	}
	if len(response.JSON) == 0 {
		return nil, "", false
	}
	return response.JSON, ResolveAuthPath(response.Path), true
}

// getAuthJSONByIndex retrieves the credential JSON by auth_index.
func AuthJSON(ctx context.Context, callbackID, authIndex string) ([]byte, bool) {
	raw, _, ok := AuthJSONAndPath(ctx, callbackID, authIndex)
	return raw, ok
}

// activeVendorSet returns the set of provider IDs currently having at least one available credential.
//
// Used for model list filtering: if a provider has no auth/key added, the model list neither fetches from upstream,
// and do not display this provider's models in the list either.
func ActiveVendorSet(ctx context.Context, callbackID string, registry *core.Registry) map[string]bool {
	entries, errList := ListAuths(ctx, callbackID, registry)
	if errList != nil {
		logger.Debug("activeVendorSet: list host auths failed: %v", errList)
		return map[string]bool{}
	}
	active := make(map[string]bool)
	for _, entry := range entries {
		if !entry.Usable() {
			continue
		}
		raw, _, okRaw := AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
		if !okRaw && entry.Path != "" {
			if r, err := os.ReadFile(entry.Path); err == nil && len(r) > 0 {
				raw = r
				okRaw = true
			}
		}
		if !okRaw || len(raw) == 0 {
			continue
		}
		if credential, okParse := registry.ParseCredential(raw, entry.Name, nil); okParse {
			if id := credential.VendorIDValue(); id != "" {
				active[id] = true
			}
		}
	}
	return active
}
