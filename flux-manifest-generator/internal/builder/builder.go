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

// Package builder assembles ManifestGenerator artifact tarballs.
//
// Slice 7 adds two pieces on top of the slice-6 single-file render:
//   - `spec.artifacts[*].forEach` fans every `templates[*]` entry over
//     the entries of a pipeline output (a list or map). Each iteration
//     binds the current item under `.<as>` in the template scope as
//     `{key, value}` for maps or `{index, value}` for lists.
//   - `templates[*].to` is itself rendered as a template against the
//     iteration scope, so each iteration writes to a distinct path.
//     The same templating is available without `forEach`, but with no
//     iteration bindings the only useful inputs are pipeline outputs
//     and inline values.
//
// Directory references in `from` are still rejected — once rendering is
// in play, a directory copy has no well-defined semantics (which scope,
// which output names?). Authors that need to fan a template across
// multiple files use `forEach` over a pipeline `load` step that itself
// expanded a glob.
package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkstorage "github.com/fluxcd/pkg/artifact/storage"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/render"
)

// maxTemplateFileBytes caps the bytes read per template source file
// during render. Files over the cap fail with a clear error rather
// than letting a hostile or accidentally huge file balloon the
// controller's memory. The cap mirrors the pipeline `load` verb's
// per-file limit so authors see the same ceiling whichever path they
// take to read a file.
const maxTemplateFileBytes int64 = 10 << 20 // 10 MiB

// artifactRefPrefix is the leading sentinel of every `templates[*].to`
// value. Centralised here so both the parser and the destination-path
// renderer agree on the exact string.
const artifactRefPrefix = "@artifact/"

// ArtifactBuilder turns a ManifestArtifact spec plus a set of fetched
// source-controller artifact directories into a stored tarball.
type ArtifactBuilder struct {
	Storage *gotkstorage.Storage
	Engine  render.Engine
}

// New creates a new ArtifactBuilder writing to the given storage and
// rendering templates through the supplied Engine.
func New(storage *gotkstorage.Storage, engine render.Engine) *ArtifactBuilder {
	return &ArtifactBuilder{Storage: storage, Engine: engine}
}

// Build assembles the artifact for one ManifestArtifact entry. It
// stages files into a per-artifact subdirectory of workspace, packs
// them into the storage backend, and returns the resulting artifact
// metadata.
//
// data is the template scope. The controller hands the pipeline
// outputs in (so a template can read `{{ .cluster.name }}`); later
// slices will fold inline values and valuesFrom into the same map
// before calling Build. When spec.ForEach is set, Build looks up the
// referenced output in data and overlays the iteration binding under
// spec.ForEach.As for each item.
func (r *ArtifactBuilder) Build(ctx context.Context,
	spec *mgapi.ManifestArtifact,
	sources map[string]string,
	data map[string]any,
	namespace string,
	workspace string) (*gotkmeta.Artifact, error) {
	stagingDir := filepath.Join(workspace, spec.Name)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create staging dir: %w", err)
	}

	if err := r.applyArtifact(ctx, spec, sources, data, stagingDir); err != nil {
		return nil, fmt.Errorf("failed to assemble artifact %q: %w", spec.Name, err)
	}

	contentsHash, err := hashStagingDir(stagingDir, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to hash staging dir: %w", err)
	}

	artifact := r.Storage.NewArtifactFor(
		sourcev1.ExternalArtifactKind,
		&metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
		},
		spec.Name,
		fmt.Sprintf("%s.tar.gz", contentsHash),
	)

	if err := r.Storage.MkdirAll(artifact); err != nil {
		return nil, fmt.Errorf("failed to create artifact directory: %w", err)
	}

	unlock, err := r.Storage.Lock(artifact)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire artifact lock: %w", err)
	}
	defer unlock()

	if err := r.Storage.Archive(&artifact, stagingDir, gotkstorage.SourceIgnoreFilter(nil, nil)); err != nil {
		return nil, fmt.Errorf("failed to archive artifact: %w", err)
	}

	artifact.Revision = fmt.Sprintf("latest@%s", artifact.Digest)

	return artifact.DeepCopy(), nil
}

