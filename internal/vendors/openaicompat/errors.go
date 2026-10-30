package openaicompat

import (
	"free-model-hub/internal/core"
)

func classify(status int, body []byte) *core.Failure {
	return &core.Failure{Status: status, Msg: core.Truncate(string(body), 300), Kind: core.ClassifyStatus(status)}
}
