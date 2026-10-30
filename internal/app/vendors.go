package app

// This file is the only provider composition root: adding/removing providers only requires changing the factory list here.

import (
	"free-model-hub/internal/core"
	"free-model-hub/internal/vendors/autoclaw"
	"free-model-hub/internal/vendors/cline"
	"free-model-hub/internal/vendors/codearts"
	"free-model-hub/internal/vendors/minimaxcode"
	"free-model-hub/internal/vendors/openaicompat"
	"free-model-hub/internal/vendors/opencodego"
	"free-model-hub/internal/vendors/opencodezen"
	"free-model-hub/internal/vendors/qoder"
	"free-model-hub/internal/vendors/raccoon"
	"free-model-hub/internal/vendors/trae"
	"free-model-hub/internal/vendors/workbuddy"
	"free-model-hub/internal/vendors/zcode"
)

// BuiltinFactories returns the built-in provider factories (order determines registration order).
func BuiltinFactories() []core.VendorFactory {
	return []core.VendorFactory{
		{Instances: autoclaw.Instances},
		{Instances: func(env core.Env) []core.Vendor { return cline.Instances(env) }},
		{Instances: codearts.Instances},
		{Instances: minimaxcode.Instances},
		{Instances: func(env core.Env) []core.Vendor { return opencodezen.Instances(env) }},
		{Instances: qoder.Instances},
		{Instances: raccoon.Instances},
		{Instances: trae.Instances},
		{Instances: workbuddy.Instances},
		{Instances: zcode.Instances},
		{Instances: opencodego.Instances},
		{Instances: openaicompat.Instances},
	}
}
