package opencodego

import (
	"free-model-hub/internal/core"
	"free-model-hub/internal/ocgo/errclass"
)

// toFailure maps a classified OpenCode Go error onto the plugin's failure
// kinds so the host cools down or rotates credentials correctly.
func toFailure(e *errclass.Error) error {
	if e == nil {
		return nil
	}
	kind := core.ClassifyStatus(e.StatusCode)
	switch e.Class {
	case errclass.ClassAuth:
		kind = core.FailureKindCredential
	case errclass.ClassRateLimit:
		kind = core.FailureKindRateLimit
	case errclass.ClassQuota, errclass.ClassBilling:
		kind = core.FailureKindQuota
	case errclass.ClassNetwork, errclass.ClassUpstream:
		kind = core.FailureKindTransient
	case errclass.ClassInvalidModel:
		kind = core.FailureKindNotFound
	case errclass.ClassUnsupported, errclass.ClassTranslation:
		kind = core.FailureKindBadParams
	}
	status := e.StatusCode
	if status == 0 {
		status = 502
	}
	return core.ToPluginError(&core.Failure{Status: status, Msg: core.Truncate(e.Message, 300), Kind: kind})
}
