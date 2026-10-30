// Package state is responsible for the plugin's own persisted state: typed read and write of state.json.
//
// No credentials are saved in the state: credentials exist only in the host's auth file, and all operations
// requiring a token must fetch them in real-time through host.auth.get.
//
// This package only handles four things: "read, modify, write, and snapshot"; side-effect-heavy initializations
// such as machine salt and log directories are left to the root layer (ABI adaptation layer).
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

// Version is the state file structure version, used for future migrations.
const Version = 1

// ClaimRecord is the latest plan claim status for an account.
//
// Embed pluginapi.ClaimStatus instead of restating its fields: quota responses and page displays share
// the same shape, so when the provider changes fields, there is no need to change them here.
type ClaimRecord struct {
	pluginapi.ClaimStatus
	// CheckedAt is the probe time (RFC3339), which the page uses to display "checked how long ago".
	CheckedAt string `json:"checked_at,omitempty"`
}

// State is the complete structure of state.json.
//
// Page settings use pointer fields to distinguish between "not set" and "set to false/empty":
// When not set, it falls back to the YAML configuration; when set, the page value takes precedence.
type State struct {
	Version int `json:"version"`

	// PromptMode is the prompt filter mode modified on the page; empty means falling back to the YAML configuration.
	PromptMode string `json:"prompt_mode,omitempty"`
	// ModelNameTag is the model name channel tag mode modified on the page (off/prefix/suffix);
	// empty means falling back to the YAML configuration.
	ModelNameTag string `json:"model_name_tag,omitempty"`

	// TaskSchedules is the list of top-of-the-hour hours (0-23) saved by task family; only keepalive remains.
	TaskSchedules map[string][]int `json:"task_schedules,omitempty"`

	// Models is the upstream model list cache (bucketed by realm in the Realm field).
	Models []core.CachedModel `json:"models,omitempty"`
	// ModelsFetchedAt is the list cache timestamp (RFC3339).
	ModelsFetchedAt string `json:"models_fetched_at,omitempty"`

	// DisabledModels are the disabled model IDs (full IDs with cn:/global: prefix).
	DisabledModels []string `json:"disabled_models,omitempty"`

	// ModelAliases is the model alias table: <vendorID>:<official model ID> -> external alias.
	ModelAliases map[string]string `json:"model_aliases,omitempty"`

	// AccountAliases is the account alias table: account identity (FileID) -> display alias.
	//
	// The key must be a stable account identity, not a nickname/email: the former is not unique across accounts
	// and changes whenever the upstream modifies it. Using it as a key would cause aliases to be lost after re-login
	// or mapped to another account. FileID is determined only by the credential file name; token refresh and re-login do not affect it (see core.Credential.FileID).
	AccountAliases map[string]string `json:"account_aliases,omitempty"`

	// RestartPending indicates "model changes have been made, but the host's model registry has not been refreshed yet".
	RestartPending bool `json:"restart_pending,omitempty"`

	// Claim records the plan claim status by account FileID (refreshed alongside quota queries).
	Claim map[string]ClaimRecord `json:"claim,omitempty"`
}

// Store is the in-process singleton for state.json: it serializes reads, modifications, and writes while caching snapshots.
type Store struct {
	mu    sync.Mutex
	dir   string
	cache *State
}

// NewStore creates a Store not yet bound to a directory.
func NewStore() *Store { return &Store{} }

// Configure binds the state directory (does not read or write files).
func (s *Store) Configure(dir string) {
	s.mu.Lock()
	s.dir = dir
	s.mu.Unlock()
}

// New returns an empty state with populated maps.
func New() *State {
	return &State{
		Version:        Version,
		Claim:          map[string]ClaimRecord{},
		ModelAliases:   map[string]string{},
		AccountAliases: map[string]string{},
		TaskSchedules:  map[string][]int{},
	}
}

// Load reads the state from disk. Returns an empty state (non-fatal) if the file is missing or corrupted.
//
// The return value is the cache itself; callers should only read from it, and modifications must go through Mutate/Snapshot.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.pathLocked()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			s.cache = New()
			return s.cache, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, errRead)
	}
	var loaded State
	if errUnmarshal := json.Unmarshal(raw, &loaded); errUnmarshal != nil {
		// State corruption does not prevent the plugin from starting: better to lose cache and settings than to render the plugin unusable for users.
		logger.Error("state file %s is corrupt, starting with empty state: %v", path, errUnmarshal)
		s.cache = New()
		return s.cache, nil
	}
	normalize(&loaded)
	s.cache = &loaded
	return s.cache, nil
}

func (s *Store) pathLocked() string {
	return filepath.Join(s.dir, "state.json")
}

func normalize(state *State) {
	state.Version = Version
	if state.TaskSchedules == nil {
		state.TaskSchedules = map[string][]int{}
	}
	if state.Claim == nil {
		state.Claim = map[string]ClaimRecord{}
	}
	if state.ModelAliases == nil {
		state.ModelAliases = map[string]string{}
	}
	if state.AccountAliases == nil {
		state.AccountAliases = map[string]string{}
	}
}

// Snapshot returns a deep copy of the state, which callers can safely read.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return *New()
	}
	return clone(s.cache)
}

// clone manually deep copies maps, slices, and pointer fields.
func clone(state *State) State {
	out := *state
	out.Models = append([]core.CachedModel(nil), state.Models...)
	out.TaskSchedules = make(map[string][]int, len(state.TaskSchedules))
	for kind, hours := range state.TaskSchedules {
		out.TaskSchedules[kind] = append([]int(nil), hours...)
	}
	out.DisabledModels = append([]string(nil), state.DisabledModels...)
	out.Claim = make(map[string]ClaimRecord, len(state.Claim))
	for key, value := range state.Claim {
		out.Claim[key] = value
	}
	out.ModelAliases = make(map[string]string, len(state.ModelAliases))
	for key, value := range state.ModelAliases {
		out.ModelAliases[key] = value
	}
	out.AccountAliases = make(map[string]string, len(state.AccountAliases))
	for key, value := range state.AccountAliases {
		out.AccountAliases[key] = value
	}
	return out
}

// Mutate modifies the state under a lock and persists it immediately.
//
// The modification function should not perform I/O (it runs under a lock); persistence failures only log
// an error without failing the caller—failing to write the state should not cause a conversation to fail.
func (s *Store) Mutate(mutate func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = New()
	}
	mutate(s.cache)
	if errSave := s.saveLocked(); errSave != nil {
		logger.Error("save state failed: %v", errSave)
	}
}

// saveLocked atomically writes the state file. The caller must hold s.mu.
func (s *Store) saveLocked() error {
	if s.cache == nil {
		return nil
	}
	s.cache.Version = Version
	raw, errMarshal := json.MarshalIndent(s.cache, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal state: %w", errMarshal)
	}
	path := s.pathLocked()
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write state: %w", errWrite)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state file: %w", errRename)
	}
	return nil
}

// Reset clears the memory cache (for testing).
func (s *Store) Reset() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}
