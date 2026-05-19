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
// In slice 6 the builder is a single-file template renderer: each
// `templates[].from` is interpreted as a literal path inside the named
// source artifact, read into memory, rendered through the supplied
// render.Engine, and written verbatim to `templates[].to` inside the
// staged tarball. Pipeline outputs are placed at the top level of the
// template scope so a template can read them as `.<stepName>`.
//
// Directory references in `from` are intentionally rejected — once
// rendering is in play, a directory copy has no well-defined semantics
// (which template scope, which output names?). Slice 7's `forEach` is
// the right answer when an author needs to fan a single template over
// a collection.
package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// slices will fold inline values, valuesFrom, and per-`forEach`
// bindings into the same map before calling Build.
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

	if err := r.applyTemplates(ctx, spec.Templates, sources, data, stagingDir); err != nil {
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

// applyTemplates resolves each TemplateSpec to a regular file in the
// named source artifact, renders it through the engine, and writes the
// rendered bytes to the staging directory.
func (r *ArtifactBuilder) applyTemplates(ctx context.Context,
	templates []mgapi.TemplateSpec,
	sources map[string]string,
	data map[string]any,
	stagingDir string) error {
	for _, t := range templates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.applyTemplate(ctx, t, sources, data, stagingDir); err != nil {
			return fmt.Errorf("template %q -> %q: %w", t.From, t.To, err)
		}
	}
	return nil
}

func (r *ArtifactBuilder) applyTemplate(ctx context.Context,
	tmpl mgapi.TemplateSpec,
	sources map[string]string,
	data map[string]any,
	stagingDir string) error {
	srcAlias, srcPath, err := parseSourceRef(tmpl.From)
	if err != nil {
		return fmt.Errorf("invalid template source: %w", err)
	}
	destPath, err := parseArtifactRef(tmpl.To)
	if err != nil {
		return fmt.Errorf("invalid template destination: %w", err)
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

	stagingRoot, err := os.OpenRoot(stagingDir)
	if err != nil {
		return fmt.Errorf("failed to open staging root %q: %w", stagingDir, err)
	}
	defer stagingRoot.Close()

	srcPath = filepath.Clean(srcPath)
	info, err := srcRoot.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source path %q does not exist in alias %q", srcPath, srcAlias)
		}
		return fmt.Errorf("failed to stat source %q: %w", srcPath, err)
	}
	if !info.Mode().IsRegular() {
		// Directory `from` is intentionally unsupported in slice 6 — see
		// the package comment. Once `forEach` lands in slice 7, the
		// path-templated `to:` is how authors fan a template across a
		// collection.
		return fmt.Errorf("source path %q is not a regular file", srcPath)
	}
	if info.Size() > maxTemplateFileBytes {
		return fmt.Errorf("template source %q is %d bytes which exceeds the per-file limit of %d bytes",
			srcPath, info.Size(), maxTemplateFileBytes)
	}

	rendered, err := r.renderTemplate(srcRoot, srcPath, tmpl.From, data)
	if err != nil {
		return err
	}

	return writeStagedFile(stagingRoot, destPath, rendered, info.Mode())
}

// renderTemplate reads the source file, runs it through the engine,
// and returns the rendered bytes. The template name passed to the
// engine is the original `@alias/<path>` reference from the spec so
// parse / execute errors point straight back at the spec entry.
func (r *ArtifactBuilder) renderTemplate(srcRoot *os.Root,
	srcPath, tmplName string,
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

	out, err := r.Engine.Render(tmplName, src, data)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return out, nil
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

// parseArtifactRef parses an `@artifact/<path>` reference and returns
// the relative path inside the output tarball.
func parseArtifactRef(ref string) (string, error) {
	const prefix = "@artifact/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("destination must start with %q", prefix)
	}
	rel := strings.TrimPrefix(ref, prefix)
	if rel == "" {
		return "", fmt.Errorf("destination path is empty")
	}
	return rel, nil
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
