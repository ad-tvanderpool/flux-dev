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

	"sigs.k8s.io/yaml"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// mapStep is the compiled form of a `map` PipelineStep. The file is
// named mapverb.go because `map` is a Go reserved identifier.
type mapStep struct {
	name string
	from string
	expr *itemTemplate
}

// compileMap validates and compiles a `map` PipelineStep.
func compileMap(spec *mgapi.PipelineStep, seen map[string]bool) (Step, error) {
	ms := spec.Map
	if !seen[ms.From] {
		return nil, fmt.Errorf("map: from %q does not reference a prior step", ms.From)
	}
	tmpl, err := newItemTemplate("expr", spec.Name, ms.Expr)
	if err != nil {
		return nil, fmt.Errorf("map: expr: %w", err)
	}
	return &mapStep{name: spec.Name, from: ms.From, expr: tmpl}, nil
}

func (s *mapStep) Name() string { return s.name }

// Eval renders `expr` per item, then parses the rendered string as
// YAML to form the new item value. The output preserves the input
// shape: a list input produces a list output of the same length, a map
// input produces a map output with the same keys.
//
// The render-then-parse-YAML contract matches Helm's `tpl` semantics
// and lets spec authors emit either a scalar or a structured value
// just by choosing what their fragment prints.
func (s *mapStep) Eval(ctx context.Context, scope *Scope) (any, error) {
	src, ok := scope.Outputs[s.from]
	if !ok {
		return nil, fmt.Errorf("from %q is not defined", s.from)
	}
	switch v := src.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			val, err := s.renderItem(map[string]any{
				"value": item,
				"index": i,
			}, scope)
			if err != nil {
				return nil, fmt.Errorf("expr at index %d: %w", i, err)
			}
			out[i] = val
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range sortedKeys(v) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			val, err := s.renderItem(map[string]any{
				"value": v[key],
				"key":   key,
			}, scope)
			if err != nil {
				return nil, fmt.Errorf("expr for key %q: %w", key, err)
			}
			out[key] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf("from %q is not a list or map (got %T)", s.from, src)
	}
}

func (s *mapStep) renderItem(bindings map[string]any, scope *Scope) (any, error) {
	raw, err := s.expr.render(withOutputs(bindings, scope))
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("decode rendered expr as yaml: %w", err)
	}
	return v, nil
}
