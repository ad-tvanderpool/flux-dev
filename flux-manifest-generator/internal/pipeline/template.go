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

	"github.com/Masterminds/sprig/v3"
)

// pipelineFuncMap returns the Go text/template function map used by
// per-item pipeline expressions (load.keyExpr, and later filter.where /
// map.expr / group.keyExpr). It is Sprig minus the environment-leaking
// helpers (`env`, `expandenv`) and minus templating recursion helpers
// that only make sense inside the artifact render engine (slice 6).
//
// Keep this in sync with the eventual render engine FuncMap so the
// pipeline and template surfaces agree on what's available.
func pipelineFuncMap() template.FuncMap {
	fm := sprig.TxtFuncMap()
	// Helm disables these for the same reason: they leak host state into
	// rendered output. We will follow suit in slice 6 for the artifact
	// renderer and apply the same policy here.
	delete(fm, "env")
	delete(fm, "expandenv")
	// `include` and `tpl` are Helm-style helpers that only make sense
	// when a render engine is in scope; pipeline expressions evaluate
	// against scalar item bindings, not template trees.
	delete(fm, "include")
	delete(fm, "tpl")
	return fm
}

// keyExprEvaluator compiles a `keyExpr` template fragment once and
// runs it per-item at evaluation time.
type keyExprEvaluator struct {
	tmpl *template.Template
}

// newKeyExprEvaluator compiles expr. The template is named after the
// step it belongs to so error messages from text/template are easy to
// trace back to a specific pipeline entry.
func newKeyExprEvaluator(stepName, expr string) (*keyExprEvaluator, error) {
	tmpl, err := template.New(fmt.Sprintf("keyExpr[%s]", stepName)).
		Funcs(pipelineFuncMap()).
		Option("missingkey=error").
		Parse(expr)
	if err != nil {
		return nil, err
	}
	return &keyExprEvaluator{tmpl: tmpl}, nil
}

// eval renders the template against the per-item bindings plus the
// accumulated pipeline outputs, and returns the resulting key as a
// trimmed string. An empty key is rejected so the caller can surface a
// clear error rather than silently colliding map keys.
func (k *keyExprEvaluator) eval(itemPath string, scope *Scope) (string, error) {
	data := map[string]any{
		"path": itemPath,
	}
	for name, val := range scope.Outputs {
		data[name] = val
	}

	var sb strings.Builder
	if err := k.tmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("keyExpr produced an empty key")
	}
	return out, nil
}
