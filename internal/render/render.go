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

// Package render implements the artifact template engine for
// ManifestGenerator. It owns the rendering surface that turns each
// `spec.artifacts[*].templates[*].from` into a file written to the
// staged artifact tarball.
//
// The default Engine is a Go text/template evaluator with the
// project-wide Sprig FuncMap and a small set of Helm-style helpers
// (`toYaml`, `fromYaml`, `toJson`, `fromJson`, `required`, `tpl`,
// `include`). The Engine interface is intentionally small so a future
// slice can plug in an alternative engine without changing the CRD
// surface or the controller wiring.
package render

// Engine renders a single template source into bytes. Implementations
// must be safe for concurrent use: the reconciler builds an Engine
// once at startup and invokes Render from per-reconcile goroutines.
//
// name is a human-readable identifier (typically the `@alias/path`
// reference from the spec) included verbatim in parse / execute error
// messages so spec authors can trace failures back to the source.
//
// data is the template scope. For slice 6 the controller passes the
// pipeline output map directly so a template fragment such as
// `{{ .cluster.name }}` resolves to the `cluster` step's value. Later
// slices will fold inline `values`, `valuesFrom`, and per-`forEach`
// bindings into the same map.
//
// Render failures (parse and execute) must be wrapped in an Error so
// callers can tell them apart from I/O or staging failures via
// errors.As; the reconciler uses that to surface RenderFailedReason
// rather than the generic BuildFailedReason.
type Engine interface {
	Render(name string, src []byte, data any) ([]byte, error)
}

// Error wraps a template parse or execute failure. The builder
// surfaces these via errors.As so the controller can map them to the
// RenderFailedReason ready-condition reason; everything else from the
// build path falls under BuildFailedReason.
type Error struct {
	Template string
	Err      error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Template == "" {
		return e.Err.Error()
	}
	return e.Template + ": " + e.Err.Error()
}

// Unwrap exposes the underlying parse/execute error for errors.Is /
// errors.As callers that want the raw template error.
func (e *Error) Unwrap() error { return e.Err }
