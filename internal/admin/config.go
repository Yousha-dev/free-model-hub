// This file parses and validates the global configuration of the plugin (config.yaml fragment issued by the host).
//
// Placed in the admin package: the configuration has only two consumers—the composition root reads it to start, and the management API
// reads/writes runtime settings, both of which already depend on admin.
package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PluginConfig is the valid global configuration parsed by plugins.configs.free-model-hub.
type PluginConfig struct {
	// EnabledRealms is the enabled realms: comma-separated subset of cn / global.
	EnabledRealms []string
	// ExtraModels are additionally registered model names (without realm prefix).
	ExtraModels []string
	// DisabledModels are the disabled models declared in configuration (raw notation, expansion done by model layer).
	DisabledModels []string
	// StateDir is the plugin state directory (machine salt, model cache, task records, logs).
	StateDir string
	// LogLevel controls plugin logs: debug / info / error.
	LogLevel string
	// LogToFile when true writes logs to <StateDir>/logs.
	LogToFile bool

	// PromptMode is the prompt filtering mode (global): passthrough / custom / append.
	PromptMode string

	// ModelNameTag is the provider identification mode for model names (global): off / prefix / suffix.
	// When enabled, each provider's model external ID automatically carries a provider prefix or suffix (e.g.,
	// zcode-GLM-5.3 / GLM-5.3-zcode), allowing same-named models from different channels to be distinguished.
	ModelNameTag string

	// Raw is the raw key-value table of configuration (key → original text), for providers to parse proprietary keys.
	Raw map[string]string
}

const (
	// DefaultLogLevel is the default log level.
	DefaultLogLevel = "info"
	// DefaultStateDirName is the default state directory name.
	DefaultStateDirName = ".free-model-hub"
	// StateDirEnvOverride is the environment variable override for the state directory.
	StateDirEnvOverride = "FREE_MODEL_HUB_HOME"
	// StateFileName is the state file name.
	StateFileName = "state.json"
	// SaltFileName is the machine salt file name.
	SaltFileName = "machine_salt"
	// DefaultRealmList is the default enabled realms.
	DefaultRealmList = "cn,global"

	// RealmCN / RealmGlobal are the supported realms.
	RealmCN     = "cn"
	RealmGlobal = "global"

	// PromptModeKey / PromptFileKey are the global configuration keys for prompt filtering.
	//
	// Prompt filtering is a plugin-level capability: mode and prompt file are globally single, whoever implements filtering reads these
	// keys (currently WorkBuddy), and the UI does not configure them per provider.
	PromptModeKey = "prompt_mode"
	PromptFileKey = "prompt_file"

	// ModelNameTagKey is the global configuration key for model name channel identification (off/prefix/suffix).
	ModelNameTagKey = "model_name_tag"

	// PromptModePassThrough / PromptModeCustom / PromptModeAppend are valid modes.
	PromptModePassThrough = "passthrough"
	PromptModeCustom      = "custom"
	PromptModeAppend      = "append"

	// ModelNameTagOff / Prefix / Suffix are the three modes for model name channel identification.
	ModelNameTagOff    = "off"
	ModelNameTagPrefix = "prefix"
	ModelNameTagSuffix = "suffix"
)

// deprecatedConfigKeys are old configuration keys after renaming: normalized directly to current keys during parsing
// to prevent old configurations from being silently ignored (having prompt filtering quietly turned off is worse than an error).
var deprecatedConfigKeys = map[string]string{
	"workbuddy_prompt_mode": PromptModeKey,
	"workbuddy_prompt_file": PromptFileKey,
}

// listValuedConfigKeys are configuration keys that "allow YAML list syntax".
var listValuedConfigKeys = map[string]bool{
	"enabled_realms":  true,
	"extra_models":    true,
	"disabled_models": true,
}

// Default returns the built-in default configuration (excluding provider defaults).
func DefaultPluginConfig() PluginConfig {
	return PluginConfig{
		EnabledRealms: SplitList(DefaultRealmList),
		StateDir:      DefaultStateDir(),
		LogLevel:      DefaultLogLevel,
		PromptMode:    PromptModePassThrough,
		ModelNameTag:  ModelNameTagOff,
	}
}

