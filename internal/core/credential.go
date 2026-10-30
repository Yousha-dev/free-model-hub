package core

import (
	"context"
	"strings"
	"sync"
)

// Credential is the normalized account credential, with **fields taking the union of all providers**.
//
// Design trade-off: Credential structures vary significantly across providers (WorkBuddy uses nested auth/account,
// Qoder uses flat device_token/refresh_token), and forcing a unified set of fields would lose information.
// Therefore, only **truly universal** fields are placed here, while provider-specific parts remain in Raw for respective Parse implementations to handle.
//
// **Concurrency convention**: All outbound request header construction must use getter methods (TokenValue, etc.) to acquire a locked snapshot,
// and must not read fields directly—the task scheduler and request forwarder concurrently refresh/read the same credential.
type Credential struct {
	mu sync.Mutex

	// VendorID is the provider instance identifier (workbuddycn, etc.), determining which upstream the request goes to.
	VendorID string
	// Region is the region identifier (cn / global).
	Region string
	// Label is the display name (nickname/email/uid, taking the first non-empty one).
	Label string
	// FileID is the **account identity**: the credential filename without the .json extension, globally unique and stable.
	//
	// Dedicated to scenarios like "persisting status by account"—check-in records, task history, quota cache.
	// Its division of labor from UID is deliberate: UID is the **upstream identity** (WorkBuddy's X-Uid and device
	// fingerprint seed, Trae's X-Uid), changing which would cause upstream to treat it as a new device; whereas FileID is only used
	// within this plugin, with consistent algorithms across vendors and no branching.
	//
	// Early versions used UID as account identity, causing providers where "UID is derived from credential content"
	// (WorkBuddy) to work normally, while providers where "UID is derived from filename" (Qoder/ZCode)
	// obtained different filenames and computed two identities across the check-in and display paths, causing the page to forever
	// display "Not Checked-in".
	FileID string
	// UID is the upstream identity identifier, used only for outbound requests (X-Uid, device fingerprint derivation, userId reporting).
	// Providers for which this is empty mean upstream does not require it.
	UID string
	// Token is the outbound credential: access token for OAuth accounts, device token for device token accounts.
	Token string
	// RefreshToken is used to refresh the Token (may be empty, indicating reliance on upstream 401 to trigger re-login).
	RefreshToken string
	// ExpiresAt is the Token expiration time (Unix seconds); 0 means unknown.
	ExpiresAt int64
	// AuthMode identifies the credential format: oauth / device / pat, etc.
	AuthMode string
	// FilePath is the source file path of the credential (written back here after refresh); empty means non-file source.
	FilePath string
	// Raw is the original credential JSON, where all provider-specific fields reside.
	//
	// Incremental merging (rather than rebuilding) is performed based on it during refresh write-backs, ensuring that user-maintained fields
	// in the auth file like disabled / prefix / proxy_url / note / weight are not lost.
	Raw map[string]any
	// Native stores the provider's own credential object (e.g., workbuddy.Credential).
	//
	// core does not understand its content nor ever access its fields—it simply allows providers to avoid repeated parsing
	// when "parsing once and using everywhere" (retrieved via type assertion).
	Native any
}

// TokenValue returns a locked snapshot of the Token.
func (c *Credential) TokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Token
}

// RefreshTokenValue returns a locked snapshot of the RefreshToken.
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// ExpiresAtValue returns a locked snapshot of the expiration time.
func (c *Credential) ExpiresAtValue() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ExpiresAt
}

// LabelValue returns a locked snapshot of the display name.
func (c *Credential) LabelValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Label
}

// FileIDValue returns a locked snapshot of the account identity (filename without .json).
func (c *Credential) FileIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.FileID
}

// SetFileID sets the account identity.
//
// Populated by each provider's Parse during credential parsing, or backed up by the root layer using the filename (see
// ensureFileID); the latter ensures no provider loses its account identity due to a missing value.
func (c *Credential) SetFileID(fileID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FileID = fileID
}

// UIDValue returns a locked snapshot of the upstream identity identifier.
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// VendorIDValue returns the provider ID of the locked snapshot.
func (c *Credential) VendorIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.VendorID
}

// RegionValue returns the region of the locked snapshot.
func (c *Credential) RegionValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Region
}

// SetRegion sets the region (host attributes can override the region declaration native to the credential).
func (c *Credential) SetRegion(region string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Region = region
}

// SetFilePath records the source file path of the credential (used for writing back after refresh).
func (c *Credential) SetFilePath(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FilePath = path
}

// ApplyTokenRefresh writes a new token under concurrent safety.
//
// Returns whether a change actually occurred — upstream sometimes returns a token identical to the old value,
// in which case disk writes should not be triggered (otherwise mtime would change meaninglessly, and users cannot tell if a refresh actually happened).
func (c *Credential) ApplyTokenRefresh(token, refreshToken string, expiresAt int64) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	if strings.TrimSpace(token) != "" && token != c.Token {
		c.Token = token
		changed = true
	}
	if strings.TrimSpace(refreshToken) != "" && refreshToken != c.RefreshToken {
		c.RefreshToken = refreshToken
		changed = true
	}
	if expiresAt > 0 && expiresAt != c.ExpiresAt {
		c.ExpiresAt = expiresAt
		changed = true
	}
	return changed
}

