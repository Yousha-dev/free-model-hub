package core

// This file provides an explicit provider registry.
//
// Registry is constructed by the composition root and injected into app / admin / tasks / auth and providers.
// Package-level global registries are no longer used, so multiple App instances in the same process are isolated from each other,
// and tests do not need ResetForTest.

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// Registry is an ordered registry of provider instances.
type Registry struct {
	mu    sync.RWMutex
	order []string
	byID  map[string]Vendor
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Vendor{}}
}

// Register registers a provider instance; duplicate or empty IDs return an error.
func (r *Registry) Register(v Vendor) error {
	if v == nil {
		return NewPluginError("vendor_invalid", "nil vendor", 500)
	}
	id := strings.ToLower(strings.TrimSpace(v.ID()))
	if id == "" {
		return NewPluginError("vendor_invalid", "vendor ID is empty", 500)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[id]; exists {
		return NewPluginError("vendor_duplicate", "duplicate vendor ID: "+id, 500)
	}
	r.byID[id] = v
	r.order = append(r.order, id)
	return nil
}

// Vendors returns all providers (in registration order).
func (r *Registry) Vendors() []Vendor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Vendor, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// ByID gets a provider by ID.
func (r *Registry) ByID(id string) (Vendor, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.byID[strings.ToLower(strings.TrimSpace(id))]
	return v, ok
}

// Resolve determines the credential owner in the order of vendor field -> filename prefix -> content sniffing.
func (r *Registry) Resolve(fileName, provider string, raw map[string]any) (Vendor, bool) {
	if r == nil {
		return nil, false
	}
	if raw != nil {
		if rawVendor, okVendor := raw[VendorKey].(string); okVendor {
			if v, ok := r.ByID(rawVendor); ok {
				return v, true
			}
		}
	}

	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		r.mu.RLock()
		ids := make([]string, len(r.order))
		copy(ids, r.order)
		r.mu.RUnlock()
		sort.Slice(ids, func(i, j int) bool {
			if len(ids[i]) != len(ids[j]) {
				return len(ids[i]) > len(ids[j])
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids {
			if base == id || strings.HasPrefix(base, id+"-") || strings.HasPrefix(base, id+"_") || strings.HasPrefix(base, id+".") {
				return r.ByID(id)
			}
		}
		// Preserve legacy embedded-marker attribution only when no leading vendor matched.
		for _, id := range ids {
			if FileNameMatchesConvention(base, id) {
				return r.ByID(id)
			}
		}
	}

	for _, v := range r.Vendors() {
		if v.Match(fileName, provider, raw) {
			return v, true
		}
	}
	return nil, false
}

// ParseCredential attributes and parses a credential, while also falling back to the account identity.
func (r *Registry) ParseCredential(raw []byte, fileNameHint string, attributes map[string]string) (*Credential, bool) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false
	}
	fileName := strings.TrimSpace(fileNameHint)
	if fileName != "" && !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}
	vendor, okVendor := r.Resolve(fileName, attributes["provider"], payload)
	if !okVendor {
		return nil, false
	}
	credential, errParse := vendor.Parse(raw, fileName)
	if errParse != nil {
		return nil, false
	}
	if credential.FileIDValue() == "" {
		credential.SetFileID(FileIDFromFileName(fileName))
	}
	applyAttributeOverrides(credential, attributes)
	return credential, true
}

// SplitModelID splits the model ID; prefixes are stripped only for known providers or regional aliases.
func (r *Registry) SplitModelID(modelID string) (vendorID, bareModel string) {
	trimmed := strings.TrimSpace(modelID)
	index := strings.Index(trimmed, ":")
	if index < 0 {
		return "", trimmed
	}
	prefix := strings.ToLower(strings.TrimSpace(trimmed[:index]))
	if _, ok := r.ByID(prefix); !ok && !isRegionAlias(prefix) {
		return "", trimmed
	}
	return prefix, trimmed[index+1:]
}

// ModelKeyFor generates the model disable key <vendorID>:<bareName>.
func (r *Registry) ModelKeyFor(vendorID, modelID string) string {
	_, bare := r.SplitModelID(modelID)
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(bare)
}

// FileNameFor generates <vendor>-<identifier>.json.
//
// When the identifier is empty, it falls back to a stable fingerprint of seeds (usually the access token), and must **not** fall back to a fixed
// placeholder name: for providers where the upstream does not provide a uid (e.g., AutoClaw CN token is not a JWT), once they fall into
// the same fixed filename, the second account will silently overwrite the first.
//
// The fingerprint is stable for the same credential (re-login overwrites the same file) and different for different credentials; when neither is available,
// it can only fall back to a random string - which only ensures no mutual overwriting, not re-login idempotency. The caller must pass in the identity field
// (uid / userId / email / phone number) when available.
func FileNameFor(vendorID, identifier string, seeds ...string) string {
	vendorID = strings.ToLower(strings.TrimSpace(vendorID))
	identifier = SanitizeFileComponent(identifier)
	if identifier == "" {
		for _, seed := range seeds {
			if fingerprint := TokenFingerprint(seed); fingerprint != "" {
				// "t" prefix: The fingerprint itself is a hex string; without a prefix, it would collide with a real uid.
				identifier = "t" + fingerprint
				break
			}
		}
	}
	if identifier == "" {
		identifier = RandomHex(8)
	}
	return vendorID + "-" + identifier + ".json"
}

// FileIDFromFileName returns the account identity from the credential filename with .json removed.
//
// Extensions are stripped case-insensitively: X.JSON and x.json modified by users on Windows are the same
// file; if the extension remains in the identity, the same account would be counted as two.
func FileIDFromFileName(fileName string) string {
	trimmed := strings.TrimSpace(fileName)
	const ext = ".json"
	if len(trimmed) > len(ext) && strings.EqualFold(trimmed[len(trimmed)-len(ext):], ext) {
		return trimmed[:len(trimmed)-len(ext)]
	}
	return trimmed
}

// SanitizeFileComponent keeps only safe filename characters and truncates it.
func SanitizeFileComponent(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), "-.@")
}

func isRegionAlias(prefix string) bool {
	switch prefix {
	case "cn", "global":
		return true
	}
	return false
}
