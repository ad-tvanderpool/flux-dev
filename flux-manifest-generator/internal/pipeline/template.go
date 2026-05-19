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
	"fmt"
	"strings"
	"text/template"

	mgtemplate "github.com/tvanderpool/flux-manifest-generator/internal/template"
)

// itemTemplate is a compiled per-item template fragment. The same shape
// backs every pipeline expression: `load.keyExpr`, `filter.where`,
// `map.expr`, and `group.keyExpr` each compile once and render per item
// at evaluation time. Callers supply the per-item bindings via the data
// map passed to render.
type itemTemplate struct {
	tmpl *template.Template
}

// newItemTemplate compiles expr. field names the spec field whose value
// is being parsed (for error messages) and stepName is the pipeline
// step the fragment belongs to; both end up in text/template's template
// name so parse / execution errors are easy to trace back to the spec
// entry that produced them.
func newItemTemplate(field, stepName, expr string) (*itemTemplate, error) {
	if expr == "" {
		return nil, fmt.Errorf("%s is empty", field)
	}
	tmpl, err := template.New(fmt.Sprintf("%s[%s]", field, stepName)).
		Funcs(mgtemplate.FuncMap()).
		Option("missingkey=error").
		Parse(expr)
	if err != nil {
		return nil, err
	}
	return &itemTemplate{tmpl: tmpl}, nil
}

// render executes the template against data and returns the raw rendered
// string. The caller decides what to do with empty / non-conforming
// output (trimming, parsing, truthiness checks, etc).
func (it *itemTemplate) render(data map[string]any) (string, error) {
	var sb strings.Builder
	if err := it.tmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// keyExprEvaluator wraps an itemTemplate compiled for a `keyExpr`
// fragment. It enforces the shared keyExpr contract: trim the rendered
// string, reject an empty result so the caller can surface a clear error
// rather than silently colliding map keys.
type keyExprEvaluator struct {
	it *itemTemplate
}

// newKeyExprEvaluator compiles expr as a `keyExpr` fragment for the
// named step.
func newKeyExprEvaluator(stepName, expr string) (*keyExprEvaluator, error) {
	it, err := newItemTemplate("keyExpr", stepName, expr)
	if err != nil {
		return nil, err
	}
	return &keyExprEvaluator{it: it}, nil
}

// eval renders the template against the supplied bindings and returns
// the resulting key as a trimmed string. The bindings are unioned with
// every prior step output before evaluation so a keyExpr can reference
// earlier pipeline values the same way templates do.
func (k *keyExprEvaluator) eval(bindings map[string]any, scope *Scope) (string, error) {
	data := withOutputs(bindings, scope)
	out, err := k.it.render(data)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("keyExpr produced an empty key")
	}
	return out, nil
}

// withOutputs builds the data dictionary passed to text/template by
// overlaying the supplied per-item bindings on top of the scope's
// named step outputs and the merged `.values` tree. Per-item bindings
// win on collision so callers can rely on `.value` / `.key` / `.index`
// having their well-known meanings even if a prior step happens to
// share a name. The `values` key is reserved (Compile and the
// controller validator both reject collisions) so it always resolves
// to scope.Values.
func withOutputs(bindings map[string]any, scope *Scope) map[string]any {
	data := make(map[string]any, len(bindings)+len(scope.Outputs)+1)
	for name, val := range scope.Outputs {
		data[name] = val
	}
	data[ValuesKey] = scope.Values
	for name, val := range bindings {
		data[name] = val
	}
	return data
}
