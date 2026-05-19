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

// mergeStep is the compiled form of a `merge` PipelineStep.
type mergeStep struct {
	name string
	from []string
}

// compileMerge validates and compiles a `merge` PipelineStep. Every
// source must reference an earlier step; duplicates within `from` are
// rejected because they make the "later wins" semantics ambiguous —
// the spec author probably meant a single mention.
func compileMerge(spec *mgapi.PipelineStep, seen map[string]bool) (Step, error) {
	ms := spec.Merge
	dedup := make(map[string]bool, len(ms.From))
	for _, name := range ms.From {
		if !seen[name] {
			return nil, fmt.Errorf("merge: from %q does not reference a prior step", name)
		}
		if dedup[name] {
			return nil, fmt.Errorf("merge: from %q appears more than once", name)
		}
		dedup[name] = true
	}
	return &mergeStep{name: spec.Name, from: append([]string(nil), ms.From...)}, nil
}

func (s *mergeStep) Name() string { return s.name }

// Eval deep-merges every referenced step's output, left-to-right, with
// Helm-style values semantics: later entries override earlier on key
// collisions; nested maps recurse; lists and scalars are replaced
// wholesale. Each source must be a map[string]any so the result is
// itself a map. The output is always a fresh tree — input outputs are
// never mutated, so later pipeline steps see them unchanged.
func (s *mergeStep) Eval(_ context.Context, scope *Scope) (any, error) {
	var acc any = map[string]any{}
	for _, name := range s.from {
		val, ok := scope.Outputs[name]
		if !ok {
			return nil, fmt.Errorf("from %q is not defined", name)
		}
		if _, isMap := val.(map[string]any); !isMap {
			return nil, fmt.Errorf("from %q is not a map (got %T)", name, val)
		}
		acc = deepMerge(acc, val)
	}
	return acc, nil
}

// deepMerge returns a new value combining a and b. When both are maps,
// keys recurse; on any other type pair, b wins. The implementation
// never mutates a or b: every map and slice put into the result is a
// deep clone, so writers on either side of the merge cannot reach back
// into a pipeline output through aliasing.
func deepMerge(a, b any) any {
	am, aOk := a.(map[string]any)
	bm, bOk := b.(map[string]any)
	if !aOk || !bOk {
		return deepClone(b)
	}
	out := make(map[string]any, len(am)+len(bm))
	for k, v := range am {
		out[k] = deepClone(v)
	}
	for k, v := range bm {
		if cur, ok := out[k]; ok {
			out[k] = deepMerge(cur, v)
		} else {
			out[k] = deepClone(v)
		}
	}
	return out
}

// deepClone recursively copies the YAML-decoder-shaped tree v so that
// merging into another tree cannot mutate the original through shared
// substructure. Scalars are returned as-is.
func deepClone(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = deepClone(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = deepClone(vv)
		}
		return out
	default:
		return v
	}
}
