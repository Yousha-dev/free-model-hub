package admin

import (
	"testing"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
)

// newTagTestCatalog constructs a catalog with the zcode provider and registered models for channel tag testing.
func newTagTestCatalog(t *testing.T, tagMode string) *Catalog {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	if err := registry.Register(&fakeVendor{Meta: core.Meta{VendorID: "zcode", VendorName: "ZCode", VendorRegion: "global"}}); err != nil {
		t.Fatal(err)
	}
	c := NewCatalog(store, registry)
	c.Configure(ModelSettings{ModelNameTag: tagMode})
	models := []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}}
	if !c.CacheProbedModels("zcode", models) {
		t.Fatal("register probe models")
	}
	return c
}

// TestChannelTagPrefixRenamesAndResolves verifies the prefix mode: external IDs carry the provider
// prefix, the request side strips the prefix to restore the official ID, and bare names without the prefix can also be resolved.
func TestChannelTagPrefixRenamesAndResolves(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagPrefix)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}})
	if len(out) != 1 || out[0].ID != "zcode-GLM-5.3" {
		t.Fatalf("tagged id = %+v", out)
	}
	if got := c.ResolveOutboundModel("zcode", "zcode-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("untag resolve = %q", got)
	}
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("bare passthrough = %q", got)
	}
	// Requests to other providers are unaffected (the prefix only strips its own vendor name).
	if got := c.ResolveOutboundModel("workbuddycn", "zcode-GLM-5.3"); got != "zcode-GLM-5.3" {
		t.Fatalf("other vendor passthrough = %q", got)
	}
}

// TestChannelTagSuffixRenames verifies the suffix mode.
func TestChannelTagSuffixRenames(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagSuffix)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}})
	if len(out) != 1 || out[0].ID != "GLM-5.3-zcode" {
		t.Fatalf("tagged id = %+v", out)
	}
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3-zcode"); got != "GLM-5.3" {
		t.Fatalf("untag resolve = %q", got)
	}
}

// TestChannelTagOffKeepsBareID verifies the off mode (default) does not rename.
func TestChannelTagOffKeepsBareID(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagOff)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}})
	if len(out) != 1 || out[0].ID != "GLM-5.3" {
		t.Fatalf("off mode must keep bare id, got %+v", out)
	}
}

// TestChannelTagDoesNotShadowOtherModel verifies safety check: when a tagged name collides with **other models**
// in the current manifest, tagging is abandoned to avoid routing ambiguity. Collision check uses the input manifest rather
// than registeredLocked—the latter would misjudge empty-cache providers as "already existing", causing tags to be abandoned entirely.
func TestChannelTagDoesNotShadowOtherModel(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagPrefix)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{
		{ID: "GLM-5.3", Name: "GLM-5.3"},
		{ID: "zcode-GLM-5.3", Name: "zcode-GLM-5.3"},
	})
	if len(out) != 2 || out[0].ID != "GLM-5.3" {
		t.Fatalf("collision must skip tagging, got %+v", out)
	}
}

// TestChannelTagResolveAfterModeSwitch verifies that old tagged requests can still be restored after mode switches:
// When prefix was enabled and switched back to off, zcode-GLM-5.3 held by the client cannot directly pass through to the upstream.
func TestChannelTagResolveAfterModeSwitch(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagPrefix)
	if got := c.ResolveOutboundModel("zcode", "zcode-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("prefix resolve = %q", got)
	}
	// Switch to off: restoration does not depend on the current mode (tries both forms).
	c.Configure(ModelSettings{ModelNameTag: ModelNameTagOff})
	if got := c.ResolveOutboundModel("zcode", "zcode-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("off-mode legacy prefix resolve = %q", got)
	}
	// Suffix form can also be restored (even though suffix was never enabled).
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3-zcode"); got != "GLM-5.3" {
		t.Fatalf("legacy suffix resolve = %q", got)
	}
}
