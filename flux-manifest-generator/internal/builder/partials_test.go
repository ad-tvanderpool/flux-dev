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

package builder

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPartialChainDirs pins the directory walk order that
// discoverPartials relies on: root first, then each parent, then the
// template's own directory.
func TestPartialChainDirs(t *testing.T) {
	cases := []struct {
		dir  string
		want []string
	}{
		{"", []string{""}},
		{"a", []string{"", "a"}},
		{"a/b", []string{"", "a", "a/b"}},
		{"a/b/c", []string{"", "a", "a/b", "a/b/c"}},
	}
	for _, tc := range cases {
		got := partialChainDirs(tc.dir)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("partialChainDirs(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// TestDiscoverPartials_RootAndDescend layers two `_helpers.tpl`
// files: one at the alias root, one in a sibling directory of the
// template. Both must be returned in root-first order so the render
// engine's last-define-wins semantics give the closer file
// precedence. A template that lives in a directory without a sibling
// `_helpers.tpl` still picks up the root-level one.
func TestDiscoverPartials_RootAndDescend(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "_helpers.tpl"), `{{ define "h" }}root{{ end }}`)
	mustMkdir(t, filepath.Join(root, "templates"))
	mustWriteFile(t, filepath.Join(root, "templates", "_helpers.tpl"), `{{ define "h" }}leaf{{ end }}`)
	mustWriteFile(t, filepath.Join(root, "templates", "deploy.yaml"), "irrelevant")

	srcRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srcRoot.Close()

	got, err := discoverPartials(srcRoot, "templates/deploy.yaml")
	if err != nil {
		t.Fatalf("discoverPartials: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 partials, got %d: %+v", len(got), got)
	}
	if got[0].Name != "_helpers.tpl" {
		t.Fatalf("first partial should be the root file, got %q", got[0].Name)
	}
	if got[1].Name != "templates/_helpers.tpl" {
		t.Fatalf("second partial should be the leaf file, got %q", got[1].Name)
	}
}

// TestDiscoverPartials_None covers the common case: no
// `_helpers.tpl` anywhere in the chain returns an empty slice (not an
// error) so the build can proceed and `include` calls will fail at
// execute time with the engine's own "not defined" message.
func TestDiscoverPartials_None(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "templates"))
	mustWriteFile(t, filepath.Join(root, "templates", "deploy.yaml"), "x")

	srcRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srcRoot.Close()

	got, err := discoverPartials(srcRoot, "templates/deploy.yaml")
	if err != nil {
		t.Fatalf("discoverPartials: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 partials, got %d", len(got))
	}
}

// TestDiscoverPartials_TemplateAtRoot exercises the edge case where
// the template itself sits at the alias root: only the root-level
// `_helpers.tpl` is considered (there's no parent to walk into).
func TestDiscoverPartials_TemplateAtRoot(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "_helpers.tpl"), `{{ define "h" }}r{{ end }}`)
	mustWriteFile(t, filepath.Join(root, "deploy.yaml"), "x")

	srcRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srcRoot.Close()

	got, err := discoverPartials(srcRoot, "deploy.yaml")
	if err != nil {
		t.Fatalf("discoverPartials: %v", err)
	}
	if len(got) != 1 || got[0].Name != "_helpers.tpl" {
		t.Fatalf("expected single root partial, got %+v", got)
	}
}

// TestNewLookupFile_RoundTrip exercises the happy path of the
// `lookupFile` backing closure: a relative path under the root reads
// the file's bytes verbatim.
func TestNewLookupFile_RoundTrip(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "snippets"))
	mustWriteFile(t, filepath.Join(root, "snippets", "x.yaml"), "hello world")

	srcRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srcRoot.Close()

	lookup := newLookupFile(srcRoot)
	b, err := lookup("snippets/x.yaml")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if string(b) != "hello world" {
		t.Fatalf("got %q want %q", string(b), "hello world")
	}
}

// TestNewLookupFile_RejectsEscapes covers the paths cleanLookupPath
// rejects up-front (absolute, empty, parent-escape) and confirms a
// missing file surfaces a clear error.
func TestNewLookupFile_RejectsEscapes(t *testing.T) {
	root := t.TempDir()
	srcRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srcRoot.Close()

	lookup := newLookupFile(srcRoot)
	for _, bad := range []string{
		"",
		"/etc/passwd",
		"../escape",
		"a/../../escape",
	} {
		if _, err := lookup(bad); err == nil {
			t.Fatalf("expected error for %q, got nil", bad)
		}
	}

	if _, err := lookup("missing"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	} else if !strings.Contains(err.Error(), "stat") {
		t.Fatalf("expected stat-wrapped error, got %v", err)
	}
}

// TestNewLookupFile_RejectsDirectory pins the "regular files only"
// contract: a directory at the requested path is rejected so
// templates can rely on the helper returning file contents.
func TestNewLookupFile_RejectsDirectory(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "adir"))

	srcRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srcRoot.Close()

	if _, err := newLookupFile(srcRoot)("adir"); err == nil {
		t.Fatal("expected error reading a directory through lookupFile, got nil")
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}
