// Package config holds the OpenCode Go settings the catalog and translators
// read. It replaces the upstream YAML loader: this plugin configures OpenCode
// Go through its own config fields, so no YAML dependency is needed.
package config

import (
	"strings"
	"time"
)

// Defaults.
const (
	DefaultBaseURL          = "https://opencode.ai/zen/go/v1"
	DefaultModelPrefix      = "opencode-go"
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
)

type ModelPrefix struct {
	Enabled bool
	Value   string
}

type Catalog struct {
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
}

type RouteOverride struct {
	Protocol string
	Endpoint string
}

type Config struct {
	BaseURL          string
	CatalogURL       string
	ModelPrefix      ModelPrefix
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// Default returns the standard configuration for baseURL ("" = default).
func Default(baseURL string) Config {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return Config{
		BaseURL:          base,
		CatalogURL:       base + "/models",
		ModelPrefix:      ModelPrefix{Enabled: true, Value: DefaultModelPrefix},
		Catalog:          Catalog{StaleWhileUnavailable: true},
		Protocols:        Protocols{ChatCompletions: true, Messages: true, Responses: true},
		RequestTimeout:   DefaultRequestTimeout,
		MaxResponseBytes: DefaultMaxResponseBytes,
	}
}

// PublicID returns the client-facing model ID.
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}
