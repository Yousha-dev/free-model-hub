package main

// This file only retains the essential entries, version injection, and ABI bridging required for c-shared.
// Domain logic is located in internal/app; C ABI is in cgo.go.

import "free-model-hub/internal/app"

// pluginVersion is injected by CI via -ldflags -X main.pluginVersion=<tag>.
var pluginVersion = app.DefaultVersion

var pluginApp = app.MustNew(app.Options{
	Version:   pluginVersion,
	Factories: app.BuiltinFactories(),
})

// handleMethod is reserved for cgo.go and legacy test calls.
func handleMethod(method string, request []byte) ([]byte, error) {
	return pluginApp.HandleMethod(method, request)
}