// DefaultStateDir infers the default state directory: environment variable > host credential directory subpath > user home directory > temp directory.
//
// The three-tier fallback allows the plugin to start even in a read-only HOME (containers, CI).
func DefaultStateDir() string {
	if env := strings.TrimSpace(os.Getenv(StateDirEnvOverride)); env != "" {
		return env
	}
	if home, errHome := os.UserHomeDir(); errHome == nil && strings.TrimSpace(home) != "" {
		cpaDir := filepath.Join(home, ".cli-proxy-api")
		if info, errStat := os.Stat(cpaDir); errStat == nil && info.IsDir() {
			return filepath.Join(cpaDir, "free-model-hub")
		}
		return filepath.Join(home, DefaultStateDirName)
	}
	return filepath.Join(os.TempDir(), DefaultStateDirName)
}

// Decode parses the configuration YAML passed by the host, with base as the initial default (provider defaults are supplemented by the caller).
//
// Supports `key: value` scalar lines, # comments, single/double quoted strings, and block/flow styles for list fields.
func DecodePluginConfig(raw []byte, base PluginConfig) (PluginConfig, error) {
	cfg := base
	if cfg.Raw == nil {
		cfg.Raw = map[string]string{}
	}
	if len(raw) == 0 {
		return cfg, nil
	}
	lines := normalizeListConfigLines(strings.Split(string(raw), "\n"))
	for lineNo, line := range lines {
		key, value, ok := splitConfigLine(line)
		if !ok {
			continue
		}
		if current, okAlias := deprecatedConfigKeys[key]; okAlias {
			key = current
		}
		if errLine := applyConfigLine(&cfg, key, value); errLine != nil {
			return cfg, fmt.Errorf("config line %d: %w", lineNo+1, errLine)
		}
	}
	if errValidate := ValidatePluginConfig(cfg); errValidate != nil {
		return cfg, errValidate
	}
	return cfg, nil
}

func applyConfigLine(cfg *PluginConfig, key, value string) error {
	if cfg.Raw == nil {
		cfg.Raw = map[string]string{}
	}
	cfg.Raw[key] = value
	switch key {
	case "enabled_realms":
		realms, errRealms := ParseRealms(value)
		if errRealms != nil {
			return errRealms
		}
		cfg.EnabledRealms = realms
	case "extra_models":
		cfg.ExtraModels = SplitList(stripInlineList(value))
	case "disabled_models":
		cfg.DisabledModels = SplitList(stripInlineList(value))
	case "state_dir":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.StateDir = trimmed
		}
	case "log_level":
		if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
			cfg.LogLevel = trimmed
		}
	case "log_to_file":
		cfg.LogToFile = ParseBool(value, cfg.LogToFile)
	case PromptModeKey:
		mode, errMode := ParsePromptMode(value)
		if errMode != nil {
			return errMode
		}
		if mode != "" {
			cfg.PromptMode = mode
		}
	case ModelNameTagKey:
		tag, errTag := NormalizeModelNameTag(value)
		if errTag != nil {
			return errTag
		}
		cfg.ModelNameTag = tag
	}
	return nil
}

// Validate validates the configuration. Error messages point directly to the problematic key for easy troubleshooting by operations.
func ValidatePluginConfig(cfg PluginConfig) error {
	if len(cfg.EnabledRealms) == 0 {
		return fmt.Errorf("enabled_realms is empty (want a subset of cn,global)")
	}
	switch cfg.PromptMode {
	case PromptModePassThrough, PromptModeCustom, PromptModeAppend:
	default:
		return fmt.Errorf("prompt_mode %q is not supported (want passthrough, custom or append)", cfg.PromptMode)
	}
	switch cfg.ModelNameTag {
	case ModelNameTagOff, ModelNameTagPrefix, ModelNameTagSuffix:
	default:
		return fmt.Errorf("model_name_tag %q is not supported (want off, prefix or suffix)", cfg.ModelNameTag)
	}
	switch cfg.LogLevel {
	case "debug", "info", "error":
	default:
		return fmt.Errorf("log_level %q is not supported (want debug, info or error)", cfg.LogLevel)
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return fmt.Errorf("state_dir is empty")
	}
	return nil
}

