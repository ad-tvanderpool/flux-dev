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

package pipeline

import (
	"context"
	"fmt"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// groupStep is the compiled form of a `group` PipelineStep.
type groupStep struct {
	name    string
	from    string
	keyExpr *keyExprEvaluator
}

// compileGroup validates and compiles a `group` PipelineStep.
func compileGroup(spec *mgapi.PipelineStep, seen map[string]bool) (Step, error) {
	gs := spec.Group
	if !seen[gs.From] {
		return nil, fmt.Errorf("group: from %q does not reference a prior step", gs.From)
	}
	ke, err := newKeyExprEvaluator(spec.Name, gs.KeyExpr)
	if err != nil {
		return nil, fmt.Errorf("group: keyExpr: %w", err)
	}
	return &groupStep{name: spec.Name, from: gs.From, keyExpr: ke}, nil
}

func (s *groupStep) Name() string { return s.name }

// Eval buckets the items of a previous list-shaped step into a map of
// lists keyed by `keyExpr`. Within each bucket, items keep their input
// order so multi-group runs over the same data are deterministic.
//
// Grouping a map is not supported — maps are already keyed; the design
// is that group converts a list-of-records into a map-of-lists. The
// caller can pre-flatten a map with a map step if needed.
func (s *groupStep) Eval(ctx context.Context, scope *Scope) (any, error) {
	list, err := listInput(scope.Outputs, s.from)
	if err != nil {
		return nil, err
	}

	result := make(map[string]any, 0)
	for i, item := range list {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, err := s.keyExpr.eval(map[string]any{
			"value": item,
			"index": i,
		}, scope)
		if err != nil {
			return nil, fmt.Errorf("keyExpr at index %d: %w", i, err)
		}
		bucket, _ := result[key].([]any)
		result[key] = append(bucket, item)
	}
	return result, nil
}
