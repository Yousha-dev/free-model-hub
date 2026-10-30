package opencodego

// OpenCode Go has no login flow: accounts are subscription API keys added in
// the console.

// AuthMode implements core.AuthModeReporter so keys are masked in the console.
func (a *adapter) AuthMode() string { return "apikey" }

// KeyURL implements core.KeyInfoProvider.
func (a *adapter) KeyURL() string { return "https://opencode.ai/auth" }

// KeyNote implements core.KeyInfoProvider.
func (a *adapter) KeyNote() string {
	return "OpenCode Go is a paid subscription; use the API key from your OpenCode account."
}
