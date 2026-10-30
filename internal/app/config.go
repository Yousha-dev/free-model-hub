package app

// This file is the App-level entry point responsible for global configuration and persistent state.
//
// Configuration parsing/validation is in internal/admin, and state.json reading/writing/snapshots are in
// internal/state; App holds instances and no longer uses package-level globals.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"free-model-hub/internal/admin"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
	"free-model-hub/internal/state"
	"free-model-hub/internal/tasks"
)

type pluginConfig = admin.PluginConfig

type pluginState = state.State

func defaultPluginConfig() pluginConfig { return admin.DefaultPluginConfig() }

func defaultStateDir() string { return admin.DefaultStateDir() }

// Config returns a copy of the currently active configuration.
func (a *App) Config() pluginConfig {
	if cfg := a.cfg.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func (a *App) storeConfig(cfg pluginConfig) { a.cfg.Store(&cfg) }

func (a *App) decodeConfig(raw []byte) (pluginConfig, error) {
	cfg, errDecode := admin.DecodePluginConfig(raw, defaultPluginConfig())
	if errDecode != nil {
		return cfg, errDecode
	}
	cfg.DisabledModels = a.models.ExpandDisabledList(cfg.DisabledModels)
	return cfg, nil
}

// ApplyConfig applies the configuration: initializes log and state directories, sets the fingerprint salt, and distributes provider configurations.
func (a *App) ApplyConfig(cfg pluginConfig) error {
	if errDir := ensureStateDir(cfg); errDir != nil {
		return errDir
	}
	logger.SetLevel(cfg.LogLevel)
	if errLog := logger.InitFile(cfg.LogToFile, filepath.Join(cfg.StateDir, "logs")); errLog != nil {
		logger.Error("init log file sink failed: %v", errLog)
	}
	a.store.Configure(cfg.StateDir)
	salt, errSalt := a.ensureMachineSalt(cfg)
	if errSalt != nil {
		return errSalt
	}
	a.machineSalt = salt
	a.storeConfig(cfg)
	a.models.Configure(admin.ModelSettings{
		EnabledRealms:    cfg.EnabledRealms,
		ExtraModels:      cfg.ExtraModels,
		DisabledModels:   cfg.DisabledModels,
		ContextWindowFor: core.DefaultContextWindow,
		ModelNameTag:     admin.EffectiveModelNameTag(cfg, a.SnapshotState().ModelNameTag),
	})
	a.models.RefreshFromState()
	a.configureVendors(cfg)
	return nil
}

func ensureStateDir(cfg pluginConfig) error {
	dir := strings.TrimSpace(cfg.StateDir)
	if dir == "" {
		return fmt.Errorf("state_dir is empty")
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state dir %s: %w", dir, errMkdir)
	}
	return nil
}

func saltFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, admin.SaltFileName)
}

func (a *App) loadState(cfg pluginConfig) (*pluginState, error) {
	a.store.Configure(cfg.StateDir)
	loaded, errLoad := a.store.Load()
	if errLoad != nil {
		return nil, errLoad
	}
	a.models.ReloadFromState(*loaded)
	return loaded, nil
}

// SnapshotState returns a copy of the state.
func (a *App) SnapshotState() pluginState { return a.store.Snapshot() }

// MutateState modifies the state within a lock and persists it immediately.
func (a *App) MutateState(mutate func(*pluginState)) { a.store.Mutate(mutate) }

func (a *App) ensureMachineSalt(cfg pluginConfig) (string, error) {
	if override := strings.TrimSpace(cfg.Raw["machine_salt"]); override != "" {
		return override, nil
	}
	path := saltFilePath(cfg)
	if raw, errRead := os.ReadFile(path); errRead == nil {
		if salt := strings.TrimSpace(string(raw)); salt != "" {
			return salt, nil
		}
	}
	buf := make([]byte, 24)
	if _, errRand := rand.Read(buf); errRand != nil {
		return "", fmt.Errorf("generate machine salt: %w", errRand)
	}
	salt := hex.EncodeToString(buf)
	if errWrite := os.WriteFile(path, []byte(salt+"\n"), 0o600); errWrite != nil {
		return "", fmt.Errorf("write machine salt: %w", errWrite)
	}
	logger.Info("machine salt generated at %s (do not change: it defines this deployment's device fingerprints)", path)
	return salt, nil
}

// taskConfig projects the global configuration into the minimal shape required by the task engine.
//
// Projecting instead of directly passing admin.PluginConfig prevents internal/tasks from depending on
// the admin interface package (repository layering guard prohibits tasks -> admin).
func taskConfig(cfg admin.PluginConfig) tasks.Config {
	return tasks.Config{
		EnabledRealms: cfg.EnabledRealms,
	}
}