// ParseRealms parses the realm list and deduplicates it.
func ParseRealms(raw string) ([]string, error) {
	items := SplitList(stripInlineList(raw))
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		realm := strings.ToLower(strings.TrimSpace(item))
		switch realm {
		case RealmCN, RealmGlobal:
		default:
			return nil, fmt.Errorf("enabled_realms contains unsupported realm %q (want cn or global)", item)
		}
		if seen[realm] {
			continue
		}
		seen[realm] = true
		out = append(out, realm)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("enabled_realms is empty (want a subset of cn,global)")
	}
	return out, nil
}

// ParsePromptMode parses the prompt filtering mode; an empty string indicates it was not provided (the caller retains the current value).
func ParsePromptMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "", PromptModePassThrough, PromptModeCustom, PromptModeAppend:
		return mode, nil
	}
	return "", fmt.Errorf("prompt_mode %q is not supported (want passthrough, custom or append)", raw)
}

// NormalizeModelNameTag normalizes the channel identifier mode; an empty string falls back to off.
func NormalizeModelNameTag(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return ModelNameTagOff, nil
	}
	switch trimmed {
	case ModelNameTagOff, ModelNameTagPrefix, ModelNameTagSuffix:
		return trimmed, nil
	}
	return "", fmt.Errorf("model_name_tag %q is not supported (want off, prefix or suffix)", value)
}

// EffectiveModelNameTag returns the currently active channel identifier mode: page override > YAML > built-in default.
func EffectiveModelNameTag(cfg PluginConfig, page string) string {
	if tag := strings.ToLower(strings.TrimSpace(page)); tag != "" {
		return tag
	}
	if cfg.ModelNameTag == "" {
		return ModelNameTagOff
	}
	return cfg.ModelNameTag
}

// EffectivePromptMode returns the currently active prompt filter mode: page override > YAML > built-in default.
func EffectivePromptMode(cfg PluginConfig, page string) string {
	if mode := strings.ToLower(strings.TrimSpace(page)); mode != "" {
		return mode
	}
	return cfg.PromptMode
}

// ParseBool parses a boolean value, keeping the fallback when parsing fails.
func ParseBool(raw string, fallback bool) bool {
	value, ok := parseStrictBool(raw)
	if !ok {
		return fallback
	}
	return value
}

// parseStrictBool parses a boolean value, returning ok=false when parsing fails (for config validation).
func parseStrictBool(raw string) (value bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	}
	return false, false
}

// SplitList splits list items by comma or whitespace.
func SplitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// splitConfigLine splits `key: value`; skips empty lines, comments, list items, and non-scalar lines.
func splitConfigLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimRight(line, "\r")
	trimmed = strings.TrimLeft(trimmed, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return "", "", false
	}
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:idx])
	value = strings.TrimSpace(trimmed[idx+1:])
	// Remove trailing comments: only process when the value is not a quoted string to avoid consuming ` #` inside quotes.
	if !strings.HasPrefix(value, "\"") && !strings.HasPrefix(value, "'") {
		if hash := strings.Index(value, " #"); hash >= 0 {
			value = strings.TrimSpace(value[:hash])
		}
	}
	value = strings.Trim(value, `"'`)
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

// normalizeListConfigLines normalizes block-style writing of list fields into a single-line scalar.
func normalizeListConfigLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	index := -1
	key := ""
	var items []string
	flush := func() {
		if index >= 0 && len(items) > 0 {
			out[index] = key + ": " + strings.Join(items, ",")
		}
		index, key, items = -1, "", nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if index >= 0 && listValuedConfigKeys[key] && item != "" {
				items = append(items, strings.Trim(item, `"'`))
				continue
			}
		}
		lineKey, _, ok := splitConfigLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		flush()
		index, key = len(out), lineKey
		out = append(out, line)
	}
	flush()
	return out
}

// stripInlineList removes the square brackets of flow-style list syntax `[a, b]`.
func stripInlineList(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	return trimmed
}