// CachedModel is the cached metadata for a single upstream model.
//
// Defined in core instead of state: both the provider and model catalog (admin.Catalog) use it,
// so that core does not need to reverse-depend on the persistence layer.
type CachedModel struct {
	VendorID        string   `json:"vendor_id,omitempty"`
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Realm           string   `json:"realm"`
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	SupportsImages  bool     `json:"supports_images,omitempty"`
	SupportsTool    bool     `json:"supports_tool_call,omitempty"`
	CanDisableThink bool     `json:"can_disable_thinking,omitempty"`
	OnlyReasoning   bool     `json:"only_reasoning,omitempty"`
	Description     string   `json:"description,omitempty"`
	// Credits is the upstream quota/cost multiplier text (e.g. "x0.05", "Free").
	//
	// Used only for console display: host contracts lack multiplier fields, and probe responses cannot carry them either,
	// so they are persisted separately along with model cache so the console does not need to wait for a round of upstream probes after restart.
	Credits string `json:"credits,omitempty"`
}

// This file retains attribute override rules for credential parsing.
// Ownership determination and parsing entry points are in Registry.ParseCredential.

// applyAttributeOverrides overrides credential fields with attributes passed from the host.
//
// realm is allowed to be overridden; token-related fields are not — they should not be present in attributes.
func applyAttributeOverrides(credential *Credential, attributes map[string]string) {
	if credential == nil || len(attributes) == 0 {
		return
	}
	if realm := strings.TrimSpace(attributes["realm"]); realm != "" {
		credential.SetRegion(realm)
	}
}

// NativeAdapter[T] unifies the duplicated credential lifecycle across provider adapters:
// Parse / Native / Refresh / MergeStorageJSON.
//
// Providers still retain their respective individual Match, model list, chat, and quota implementations;
// this only abstracts the parts where "the same code repeats across different providers".

// CredentialSeed is the normalized credential fields parsed by a provider.
type CredentialSeed struct {
	VendorID     string
	Region       string
	Label        string
	FileID       string
	UID          string
	Token        string
	RefreshToken string
	ExpiresAt    int64
	AuthMode     string
}

// NativeRefresh is the result of a refresh operation.
type NativeRefresh[T any] struct {
	Native       T
	Token        string
	RefreshToken string
	ExpiresAt    int64
	Changed      bool
}

// NativeSpec is the hook collection for NativeAdapter.
type NativeSpec[T any] struct {
	Meta    Meta
	Env     Env
	Extract func(*Credential) (T, bool)

	// ParseSeed parses the native credential and returns normalized fields.
	ParseSeed func(raw []byte, fileName string) (CredentialSeed, T, error)
	// RefreshNative refreshes the credential; nil indicates the provider does not require refreshing.
	RefreshNative func(ctx context.Context, native T) (NativeRefresh[T], error)
	// MergeNative merges the new token back into the original JSON; nil indicates no write-back.
	MergeNative func(original []byte, native T) ([]byte, error)
}

// NativeAdapter is the credential lifecycle base class for core.Vendor.
type NativeAdapter[T any] struct {
	Meta
	Env  Env
	spec NativeSpec[T]
}

// NewNativeAdapter constructs an adapter base class.
func NewNativeAdapter[T any](spec NativeSpec[T]) *NativeAdapter[T] {
	return &NativeAdapter[T]{Meta: spec.Meta, Env: spec.Env, spec: spec}
}

// Parse parses credential JSON into core.Credential.
func (a *NativeAdapter[T]) Parse(raw []byte, fileName string) (*Credential, error) {
	seed, native, errParse := a.spec.ParseSeed(raw, fileName)
	if errParse != nil {
		return nil, errParse
	}
	if seed.VendorID == "" {
		seed.VendorID = a.ID()
	}
	if seed.Region == "" {
		seed.Region = a.Region()
	}
	if seed.FileID == "" {
		seed.FileID = FileIDFromFileName(fileName)
	}
	return &Credential{
		VendorID:     seed.VendorID,
		Region:       seed.Region,
		Label:        seed.Label,
		FileID:       seed.FileID,
		UID:          seed.UID,
		Token:        seed.Token,
		RefreshToken: seed.RefreshToken,
		ExpiresAt:    seed.ExpiresAt,
		AuthMode:     seed.AuthMode,
		Native:       native,
	}, nil
}

// Native retrieves the provider's native credential object.
func (a *NativeAdapter[T]) Native(cred *Credential) (T, error) {
	var zero T
	if a.spec.Extract == nil {
		return zero, NewPluginError("credential_missing", "vendor adapter has no native extractor", 401)
	}
	if cred == nil {
		return zero, NewPluginError("credential_missing", "credential is nil", 401)
	}
	native, okNative := a.spec.Extract(cred)
	if !okNative || any(native) == nil {
		return zero, NewPluginError("credential_missing", "credential was not parsed by the "+a.ID()+" vendor", 401)
	}
	return native, nil
}

// Refresh validates/refreshes the credential; treated as no-refresh-needed if no hook is provided.
func (a *NativeAdapter[T]) Refresh(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if a.spec.RefreshNative == nil {
		return cred, false, nil
	}
	native, errNative := a.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	result, errRefresh := a.spec.RefreshNative(ctx, native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if result.Changed {
		cred.ApplyTokenRefresh(result.Token, result.RefreshToken, result.ExpiresAt)
		cred.Native = result.Native
	}
	return cred, result.Changed, nil
}

// MergeStorageJSON merges the refreshed token back into the original credential JSON.
func (a *NativeAdapter[T]) MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	if a.spec.MergeNative == nil {
		return original, nil
	}
	native, errNative := a.Native(cred)
	if errNative != nil {
		return original, errNative
	}
	return a.spec.MergeNative(original, native)
}
