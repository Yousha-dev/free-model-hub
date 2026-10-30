package tasks

import (
	"context"
	"time"

	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
)

// fakeDeps is the minimum Deps required for keep-alive testing.
type fakeDeps struct {
	now   time.Time
	state state.State
	cfg   Config
	// refreshSaves counts the number of credential write-backs: concurrent renewal should only write back once.
	refreshSaves int
}

func (f *fakeDeps) Registry() *core.Registry { return nil }
func (f *fakeDeps) Pool() *auth.Pool         { return nil }
func (f *fakeDeps) Config() Config           { return f.cfg }
func (f *fakeDeps) State() state.State       { return f.state }
func (f *fakeDeps) Now() time.Time           { return f.now }

func (f *fakeDeps) MutateState(mutate func(*state.State)) { mutate(&f.state) }

func (f *fakeDeps) SaveRefreshedCredential(
	ctx context.Context, callbackID string, vendor core.Vendor,
	entry core.HostAuth, original []byte, updated *core.Credential, filePath string,
) {
	f.refreshSaves++
}

// newFakeDeps constructs a minimum Deps with an initialized map.
func newFakeDeps(now time.Time) *fakeDeps {
	return &fakeDeps{now: now, state: *state.New()}
}
