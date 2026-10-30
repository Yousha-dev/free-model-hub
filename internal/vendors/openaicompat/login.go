package openaicompat

// These providers have no login flow: accounts are API keys added in the
// console, or the keyless account written automatically.

// AuthMode implements core.AuthModeReporter so keys are masked in the console.
func (a *adapter) AuthMode() string { return "apikey" }

// KeyURL implements core.KeyInfoProvider.
func (a *adapter) KeyURL() string { return a.provider.KeyURL }

// KeyNote implements core.KeyInfoProvider.
func (a *adapter) KeyNote() string { return a.provider.Note }
