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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"sigs.k8s.io/yaml"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// maxLoadFileBytes caps the bytes read per file during pipeline load.
// Files over the cap fail the step so the controller surfaces a clear
// error rather than OOMing on a hostile or accidentally huge file. The
// cap is generous for realistic config inputs (cluster manifests, values
// files, helper templates) and can be revisited if real workloads need
// more.
const maxLoadFileBytes int64 = 10 << 20 // 10 MiB

// loadStep is the compiled form of a `load` PipelineStep.
type loadStep struct {
	name    string
	alias   string
	pattern string
	format  string
	asMap   bool
	// keyExpr is set when asMap is true; nil otherwise.
	keyExpr *keyExprEvaluator
}

// compileLoad turns a `load` PipelineStep into a Step. It performs all
// the static validation that does not require a fetched source so the
// caller can fail at admission/validation time rather than mid-fetch.
func compileLoad(spec *mgapi.PipelineStep, aliases map[string]bool) (Step, error) {
	ls := spec.Load
	alias, pattern, ok := splitSourceRef(ls.From)
	if !ok {
		return nil, fmt.Errorf("load: invalid `from` reference %q", ls.From)
	}
	if !aliases[alias] {
		return nil, fmt.Errorf("load: source alias %q not declared in spec.sources", alias)
	}
	if err := validateGlob(pattern); err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}

	format := ls.Format
	if format == "" {
		format = mgapi.LoadFormatYAML
	}
	switch format {
	case mgapi.LoadFormatYAML, mgapi.LoadFormatJSON, mgapi.LoadFormatText, mgapi.LoadFormatRaw:
	default:
		return nil, fmt.Errorf("load: unsupported format %q", format)
	}

	asShape := ls.As
	if asShape == "" {
		asShape = mgapi.LoadAsList
	}

	s := &loadStep{
		name:    spec.Name,
		alias:   alias,
		pattern: pattern,
		format:  format,
		asMap:   asShape == mgapi.LoadAsMap,
	}

	if s.asMap {
		if ls.KeyExpr == "" {
			return nil, fmt.Errorf("load: `as: map` requires `keyExpr`")
		}
		ke, err := newKeyExprEvaluator(spec.Name, ls.KeyExpr)
		if err != nil {
			return nil, fmt.Errorf("load: keyExpr: %w", err)
		}
		s.keyExpr = ke
	} else if ls.KeyExpr != "" {
		return nil, fmt.Errorf("load: keyExpr is only valid with `as: map`")
	}

	return s, nil
}

func (s *loadStep) Name() string { return s.name }

// Eval reads files matching the glob from the fetched source directory
// and shapes the result per the load spec:
//   - Non-glob pattern: returns the single decoded value.
//   - Glob with `as: list` (default): returns a sorted []any.
//   - Glob with `as: map`: keyed by `keyExpr` evaluated per item; duplicate
//     keys are an error.
//
// An empty glob match is non-fatal; it yields an empty list/map so a
// pipeline that probes for optional files does not fail spuriously.
func (s *loadStep) Eval(ctx context.Context, scope *Scope) (any, error) {
	srcDir, ok := scope.Sources[s.alias]
	if !ok {
		return nil, fmt.Errorf("source alias %q has no fetched artifact", s.alias)
	}

	matches, err := expandGlob(srcDir, s.pattern)
	if err != nil {
		return nil, err
	}

	isGlob := hasGlobMeta(s.pattern)

	if !isGlob {
		if len(matches) != 1 {
			return nil, fmt.Errorf("expected exactly one file at %q, got %d", s.pattern, len(matches))
		}
		return readDecode(srcDir, matches[0], s.format)
	}

	type item struct {
		rel   string
		value any
	}
	items := make([]item, 0, len(matches))
	for _, rel := range matches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err := readDecode(srcDir, rel, s.format)
		if err != nil {
			return nil, err
		}
		items = append(items, item{rel: rel, value: v})
	}

	if s.asMap {
		result := make(map[string]any, len(items))
		for _, it := range items {
			key, err := s.keyExpr.eval(map[string]any{"path": it.rel}, scope)
			if err != nil {
				return nil, fmt.Errorf("keyExpr for %q: %w", it.rel, err)
			}
			if _, dup := result[key]; dup {
				return nil, fmt.Errorf("keyExpr produced duplicate key %q (path %q)", key, it.rel)
			}
			result[key] = it.value
		}
		return result, nil
	}

	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it.value
	}
	return out, nil
}

// splitSourceRef parses an `@<alias>/<rest>` reference into its
// alias and the path/glob remainder. Both halves must be non-empty.
func splitSourceRef(ref string) (alias, rest string, ok bool) {
	if !strings.HasPrefix(ref, "@") {
		return "", "", false
	}
	body := ref[1:]
	idx := strings.IndexByte(body, '/')
	if idx <= 0 || idx == len(body)-1 {
		return "", "", false
	}
	return body[:idx], body[idx+1:], true
}

// validateGlob rejects patterns that try to escape the source root or
// use an absolute path. Forward-slash semantics match the fs.FS shape
// doublestar globs through.
func validateGlob(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("glob is empty")
	}
	if path.IsAbs(pattern) || strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("glob must be a relative path")
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == ".." {
			return fmt.Errorf("glob must not contain '..' segments")
		}
	}
	return nil
}

// hasGlobMeta reports whether pattern contains any character doublestar
// or path.Match treats as a glob meta-character.
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[{")
}

// expandGlob returns the sorted relative paths inside srcDir that match
// pattern. A non-glob pattern is treated as a literal file path and
// returned as a single-element slice if (and only if) it resolves to a
// regular file.
func expandGlob(srcDir, pattern string) ([]string, error) {
	fsys := os.DirFS(srcDir)

	if !hasGlobMeta(pattern) {
		info, err := fs.Stat(fsys, pattern)
		if err != nil {
			return nil, fmt.Errorf("source path %q: %w", pattern, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("source path %q is not a regular file", pattern)
		}
		return []string{pattern}, nil
	}

	matches, err := doublestar.Glob(fsys, pattern, doublestar.WithFilesOnly())
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", pattern, err)
	}
	sort.Strings(matches)
	return matches, nil
}

// readDecode reads and decodes a single file inside srcDir according to
// format. Size is bounded by maxLoadFileBytes so a hostile or huge
// source artifact can't OOM the controller.
func readDecode(srcDir, rel, format string) (any, error) {
	f, err := os.Open(filepath.Join(srcDir, rel))
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", rel, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", rel)
	}
	if info.Size() > maxLoadFileBytes {
		return nil, fmt.Errorf("%q is %d bytes which exceeds the per-file load limit of %d bytes",
			rel, info.Size(), maxLoadFileBytes)
	}

	// LimitReader with +1 so we can detect overruns that slipped past
	// the Size() check (e.g. file grew between Stat and Read).
	data, err := io.ReadAll(io.LimitReader(f, maxLoadFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", rel, err)
	}
	if int64(len(data)) > maxLoadFileBytes {
		return nil, fmt.Errorf("%q exceeded the per-file load limit of %d bytes during read",
			rel, maxLoadFileBytes)
	}

	switch format {
	case mgapi.LoadFormatYAML:
		var v any
		if err := yaml.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("decode %q as yaml: %w", rel, err)
		}
		return v, nil
	case mgapi.LoadFormatJSON:
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("decode %q as json: %w", rel, err)
		}
		return v, nil
	case mgapi.LoadFormatText:
		return string(data), nil
	case mgapi.LoadFormatRaw:
		return data, nil
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}