// applyArtifact resolves the iteration plan for the artifact (one
// iteration without forEach, N iterations with) and renders every
// template once per iteration into the staging directory.
func (r *ArtifactBuilder) applyArtifact(ctx context.Context,
	spec *mgapi.ManifestArtifact,
	sources map[string]string,
	data map[string]any,
	stagingDir string) error {
	iters, err := buildIterations(spec, data)
	if err != nil {
		return err
	}

	stagingRoot, err := os.OpenRoot(stagingDir)
	if err != nil {
		return fmt.Errorf("failed to open staging root %q: %w", stagingDir, err)
	}
	defer stagingRoot.Close()

	// Track destination paths so two iterations rendering the same
	// `to:` collide loudly rather than silently overwriting one
	// another. The earlier write would simply be lost otherwise.
	writtenPaths := make(map[string]string, len(iters)*len(spec.Templates))

	for _, iter := range iters {
		if err := ctx.Err(); err != nil {
			return err
		}
		iterData := overlayData(data, iter.bindings)
		for ti := range spec.Templates {
			t := &spec.Templates[ti]
			if err := r.applyTemplate(ctx, t, sources, iterData, stagingRoot, writtenPaths, iter.label); err != nil {
				if iter.label != "" {
					return fmt.Errorf("template %q -> %q (iteration %s): %w",
						t.From, t.To, iter.label, err)
				}
				return fmt.Errorf("template %q -> %q: %w", t.From, t.To, err)
			}
		}
	}
	return nil
}

// iteration is one expansion of an artifact's forEach scope. label is
// a short, human-readable identifier ("map key:foo", "list index:3")
// used in error messages so a render failure points back at the
// offending entry; bindings is the per-iteration template-scope
// overlay. Both are empty for an artifact without forEach.
type iteration struct {
	label    string
	bindings map[string]any
}

// buildIterations expands the artifact's forEach into a deterministic
// slice of iterations. The result is a single empty iteration when
// forEach is absent — the rest of the build path treats no-forEach as
// "one iteration with no overlay".
func buildIterations(spec *mgapi.ManifestArtifact, data map[string]any) ([]iteration, error) {
	if spec.ForEach == nil {
		return []iteration{{}}, nil
	}

	src, ok := data[spec.ForEach.From]
	if !ok {
		return nil, fmt.Errorf("forEach.from %q is not defined", spec.ForEach.From)
	}

	switch v := src.(type) {
	case []any:
		iters := make([]iteration, len(v))
		for i, item := range v {
			iters[i] = iteration{
				label: fmt.Sprintf("list index:%d", i),
				bindings: map[string]any{
					spec.ForEach.As: map[string]any{
						"index": i,
						"value": item,
					},
				},
			}
		}
		return iters, nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		iters := make([]iteration, len(keys))
		for i, k := range keys {
			iters[i] = iteration{
				label: fmt.Sprintf("map key:%s", k),
				bindings: map[string]any{
					spec.ForEach.As: map[string]any{
						"key":   k,
						"value": v[k],
					},
				},
			}
		}
		return iters, nil
	default:
		return nil, fmt.Errorf("forEach.from %q is not a list or map (got %T)",
			spec.ForEach.From, src)
	}
}

