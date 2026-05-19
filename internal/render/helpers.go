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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// addHelmHelpers registers the project's stateless Helm-style helpers
// into the supplied FuncMap. The recursive helpers (`tpl`, `include`)
// and the per-render `lookupFile` helper are bound in gotemplate.go
// because they need either a *template.Template handle or the
// per-render Options to close over.
//
// Helper surface mirrors Helm so spec authors who know the Helm
// vocabulary can use the same helpers here without surprises:
//
//   - `toYaml`, `toJson` — marshal a value; errors render as the empty
//     string (matches Helm) so a malformed sub-tree does not abort the
//     surrounding pipeline.
//   - `fromYaml`, `fromJson` — unmarshal a string into a map; any
//     decode error is recorded under the `Error` key in the returned
//     map (matches Helm) so callers can branch on it inline.
//   - `required` — fails the render with the supplied message when the
//     value is nil or an empty string. The intended pattern is
//     `{{ required "image.tag is required" .Values.image.tag }}`.
func addHelmHelpers(fm template.FuncMap) {
	fm["toYaml"] = toYAML
	fm["fromYaml"] = fromYAML
	fm["toJson"] = toJSON
	fm["fromJson"] = fromJSON
	fm["required"] = required
}

// toYAML marshals a value to YAML and returns the result. The Helm
// convention is to return the empty string on error so a marshal
// failure does not abort the surrounding template; downstream parsers
// will see the empty document and surface their own error if any.
func toYAML(v any) string {
	out, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	// yaml.Marshal always trails with a newline. Match Helm's `toYaml`
	// which trims the trailing newline so the helper composes cleanly
	// inside indented blocks.
	return strings.TrimSuffix(string(out), "\n")
}

// fromYAML parses a YAML string into a map. Decode errors are recorded
// under the `Error` key in the returned map so templates can branch
// inline (matches Helm's `fromYaml`).
func fromYAML(s string) map[string]any {
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}

// toJSON marshals a value to compact JSON. Errors yield the empty
// string (mirrors Helm's `toJson`).
func toJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// fromJSON parses a JSON string into a map. Decode errors are recorded
// under the `Error` key in the returned map (mirrors Helm's
// `fromJson`).
func fromJSON(s string) map[string]any {
	m := map[string]any{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}

// required fails the render with msg when val is nil or an empty
// string. Non-string values are returned as-is; only the empty string
// is treated as missing, matching Helm.
func required(msg string, val any) (any, error) {
	if val == nil {
		return nil, errors.New(msg)
	}
	if s, ok := val.(string); ok && s == "" {
		return nil, errors.New(msg)
	}
	return val, nil
}

// lookupFileFunc returns a closure suitable for use as the `lookupFile`
// template helper. The closure delegates to lookup, which the builder
// constructs once per template render and jails to the template's
// source-artifact root. A nil lookup means the helper is unavailable
// in this rendering context (the destination-path render path uses a
// zero render.Options and so cannot read files); in that case the
// helper fails at execute time with a clear message rather than
// silently returning an empty string.
func lookupFileFunc(lookup func(path string) ([]byte, error)) func(string) (string, error) {
	return func(path string) (string, error) {
		if lookup == nil {
			return "", errors.New("lookupFile: not available in this rendering context")
		}
		b, err := lookup(path)
		if err != nil {
			return "", fmt.Errorf("lookupFile %q: %w", path, err)
		}
		return string(b), nil
	}
}

// includeFunc returns a closure suitable for use as the `include`
// template helper. The closure executes the named sub-template parsed
// into root and returns the rendered string. Slice 9 wires in
// auto-discovered `_helpers.tpl` siblings as render.Options.Partials,
// so a template that calls `include "name"` resolves "name" against
// any `{{ define "name" }}` block published by those partials.
// Templates without partials (or that ask for an undefined name) see
// a clear error rather than a silent empty render.
func includeFunc(root *template.Template) func(string, any) (string, error) {
	return func(name string, data any) (string, error) {
		t := root.Lookup(name)
		if t == nil {
			return "", fmt.Errorf("include: template %q is not defined", name)
		}
		var sb strings.Builder
		if err := t.Execute(&sb, data); err != nil {
			return "", fmt.Errorf("include %q: %w", name, err)
		}
		return sb.String(), nil
	}
}

// tplFunc returns a closure suitable for use as the `tpl` template
// helper. The closure re-parses src as a Go template — using a clone
// of root so the new template inherits the same FuncMap and any
// sub-templates — and executes it against data, returning the
// rendered string. This mirrors Helm's `tpl`: handy for `Values`
// strings that themselves contain template syntax.
func tplFunc(root *template.Template) func(string, any) (string, error) {
	return func(src string, data any) (string, error) {
		// Each call gets a fresh sub-template so a `tpl` call cannot
		// pollute the root template tree with named definitions that
		// leak into later renders.
		t, err := root.Clone()
		if err != nil {
			return "", fmt.Errorf("tpl: clone root template: %w", err)
		}
		parsed, err := t.New("tpl").Parse(src)
		if err != nil {
			return "", fmt.Errorf("tpl: parse: %w", err)
		}
		var sb strings.Builder
		if err := parsed.Execute(&sb, data); err != nil {
			return "", fmt.Errorf("tpl: execute: %w", err)
		}
		return sb.String(), nil
	}
}
