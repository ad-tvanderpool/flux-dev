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
// In slice 3 the builder is a pure pass-through copy: each
// `templates[].from` is treated literally as a path inside the named
// source artifact and copied to `templates[].to` inside the resulting
// tarball, with no templating or pipeline evaluation. Later slices
// will replace this with a real render pipeline.
package builder

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkstorage "github.com/fluxcd/pkg/artifact/storage"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// ArtifactBuilder turns a ManifestArtifact spec plus a set of fetched
// source-controller artifact directories into a stored tarball.
type ArtifactBuilder struct {
	Storage *gotkstorage.Storage
}

// New creates a new ArtifactBuilder writing to the given storage.
func New(storage *gotkstorage.Storage) *ArtifactBuilder {
	return &ArtifactBuilder{Storage: storage}
}

// Build assembles the artifact for one ManifestArtifact entry. It
// stages files into a per-artifact subdirectory of workspace, packs
// them into the storage backend, and returns the resulting artifact
// metadata.
//
// Slice 3 semantics: each template is interpreted as a literal copy
// from `@<alias>/<path>` to `@artifact/<path>`. Pipeline evaluation,
// values, valuesFrom, and forEach are intentionally rejected by the
// validation layer for now and will be added in subsequent slices.
func (r *ArtifactBuilder) Build(ctx context.Context,
	spec *mgapi.ManifestArtifact,
	sources map[string]string,
	namespace string,
	workspace string) (*gotkmeta.Artifact, error) {
	stagingDir := filepath.Join(workspace, spec.Name)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create staging dir: %w", err)
	}

	if err := applyTemplates(ctx, spec.Templates, sources, stagingDir); err != nil {
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

// applyTemplates resolves each TemplateSpec as a literal copy of
// `<source-dir>/<from-path>` into `<stagingDir>/<to-path>`.
func applyTemplates(ctx context.Context,
	templates []mgapi.TemplateSpec,
	sources map[string]string,
	stagingDir string) error {
	for _, t := range templates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := applyTemplate(ctx, t, sources, stagingDir); err != nil {
			return fmt.Errorf("template %q -> %q: %w", t.From, t.To, err)
		}
	}
	return nil
}

func applyTemplate(ctx context.Context,
	tmpl mgapi.TemplateSpec,
	sources map[string]string,
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

	if info.IsDir() {
		return copyDir(ctx, srcRoot, srcPath, stagingRoot, destPath)
	}
	return copyFile(ctx, srcRoot, srcPath, stagingRoot, destPath)
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

func copyDir(ctx context.Context,
	srcRoot *os.Root,
	srcPath string,
	dstRoot *os.Root,
	dstPath string) error {
	return fs.WalkDir(srcRoot.FS(), srcPath, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcPath, path)
		if err != nil {
			return err
		}
		target := dstPath
		if rel != "." {
			target = filepath.Join(dstPath, rel)
		}
		if d.IsDir() {
			return mkdirAll(dstRoot, target)
		}
		return copyFile(ctx, srcRoot, path, dstRoot, target)
	})
}

func copyFile(ctx context.Context,
	srcRoot *os.Root,
	srcPath string,
	dstRoot *os.Root,
	dstPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir := filepath.Dir(dstPath); dir != "." && dir != "" {
		if err := mkdirAll(dstRoot, dir); err != nil {
			return fmt.Errorf("failed to create destination directory %q: %w", dir, err)
		}
	}
	src, err := srcRoot.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source %q: %w", srcPath, err)
	}
	defer src.Close()
	dst, err := dstRoot.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create destination %q: %w", dstPath, err)
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(src); err != nil {
		return fmt.Errorf("copy %q -> %q: %w", srcPath, dstPath, err)
	}
	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source %q: %w", srcPath, err)
	}
	return dst.Chmod(info.Mode())
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
