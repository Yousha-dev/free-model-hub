package admin

import (
	"strings"

	"free-model-hub/internal/core"
)

// vendorDescriptor is the display description of the provider (for page dropdowns and grouping).
type vendorDescriptor struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Region        string `json:"region"`
	AuthMode      string `json:"auth_mode"`
	SupportsLogin bool   `json:"supports_login"`
	// SMSLogin reports whether the provider supports a two-step login form of "phone number + SMS verification code".
	SMSLogin bool `json:"sms_login"`
	// PasteCallback reports whether the provider's login requires (or supports) pasting a localhost callback link as a fallback.
	PasteCallback bool `json:"paste_callback"`
	// CallbackHint is the local callback address to be displayed on the page (has a value only when PasteCallback is true).
	CallbackHint string `json:"callback_hint,omitempty"`
	// LoginCaptcha reports whether an in-page captcha widget is needed before login.
	LoginCaptcha bool `json:"login_captcha"`
	// LoginVariants are optional login variants for the same provider (e.g., Zai / Google for AutoClaw).
	LoginVariants []core.LoginVariant `json:"login_variants,omitempty"`
	// KeyURL is where users create an API key for this provider; KeyNote explains its free tier.
	KeyURL  string `json:"key_url,omitempty"`
	KeyNote string `json:"key_note,omitempty"`
}

func (s *Service) buildVendorsPayload() map[string]any {
	cfg := s.deps.Config()
	vendors := make([]vendorDescriptor, 0, len(s.registry.Vendors()))
	for _, vendor := range s.registry.Vendors() {
		if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) {
			continue
		}
		descriptor := vendorDescriptor{
			ID:            vendor.ID(),
			Name:          vendor.Name(),
			Region:        vendor.Region(),
			AuthMode:      vendorAuthMode(vendor),
			SupportsLogin: vendorSupportsLogin(vendor),
		}
		if _, okSMS := core.As[core.SMSLoginProvider](vendor); okSMS {
			descriptor.SMSLogin = true
		}
		if submitter, okSubmit := core.As[core.CallbackSubmitter](vendor); okSubmit {
			descriptor.PasteCallback = true
			descriptor.CallbackHint = strings.TrimSpace(submitter.LocalCallbackHint())
		}
		if _, okCaptcha := core.As[core.LoginCaptchaProvider](vendor); okCaptcha {
			descriptor.LoginCaptcha = true
		}
		if variants, okVariants := core.As[core.LoginVariantProvider](vendor); okVariants {
			descriptor.LoginVariants = variants.LoginVariants()
		}
		if info, okInfo := core.As[core.KeyInfoProvider](vendor); okInfo {
			descriptor.KeyURL = info.KeyURL()
			descriptor.KeyNote = info.KeyNote()
		}
		vendors = append(vendors, descriptor)
	}
	return map[string]any{"ok": true, "vendors": vendors}
}

func vendorAuthMode(vendor core.Vendor) string {
	if reporter, okReporter := vendor.(core.AuthModeReporter); okReporter {
		if mode := strings.TrimSpace(reporter.AuthMode()); mode != "" {
			return mode
		}
	}
	return "oauth"
}

func vendorSupportsLogin(vendor core.Vendor) bool {
	if _, okLogin := core.As[core.LoginProvider](vendor); okLogin {
		return true
	}
	// SMS verification code login is also a login (the console renders a two-step form instead of an authorization link).
	if _, okSMS := core.As[core.SMSLoginProvider](vendor); okSMS {
		return true
	}
	return false
}
