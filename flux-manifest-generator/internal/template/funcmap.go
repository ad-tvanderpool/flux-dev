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

// Package template holds the function map shared by every Go
// text/template surface in the controller: pipeline expressions
// (load.keyExpr, filter.where, map.expr, group.keyExpr) and the
// artifact render engine.
//
// Centralising the base FuncMap means a helper added or removed here
// shows up in both surfaces at once — there is no second list of
// helpers to keep in sync. The pipeline and the render engine each
// apply their own additional policy on top: the render engine wires in
// the recursive helpers (`include`, `tpl`) and any Helm-style additions
// once it has a *template.Template handle to bind them to; the
// pipeline doesn't, because per-item expressions evaluate against
// scalar bindings and never need to recurse.
package template

import (
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// FuncMap returns the base function map shared by pipeline expressions
// and the artifact render engine. It is Sprig minus the
// environment-leaking helpers (`env`, `expandenv`) — these would let a
// spec author exfiltrate controller-host state into rendered output —
// and minus the recursive helpers (`include`, `tpl`) that only make
// sense once a render engine is in scope. Callers that have such a
// scope (the render engine) re-add the recursive helpers bound to
// their own *template.Template.
//
// A fresh map is returned on every call so callers may freely add or
// override entries without affecting one another.
func FuncMap() template.FuncMap {
	fm := sprig.TxtFuncMap()
	delete(fm, "env")
	delete(fm, "expandenv")
	delete(fm, "include")
	delete(fm, "tpl")
	return fm
}
