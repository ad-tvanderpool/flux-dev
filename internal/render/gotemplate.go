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
//     parsed into it. `opts.Partials` are parsed into the same tree
//     before the main template so their `{{ define }}` blocks become
//     visible to `include`. The builder discovers `_helpers.tpl`
//     siblings and passes them here, giving spec authors the Helm
//     partials experience.
//   - The `lookupFile` helper is bound to `opts.LookupFile`. The
//     closure carries the per-render jail (typically the template's
//     source-artifact root) so a template that calls `lookupFile`
//     cannot escape its alias scope.
func (e *GoEngine) Render(name string, src []byte, data any, opts Options) ([]byte, error) {
	funcs := mgtemplate.FuncMap()
	addHelmHelpers(funcs)
	funcs["lookupFile"] = lookupFileFunc(opts.LookupFile)

	t := template.New(name).
		Funcs(funcs).
		Option("missingkey=zero")
	// Bind recursive helpers after the template handle exists so they
	// can execute named sub-templates against the same tree.
	t.Funcs(template.FuncMap{
		"include": includeFunc(t),
		"tpl":     tplFunc(t),
	})

	// Parse partials first so their `{{ define }}` blocks are
	// available to the main template (and to any other partial
	// parsed later). Each partial gets its own associated-template
	// name for traceability; if two partials redefine the same
	// sub-template, text/template lets the later parse win, which
	// matches the builder's root-first → template-closest ordering.
	for _, p := range opts.Partials {
		if _, err := t.New(p.Name).Parse(string(p.Src)); err != nil {
			return nil, &Error{Template: p.Name, Err: fmt.Errorf("parse partial: %w", err)}
		}
	}

	if _, err := t.Parse(string(src)); err != nil {
		return nil, &Error{Template: name, Err: fmt.Errorf("parse: %w", err)}
	}

	var buf bytes.Buffer
	// ExecuteTemplate by name picks the root template explicitly so
	// the body parsed above runs even when one of the partials
	// happens to share the same template name (defensive; the builder
	// avoids collisions but we shouldn't rely on it).
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, &Error{Template: name, Err: fmt.Errorf("execute: %w", err)}
	}
	return buf.Bytes(), nil
}
