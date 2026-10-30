package tasks

// This file defines the minimal interfaces required by the task engine; implemented by the internal/app composition root.

import (
	"context"
	"time"

	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
)

// Deps represents the root-level capabilities required by the task engine.
type Deps interface {
	Registry() *core.Registry
	Pool() *auth.Pool
	Config() Config
	State() state.State
	MutateState(func(*state.State))
	Now() time.Time
	// SaveRefreshedCredential writes the refreshed credential back to the host and disk.
	//
	// Implemented by the composition root (internally via core.StorageMerger → host.auth.save → disk persistence).
	// The task engine depends on this rather than making host RPCs directly, so this package does not need to know about the host.
	SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor,
		entry core.HostAuth, original []byte, updated *core.Credential, filePath string)
}
