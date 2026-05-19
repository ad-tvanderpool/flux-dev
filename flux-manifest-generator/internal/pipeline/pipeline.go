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

// Package pipeline evaluates the declarative ManifestGenerator
// pipeline. Each spec.pipeline entry is compiled into a Step that, at
// reconcile time, produces a single named output. Later steps reference
// earlier outputs by name; ordering is implied by data dependency.
//
// Verbs supported in v1: load, filter, map, group, merge. All five
// are implemented; only `load` reads source-controller artifacts —
// `filter`, `map`, `group`, and `merge` reshape the outputs of prior
// steps.
package pipeline

import (
	"context"
	"fmt"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// Outputs maps a step name to the value the step produced.
type Outputs map[string]any

// Scope is the read-only view a Step sees while it is being evaluated:
// the alias→local-dir map for source artifacts, plus the outputs of
// every previously evaluated step keyed by name.
type Scope struct {
	Sources map[string]string
	Outputs Outputs
}

// Step is one compiled pipeline operation.
type Step interface {
	// Name reports the step's unique name, which is also the key it
	// publishes into the Outputs map.
	Name() string
	// Eval produces the step's value given the accumulated scope.
	Eval(ctx context.Context, scope *Scope) (any, error)
}

// Evaluator runs a compiled pipeline. Build one with Compile and then
// invoke Run once per reconcile.
type Evaluator struct {
	steps []Step
}

// Compile validates the spec and builds an Evaluator. aliases is the
// set of source aliases known to the parent ManifestGenerator; load
// steps that reference an unknown alias are rejected here so the error
// surfaces at the validator before any artifact fetches happen.
//
// Compilation also enforces the forward-reference rule: a step's `from`
// must name an earlier step. The pipeline is evaluated in spec order;
// allowing back-references keeps the data-dependency story honest while
// still failing fast on typos.
func Compile(spec []mgapi.PipelineStep, aliases map[string]bool) (*Evaluator, error) {
	seen := make(map[string]bool, len(spec))
	steps := make([]Step, 0, len(spec))
	for i := range spec {
		ps := &spec[i]
		if seen[ps.Name] {
			return nil, fmt.Errorf("step %q: duplicate step name", ps.Name)
		}

		s, err := compileStep(ps, aliases, seen)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", ps.Name, err)
		}
		// Record the name only after a successful compile so a step
		// cannot reference itself via `from`.
		seen[ps.Name] = true
		steps = append(steps, s)
	}
	return &Evaluator{steps: steps}, nil
}

// Len reports how many compiled steps the evaluator holds.
func (e *Evaluator) Len() int { return len(e.steps) }

// Run executes every step in order and returns the named outputs.
// sources maps each declared source alias to the local directory the
// controller fetched its artifact into.
func (e *Evaluator) Run(ctx context.Context, sources map[string]string) (Outputs, error) {
	scope := &Scope{Sources: sources, Outputs: make(Outputs, len(e.steps))}
	for _, step := range e.steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		val, err := step.Eval(ctx, scope)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", step.Name(), err)
		}
		scope.Outputs[step.Name()] = val
	}
	return scope.Outputs, nil
}

// compileStep dispatches on the verb set in the PipelineStep. The CRD
// schema enforces "exactly one verb" at admission time via an
// XValidation rule; the default case below is the defensive runtime
// guard for callers that bypass schema validation (e.g. unit tests).
func compileStep(ps *mgapi.PipelineStep, aliases map[string]bool, seen map[string]bool) (Step, error) {
	switch {
	case ps.Load != nil:
		return compileLoad(ps, aliases)
	case ps.Filter != nil:
		return compileFilter(ps, seen)
	case ps.Map != nil:
		return compileMap(ps, seen)
	case ps.Group != nil:
		return compileGroup(ps, seen)
	case ps.Merge != nil:
		return compileMerge(ps, seen)
	default:
		return nil, fmt.Errorf("no verb set")
	}
}
