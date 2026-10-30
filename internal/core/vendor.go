package core

import (
	"context"
	"time"

	"free-model-hub/cpasdk/pluginapi"
)

// Package core is the vendor-agnostic skeleton of this plugin: shared types, vendor interfaces, and registry.
//
// Layering discipline:
//   - core does not import any vendor packages (internal/vendors/*), nor package main;
//   - Vendor-specific logic (endpoints, request headers, fingerprints, model catalogs, check-in flows) must be implemented
//     in internal/vendors/<name> and called by the root layer via the Vendor interface;
//   - The root layer (package main) is the ABI adaptation layer, responsible for translating host RPCs into Vendor calls.
//
// Why this layer is needed: The host ABI restricts a plugin process to declaring a single provider key
// (see ProviderKey description), so multiple vendors must coexist within a single plugin. Without
// this abstraction, adding every new vendor would require modifying a switch statement in the root capability file.

// ProviderKey is the provider key occupied by this plugin in CPA, **shared by all vendors**.
//
// The host's auth.identifier returns only a single string, is called once during registration and cached permanently, and must
// strictly equal the auth file's type field to claim that file (internal/pluginhost/auth_provider.go's
// authProviderRecord). ModelsForAuth also uses the same value to match auth.Provider;
// if it doesn't match, it is skipped (the account is unavailable).
//
// Therefore, vendor differentiation falls on the **credential filename prefix** (workbuddycn-<uid>.json) and the vendor
// field inside the file, rather than the provider key.
const ProviderKey = "freehub"

// VendorKey is the field name inside the credential that identifies the vendor.
//
// Redundancy with the filename prefix is deliberate: the filename lets users see the affiliation at a glance, while the vendor field ensures
// determination does not depend on file naming (it still belongs correctly if the user renames the file manually).
const VendorKey = "vendor"

// FormatChatCompletions is the only protocol format declared and supported by this plugin.
//
// All vendors uniformly handle chat-completions only: Claude / Codex / Gemini clients are translated by
// the host into chat-completions and then passed to the plugin. Re-implementing protocol conversion in the plugin would produce
// inconsistent framing behavior on both sides, and both sets of code would need to be maintained alongside upstream protocol changes.
const FormatChatCompletions = "chat-completions"

// MaxAccountsPerVendor is the maximum number of accounts allowed to be added for a single vendor.
const MaxAccountsPerVendor = 10

// Vendor is the **required** contract for a vendor instance (= vendor protocol × region).
//
// Only capabilities that every provider must possess are included: identity, credential ownership/parsing, model list, chat execution.
// Remaining capabilities (login, refresh, quota, check-in, tasks, configuration, storage merging, page model details)
// are all optional small interfaces (see capabilities.go), type-asserted by the caller on demand—
// Go lacks Java's default methods, so unsupported vendors simply do not implement them without writing empty methods.
//
// Convention: implementations must be stateless and concurrent-safe—the host will call these methods concurrently.
type Vendor interface {
	// ID is the vendor instance identifier, and also the credential filename prefix (workbuddycn, etc.).
	ID() string
	// Name is the management console display name (such as "WorkBuddy CN").
	Name() string
	// Region is the region identifier: cn / global.
	Region() string
	// ModelPrefix is the ID prefix when registering models; empty means using bare names.
	ModelPrefix() string

	// Match determines whether a credential file belongs to this instance.
	//
	// **Must be strict**: The host will feed all non-built-in format credentials to plugins sequentially. Mistakenly swallowing another's
	// will cause the host to overwrite the other's account with this plugin's structure (silently).
	Match(fileName, provider string, raw map[string]any) bool
	// Parse parses the credential JSON into a normalized credential.
	Parse(raw []byte, fileName string) (*Credential, error)

	// StaticModels returns all models of this vendor (for /v1/models display).
	StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error)

	// Execute executes a non-streaming chat.
	Execute(ctx context.Context, cred *Credential, req *ExecuteRequest) (*pluginapi.ExecutorResponse, error)
	// ExecuteStream performs a streaming chat, delivering chunks to the host via sink.
	//
	// It only produces the upstream's raw chunk content (naked SSE data payload), without `data: ` wrapping
	// and multi-protocol framing—which are completed by the host according to the declared format.
	ExecuteStream(ctx context.Context, cred *Credential, req *ExecuteRequest, sink StreamSink) error
}

// ExecuteRequest is the input for a chat execution.
type ExecuteRequest struct {
	// Model is the bare model name with the provider prefix stripped.
	Model string
	// Payload is the chat-completions request body translated by the host.
	Payload []byte
	// Headers are the client request headers.
	Headers map[string][]string
	// ClientIP is the client IP (empty when passthrough is disabled).
	ClientIP string
	// Stream reports whether it is streaming.
	Stream bool
}

// StreamSink is the output for the plugin to deliver streaming chunks to the host (implemented by the root layer).
type StreamSink interface {
	// Emit delivers a chunk (naked SSE data payload, without the `data: ` prefix).
	Emit(payload []byte) error
	// Close ends the stream. When errMsg is non-empty, it is delivered to the client as an error frame.
	Close(errMsg string) error
}

// This file defines the provider runtime environment and composition root factory.
//
// Env is the only host surface a provider is allowed to depend on: host port, registry, login session table.
// Providers no longer read package-level global registries or global host ports.

// VendorFactory is responsible for constructing an instance of a provider family (regional variants are expanded within the family).
type VendorFactory struct {
	Instances func(Env) []Vendor
}

// Env represents the runtime dependencies of a provider instance.
type Env struct {
	Host     Host
	Registry *Registry
	Logins   LoginSessions
}

// Settings returns the current global settings snapshot.
func (e Env) Settings() Settings {
	if e.Host == nil {
		return Settings{}
	}
	return e.Host.Settings()
}

// PollState is the admission result of login polling.
type PollState int

const (
	PollMissing PollState = iota
	PollThrottled
	PollReady
)

// LoginSessions is the login session registry interface; the implementation is provided by internal/auth.SessionRegistry.
type LoginSession struct {
	VendorID string
	Data     any
}

type LoginSessions interface {
	Register(vendorID, sessionID string, ttl, interval time.Duration, data any)
	Poll(sessionID string) (*LoginSession, PollState)
	Owns(sessionID string) (vendorID string, ok bool)
	Delete(sessionID string)
}

// ProbeNativeCredential picks the first available native credential belonging to vendorID from the host credentials.
func ProbeNativeCredential[T any](env Env, vendorID string, extract NativeExtractor[T]) (*T, bool) {
	if env.Host == nil || env.Registry == nil {
		return nil, false
	}
	ctx := context.Background()
	entries, errList := env.Host.ListAuths(ctx)
	if errList != nil {
		return nil, false
	}
	for _, entry := range entries {
		if !entry.Usable() {
			continue
		}
		raw, okRaw := env.Host.AuthJSON(ctx, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, okParse := env.Registry.ParseCredential(raw, entry.Name, nil)
		if !okParse || credential.VendorIDValue() != vendorID {
			continue
		}
		if native, okNative := extract(credential); okNative && native != nil {
			return native, true
		}
	}
	return nil, false
}
