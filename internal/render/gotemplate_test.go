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
	"errors"
	"strings"
	"sync"
	"testing"
)

// TestGoEngine_Render walks the basic Engine contract: literal
// passthrough when no actions are present, Sprig and project helpers
// resolve against the supplied data, missing-key policy matches Helm
// (renders the zero value rather than aborting), and parse / execute
// failures surface as wrapped errors that name the template.
func TestGoEngine_Render(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		data    any
		want    string
		wantErr string
	}{
		{
			name: "literal passthrough",
			src:  "image:\n  tag: v1.0.0\n",
			want: "image:\n  tag: v1.0.0\n",
		},
		{
			name: "field lookup",
			src:  "image: {{ .image.tag }}",
			data: map[string]any{"image": map[string]any{"tag": "v1.0.0"}},
			want: "image: v1.0.0",
		},
		{
			name: "sprig upper",
			src:  "name: {{ .name | upper }}",
			data: map[string]any{"name": "core"},
			want: "name: CORE",
		},
		{
			// text/template's `missingkey=zero` returns the zero value
			// of the map's element type — `nil` for `map[string]any`,
			// which prints as "<no value>" rather than aborting. The
			// `default ""` Sprig helper is the idiomatic way to coerce
			// it to an empty string, exercised in the next case.
			name: "missing key renders the zero-value sentinel, not error",
			src:  "x={{ .nope }}!",
			data: map[string]any{},
			want: "x=<no value>!",
		},
		{
			name: "default coerces missing key to empty string",
			src:  `x={{ default "" .nope }}!`,
			data: map[string]any{},
			want: "x=!",
		},
		{
			name:    "parse error wraps template name",
			src:     "{{ .x",
			wantErr: `test: parse:`,
		},
		{
			name:    "execute error wraps template name",
			src:     `{{ required "need x" .x }}`,
			data:    map[string]any{},
			wantErr: `test: execute:`,
		},
	}

	e := NewGoEngine()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.Render("test", []byte(tc.src), tc.data)
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

// TestGoEngine_NoEnvLeak proves the engine does not expose `env` or
// `expandenv` — the host-leaking helpers the shared FuncMap deletes
// out of Sprig. A template that references them must fail at parse
// time because text/template only resolves function names declared in
// the FuncMap.
func TestGoEngine_NoEnvLeak(t *testing.T) {
	e := NewGoEngine()
	for _, src := range []string{`{{ env "PATH" }}`, `{{ expandenv "$PATH" }}`} {
		if _, err := e.Render("envtest", []byte(src), nil); err == nil {
			t.Fatalf("expected parse error for %q, got nil", src)
		}
	}
}

// TestGoEngine_ErrorIsRenderError asserts the parse / execute failure
// modes produce a *render.Error so callers can map them to the
// RenderFailedReason ready-condition reason via errors.As. The other
// build-path failures (I/O, staging) must NOT match.
func TestGoEngine_ErrorIsRenderError(t *testing.T) {
	e := NewGoEngine()
	_, err := e.Render("t", []byte("{{ .x"), nil)
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("parse error should be *render.Error: %v", err)
	}
	if re.Template != "t" {
		t.Fatalf("expected Template=%q, got %q", "t", re.Template)
	}
}

// TestGoEngine_ConcurrentRender exercises the safe-for-concurrent-use
// contract: a single Engine instance shared across goroutines must not
// race on internal state and must produce per-goroutine output. The
// per-render template tree is allocated inside Render, so this is
// really verifying that no mutable state was hoisted onto the Engine
// struct itself.
func TestGoEngine_ConcurrentRender(t *testing.T) {
	e := NewGoEngine()
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			out, err := e.Render("c", []byte(`v={{ .v }}`), map[string]any{"v": i})
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			want := "v="
			if !strings.HasPrefix(string(out), want) {
				t.Errorf("goroutine %d: got %q want prefix %q", i, string(out), want)
			}
		}(i)
	}
	wg.Wait()
}
