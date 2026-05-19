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
	"strings"
	"testing"
)

// TestHelpers_RoundTrip walks every Helm-style helper through a
// representative template fragment, asserting on the rendered output
// rather than re-implementing the helper. Helpers are exercised
// through the Engine on purpose: tests this way also cover the
// FuncMap wiring, not just the helper bodies in isolation.
func TestHelpers_RoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		data    any
		want    string
		wantErr string
	}{
		{
			name: "toYaml strips trailing newline",
			src:  "{{ toYaml .x }}",
			data: map[string]any{"x": map[string]any{"a": 1, "b": "two"}},
			want: "a: 1\nb: two",
		},
		{
			name: "fromYaml parses string into map",
			src:  `{{ (fromYaml "a: 1\nb: two").a }}`,
			want: "1",
		},
		{
			name: "fromYaml records error under Error key",
			src:  `{{ (fromYaml ":\n:\n  bad").Error }}`,
			want: "error converting YAML to JSON: yaml: did not find expected key",
		},
		{
			name: "toJson emits compact json",
			src:  "{{ toJson .x }}",
			data: map[string]any{"x": map[string]any{"k": "v"}},
			want: `{"k":"v"}`,
		},
		{
			name: "fromJson parses string into map",
			src:  `{{ (fromJson "{\"k\":\"v\"}").k }}`,
			want: "v",
		},
		{
			name: "required passes through non-empty value",
			src:  `{{ required "need x" .x }}`,
			data: map[string]any{"x": "ok"},
			want: "ok",
		},
		{
			name:    "required fails on nil",
			src:     `{{ required "need x" .x }}`,
			data:    map[string]any{},
			wantErr: "need x",
		},
		{
			name:    "required fails on empty string",
			src:     `{{ required "need x" .x }}`,
			data:    map[string]any{"x": ""},
			wantErr: "need x",
		},
		{
			name: "tpl evaluates a string template",
			src:  `{{ tpl "{{ .x }}-{{ .y }}" . }}`,
			data: map[string]any{"x": "a", "y": "b"},
			want: "a-b",
		},
		{
			name: "tpl can recurse through Sprig",
			src:  `{{ tpl "{{ .x | upper }}" . }}`,
			data: map[string]any{"x": "abc"},
			want: "ABC",
		},
		{
			name:    "include against unknown name errors",
			src:     `{{ include "missing" . }}`,
			wantErr: `include: template "missing" is not defined`,
		},
		{
			name: "include against parsed sub-template renders it",
			// `define` parses a sub-template into the same tree, then
			// `include` resolves it by name. This proves the helper is
			// bound to the per-render template root rather than to a
			// global registry.
			src:  `{{ define "greet" }}hello {{ .name }}{{ end }}{{ include "greet" . }}`,
			data: map[string]any{"name": "world"},
			want: "hello world",
		},
	}

	e := NewGoEngine()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.Render("h", []byte(tc.src), tc.data)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (output %q)", tc.wantErr, out)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(out) != tc.want {
				t.Fatalf("output mismatch: got %q want %q", string(out), tc.want)
			}
		})
	}
}

// TestToYAML_MarshalError covers the documented failure mode where
// `toYaml` returns the empty string rather than aborting the render.
// Channels cannot be marshalled to YAML, so they exercise the error
// path without needing a contrived custom type.
func TestToYAML_MarshalError(t *testing.T) {
	got := toYAML(make(chan int))
	if got != "" {
		t.Fatalf("toYaml on unmarshalable value: got %q, want empty string", got)
	}
}

// TestFromYAML_DirectErrorKey verifies the helper's failure-reporting
// shape independently of the Engine, since callers may inspect the
// Error key from outside a template.
func TestFromYAML_DirectErrorKey(t *testing.T) {
	m := fromYAML(":\n:\n  bad")
	if _, ok := m["Error"].(string); !ok {
		t.Fatalf("expected Error key with string value, got %#v", m)
	}
}

// TestFromJSON_DirectErrorKey is the JSON twin of the YAML test
// above: malformed input is surfaced via the `Error` key in the
// returned map.
func TestFromJSON_DirectErrorKey(t *testing.T) {
	m := fromJSON("{not json")
	if _, ok := m["Error"].(string); !ok {
		t.Fatalf("expected Error key with string value, got %#v", m)
	}
}
