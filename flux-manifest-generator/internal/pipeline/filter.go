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
	"strconv"
	"strings"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// filterStep is the compiled form of a `filter` PipelineStep.
type filterStep struct {
	name  string
	from  string
	where *itemTemplate
}

// compileFilter validates and compiles a `filter` PipelineStep. seen is
// the set of step names that appear before this one in the spec; we
// reject a forward `from` reference at compile time so spec authors
// learn about the typo at admission rather than at fetch.
func compileFilter(spec *mgapi.PipelineStep, seen map[string]bool) (Step, error) {
	fs := spec.Filter
	if !seen[fs.From] {
		return nil, fmt.Errorf("filter: from %q does not reference a prior step", fs.From)
	}
	tmpl, err := newItemTemplate("where", spec.Name, fs.Where)
	if err != nil {
		return nil, fmt.Errorf("filter: where: %w", err)
	}
	return &filterStep{name: spec.Name, from: fs.From, where: tmpl}, nil
}

func (s *filterStep) Name() string { return s.name }

// Eval renders the `where` template per item against the previous
// step's output. The rendered string is parsed as a boolean via
// truthyString. The output preserves the input collection shape: list
// in, list out; map in, map out.
func (s *filterStep) Eval(ctx context.Context, scope *Scope) (any, error) {
	src, ok := scope.Outputs[s.from]
	if !ok {
		return nil, fmt.Errorf("from %q is not defined", s.from)
	}
	switch v := src.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for i, item := range v {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			keep, err := s.evalPredicate(map[string]any{
				"value": item,
				"index": i,
			}, scope)
			if err != nil {
				return nil, fmt.Errorf("where at index %d: %w", i, err)
			}
			if keep {
				out = append(out, item)
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range sortedKeys(v) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			keep, err := s.evalPredicate(map[string]any{
				"value": v[key],
				"key":   key,
			}, scope)
			if err != nil {
				return nil, fmt.Errorf("where for key %q: %w", key, err)
			}
			if keep {
				out[key] = v[key]
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("from %q is not a list or map (got %T)", s.from, src)
	}
}

func (s *filterStep) evalPredicate(bindings map[string]any, scope *Scope) (bool, error) {
	raw, err := s.where.render(withOutputs(bindings, scope))
	if err != nil {
		return false, err
	}
	return truthyString(raw)
}

// truthyString interprets a rendered `where` value as a boolean. An
// empty string (or whitespace-only) counts as false so an expression
// like `{{ if .ok }}yes{{ end }}` short-circuits cleanly. Anything else
// must parse as a Go boolean literal (`true`, `false`, `1`, `0`,
// `True`, `False`, etc.); free-form strings are rejected so a typo in
// the spec stalls the pipeline rather than silently keeping every
// item.
func truthyString(s string) (bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("where did not produce a boolean (got %q)", s)
	}
	return b, nil
}
