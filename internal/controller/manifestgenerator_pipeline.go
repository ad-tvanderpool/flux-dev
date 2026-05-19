/*
Copyright 2026 Travis Vanderpool

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/pipeline"
)

// runPipeline compiles and evaluates spec.pipeline against the freshly
// fetched source-controller artifacts. Returns nil outputs when the
// spec has no pipeline so callers can treat the absence the same as an
// empty map.
//
// Slice 6 onward: pipeline outputs are handed to the artifact render
// engine as the top-level template scope so a template can read a
// step's value as `.<stepName>`. Evaluation failures still surface as
// PipelineFailedReason before any template render is attempted.
func (r *ManifestGeneratorReconciler) runPipeline(ctx context.Context,
	obj *mgapi.ManifestGenerator,
	sources map[string]string) (pipeline.Outputs, error) {
	if len(obj.Spec.Pipeline) == 0 {
		return nil, nil
	}

	aliases := make(map[string]bool, len(obj.Spec.Sources))
	for _, src := range obj.Spec.Sources {
		aliases[src.Alias] = true
	}

	ev, err := pipeline.Compile(obj.Spec.Pipeline, aliases)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	outs, err := ev.Run(ctx, sources)
	if err != nil {
		return nil, fmt.Errorf("evaluate: %w", err)
	}
	return outs, nil
}