// applyTemplate renders one template entry into the staging directory
// for the current iteration. writtenPaths records every staged
// destination so duplicates surface as an error rather than a silent
// overwrite. iterLabel is empty for artifacts without forEach.
func (r *ArtifactBuilder) applyTemplate(ctx context.Context,
	tmpl *mgapi.TemplateSpec,
	sources map[string]string,
	data map[string]any,
	stagingRoot *os.Root,
	writtenPaths map[string]string,
	iterLabel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	srcAlias, srcPath, err := parseSourceRef(tmpl.From)
	if err != nil {
		return fmt.Errorf("invalid template source: %w", err)
	}

	srcDir, ok := sources[srcAlias]
	if !ok {
		return fmt.Errorf("source alias %q not found", srcAlias)
	}

	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		return fmt.Errorf("failed to open source root %q: %w", srcDir, err)
	}
	defer srcRoot.Close()

	srcPath = filepath.Clean(srcPath)
	info, err := srcRoot.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source path %q does not exist in alias %q", srcPath, srcAlias)
		}
		return fmt.Errorf("failed to stat source %q: %w", srcPath, err)
	}
	if !info.Mode().IsRegular() {
		// Directory `from` is intentionally unsupported. The slice-7
		// answer for "fan a template across many files" is to load
		// the files via a pipeline step (e.g. `load` with a glob) and
		// then forEach over that step's output.
		return fmt.Errorf("source path %q is not a regular file", srcPath)
	}
	if info.Size() > maxTemplateFileBytes {
		return fmt.Errorf("template source %q is %d bytes which exceeds the per-file limit of %d bytes",
			srcPath, info.Size(), maxTemplateFileBytes)
	}

	destPath, err := r.renderDestPath(tmpl.To, data)
	if err != nil {
		return err
	}

	if prev, ok := writtenPaths[destPath]; ok {
		return fmt.Errorf("destination %q already written by template %q",
			destPath, prev)
	}

	rendered, err := r.renderTemplate(srcRoot, srcPath, tmpl.From, iterLabel, data)
	if err != nil {
		return err
	}

	if err := writeStagedFile(stagingRoot, destPath, rendered, info.Mode()); err != nil {
		return err
	}
	writtenPaths[destPath] = tmpl.From
	return nil
}

// renderDestPath template-expands the `@artifact/<path>` reference
// against the current iteration scope and returns the relative path
// inside the output tarball. The `@artifact/` prefix is fixed and
// stripped before rendering so a template fragment cannot accidentally
// produce or break the prefix.
func (r *ArtifactBuilder) renderDestPath(toRef string, data map[string]any) (string, error) {
	if !strings.HasPrefix(toRef, artifactRefPrefix) {
		return "", fmt.Errorf("invalid template destination %q: must start with %q",
			toRef, artifactRefPrefix)
	}
	pathTemplate := strings.TrimPrefix(toRef, artifactRefPrefix)
	if pathTemplate == "" {
		return "", fmt.Errorf("invalid template destination %q: empty path", toRef)
	}

	// Destination paths render with a zero Options: no partial
	// discovery (the path template lives in the spec, not in a source
	// file with sibling helpers) and `lookupFile` is unavailable
	// (paths must be derived from in-scope values, not read from disk).
	out, err := r.Engine.Render("destination:"+toRef, []byte(pathTemplate), data, render.Options{})
	if err != nil {
		return "", err
	}
	rendered := strings.TrimSpace(string(out))
	if rendered == "" {
		return "", fmt.Errorf("template destination %q rendered to empty path", toRef)
	}
	cleaned := filepath.Clean(rendered)
	if cleaned == "." || strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("template destination %q rendered to invalid path %q",
			toRef, rendered)
	}
	return cleaned, nil
}

