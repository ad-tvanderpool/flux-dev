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

package render

import (
	"bytes"
	"fmt"
	"text/template"

	mgtemplate "github.com/tvanderpool/flux-manifest-generator/internal/template"
)

// GoEngine is the default Engine implementation. It composes the shared
// FuncMap from internal/template (Sprig minus host-leaking and
// recursion-only helpers) with the Helm-style helpers in helpers.go and
// the recursive helpers (`tpl`, `include`) which are bound to the
// per-render *template.Template so they can reach the parsed template
// tree.
type GoEngine struct{}

// NewGoEngine returns a stateless GoEngine usable across reconciles.
// The engine carries no mutable state; per-render template trees are
// built inside Render so concurrent calls do not share funcs or parsed
// templates.
func NewGoEngine() *GoEngine {
	return &GoEngine{}
}

// Render parses src as a Go text/template named name and executes it
// against data, returning the rendered output.
//
// Policy:
//   - `missingkey=zero` matches Helm's behaviour: a missing scope key
//     renders as the zero value rather than aborting. Authors that
//     want strict missing-key checks use the `required` helper for the
//     specific value they care about.
//   - The recursive helpers `tpl` and `include` are bound to the
//     per-render template tree so they can reach any sub-template
//     parsed into it. Slice 6 ships no implicit sub-templates;
//     slice 9 will discover `_helpers.tpl` siblings and parse them
//     into the same tree.
func (e *GoEngine) Render(name string, src []byte, data any) ([]byte, error) {
	funcs := mgtemplate.FuncMap()
	addHelmHelpers(funcs)

	t := template.New(name).
		Funcs(funcs).
		Option("missingkey=zero")
	// Bind recursive helpers after the template handle exists so they
	// can execute named sub-templates against the same tree.
	t.Funcs(template.FuncMap{
		"include": includeFunc(t),
		"tpl":     tplFunc(t),
	})

	if _, err := t.Parse(string(src)); err != nil {
		return nil, &Error{Template: name, Err: fmt.Errorf("parse: %w", err)}
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, &Error{Template: name, Err: fmt.Errorf("execute: %w", err)}
	}
	return buf.Bytes(), nil
}