// renderTemplate reads the source file, runs it through the engine,
// and returns the rendered bytes. The template name passed to the
// engine is the original `@alias/<path>` reference from the spec (with
// the current iteration label appended when forEach is in use) so
// parse / execute errors point straight back at the spec entry.
//
// The render is given a render.Options carrying:
//   - every `_helpers.tpl` discovered between the alias root and the
//     template's directory (root-first → template-closest order so a
//     closer file's `define` wins);
//   - a `lookupFile` closure jailed to srcRoot, so a template that
//     calls `lookupFile "rel/path"` reads through the same os.Root
//     and cannot escape its alias scope.
func (r *ArtifactBuilder) renderTemplate(srcRoot *os.Root,
	srcPath, tmplName, iterLabel string,
	data map[string]any) ([]byte, error) {
	f, err := srcRoot.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("open template source %q: %w", srcPath, err)
	}
	defer f.Close()

	// LimitReader with +1 so a file that grew between Stat and Read is
	// still caught (defense in depth alongside the Size() check).
	src, err := io.ReadAll(io.LimitReader(f, maxTemplateFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read template source %q: %w", srcPath, err)
	}
	if int64(len(src)) > maxTemplateFileBytes {
		return nil, fmt.Errorf("template source %q exceeded the per-file limit of %d bytes during read",
			srcPath, maxTemplateFileBytes)
	}

	partials, err := discoverPartials(srcRoot, srcPath)
	if err != nil {
		return nil, fmt.Errorf("discover partials for %q: %w", srcPath, err)
	}

	name := tmplName
	if iterLabel != "" {
		name = tmplName + " (" + iterLabel + ")"
	}
	out, err := r.Engine.Render(name, src, data, render.Options{
		Partials:   partials,
		LookupFile: newLookupFile(srcRoot),
	})
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return out, nil
}

// overlayData returns a shallow copy of base with the entries of
// overlay applied on top. Used to fold the per-iteration forEach
// binding into the existing template scope without mutating the
// caller's map.
func overlayData(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// parseSourceRef parses an `@<alias>/<path>` reference.
func parseSourceRef(ref string) (alias, path string, err error) {
	if !strings.HasPrefix(ref, "@") {
		return "", "", fmt.Errorf("source ref must start with '@'")
	}
	parts := strings.SplitN(ref[1:], "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("source ref must be '@<alias>/<path>'")
	}
	if parts[0] == "artifact" {
		return "", "", fmt.Errorf("'@artifact' is not valid as a template source")
	}
	return parts[0], parts[1], nil
}

// ValidateDestTemplate confirms ref begins with the `@artifact/`
// prefix and that the templated path portion parses as a Go template.
// Used by the controller's validator so a malformed `to:` stalls with
// ValidationFailedReason before any source fetch happens. Execution-
// time failures (missing keys, runtime template errors) still surface
// during the build.
func ValidateDestTemplate(ref string) error {
	if !strings.HasPrefix(ref, artifactRefPrefix) {
		return fmt.Errorf("must start with %q", artifactRefPrefix)
	}
	pathTemplate := strings.TrimPrefix(ref, artifactRefPrefix)
	if pathTemplate == "" {
		return fmt.Errorf("empty path")
	}
	return parseDestTemplate(pathTemplate)
}

// writeStagedFile writes rendered to dstPath inside the staging root,
// creating any intermediate directories. The destination file inherits
// the source file's permission bits so executable templates round-trip
// their mode through the tarball.
func writeStagedFile(dstRoot *os.Root, dstPath string, rendered []byte, mode os.FileMode) error {
	if dir := filepath.Dir(dstPath); dir != "." && dir != "" {
		if err := mkdirAll(dstRoot, dir); err != nil {
			return fmt.Errorf("create destination directory %q: %w", dir, err)
		}
	}
	dst, err := dstRoot.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create destination %q: %w", dstPath, err)
	}
	defer dst.Close()
	if _, err := dst.Write(rendered); err != nil {
		return fmt.Errorf("write destination %q: %w", dstPath, err)
	}
	return dst.Chmod(mode)
}

func mkdirAll(root *os.Root, path string) error {
	if path == "." || path == "" {
		return nil
	}
	if err := root.Mkdir(path, 0o755); err == nil {
		return nil
	} else if os.IsExist(err) {
		return nil
	}
	parent := filepath.Dir(path)
	if parent != path {
		if err := mkdirAll(root, parent); err != nil {
			return err
		}
	}
	if err := root.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// MkdirTempAbs creates a temp dir and returns its absolute path with
// any symlinks resolved, matching source-watcher's helper.
func MkdirTempAbs(dir, pattern string) (string, error) {
	tmpDir, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	tmpDir, err = filepath.EvalSymlinks(tmpDir)
	if err != nil {
		return "", fmt.Errorf("error evaluating symlink: %w", err)
	}
	return tmpDir, nil
}
