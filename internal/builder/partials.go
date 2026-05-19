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
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tvanderpool/flux-manifest-generator/internal/render"
)

// partialFilename is the well-known basename the builder discovers
// between the alias root and the template's directory. Spec authors
// drop named templates into one of these files; everything else in
// the artifact tree is left alone. The single-filename rule matches
// the design's "_helpers.tpl siblings" wording (DESIGN.md §7) and
// keeps the discovery walk cheap and predictable.
const partialFilename = "_helpers.tpl"

// maxPartialFileBytes caps an individual partial source file. Mirrors
// the per-file ceiling on pipeline `load` and template `from:` reads
// so a hostile or accidentally huge `_helpers.tpl` can't OOM the
// controller.
const maxPartialFileBytes int64 = 10 << 20 // 10 MiB

// maxLookupFileBytes caps an individual `lookupFile` read. Same
// per-file ceiling as partials and template sources so the entire
// file-reading surface of the render path has a single bound.
const maxLookupFileBytes int64 = 10 << 20 // 10 MiB

// discoverPartials walks from the alias root down toward the
// template's directory, collecting every `_helpers.tpl` file along
// the way. Results are returned in root-first → template-closest
// order so when the render engine parses them into the template tree
// the closer file's `{{ define }}` blocks override any same-named
// blocks from a parent directory (text/template's last-define-wins
// rule).
//
// Walking through parent directories — rather than recursively
// scanning the alias root — keeps discovery proportional to the
// template's depth and gives spec authors a Helm-like layering rule
// without forcing every artifact into a single flat templates/ dir.
//
// srcRoot is jailed to the alias root by callers (os.OpenRoot), so
// the cleaned relative paths constructed here are safe to Stat / Open
// against it.
func discoverPartials(srcRoot *os.Root, srcPath string) ([]render.Partial, error) {
	// path.Dir gives an empty string for paths in the root; normalise
	// to "." here so the chain construction below treats the root as
	// a real (empty-relative) directory.
	dir := path.Dir(filepath.ToSlash(srcPath))
	if dir == "." {
		dir = ""
	}

	chain := partialChainDirs(dir)

	var partials []render.Partial
	for _, d := range chain {
		rel := partialFilename
		if d != "" {
			rel = d + "/" + partialFilename
		}
		// Use filepath separators for the actual filesystem call;
		// os.Root works with native paths.
		fsRel := filepath.FromSlash(rel)

		info, err := srcRoot.Stat(fsRel)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			// Symlinks / dirs named _helpers.tpl are ignored on
			// purpose: skipping them is safer than treating them as
			// partials, and discovery should never fail because the
			// tree happens to contain an odd entry.
			continue
		}
		if info.Size() > maxPartialFileBytes {
			return nil, fmt.Errorf("partial %q is %d bytes which exceeds the per-file limit of %d bytes",
				rel, info.Size(), maxPartialFileBytes)
		}

		body, err := readSmallFile(srcRoot, fsRel, maxPartialFileBytes)
		if err != nil {
			return nil, fmt.Errorf("read partial %q: %w", rel, err)
		}
		partials = append(partials, render.Partial{Name: rel, Src: body})
	}
	return partials, nil
}

// partialChainDirs returns the list of directories from the alias
// root ("") down to dir, inclusive, in walk order. Used by
// discoverPartials to enumerate where a `_helpers.tpl` may sit; given
// "a/b/c" the result is ["", "a", "a/b", "a/b/c"].
func partialChainDirs(dir string) []string {
	if dir == "" {
		return []string{""}
	}
	parts := strings.Split(dir, "/")
	out := make([]string, 0, len(parts)+1)
	out = append(out, "")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		out = append(out, cur)
	}
	return out
}

// newLookupFile returns the `lookupFile` template-helper backing
// closure for one template render. The closure is jailed to srcRoot
// (the template's alias root): absolute paths and parent-segment
// escapes are rejected before any filesystem call, and the os.Root
// itself confines the read to the alias subtree as defence in depth.
// Reads are size-capped at maxLookupFileBytes so a template that
// asks for a hostile or accidentally huge file fails with a clear
// error rather than ballooning controller memory.
func newLookupFile(srcRoot *os.Root) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		cleaned, err := cleanLookupPath(p)
		if err != nil {
			return nil, err
		}
		info, err := srcRoot.Stat(cleaned)
		if err != nil {
			return nil, fmt.Errorf("stat: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%q is not a regular file", p)
		}
		if info.Size() > maxLookupFileBytes {
			return nil, fmt.Errorf("%q is %d bytes which exceeds the per-file limit of %d bytes",
				p, info.Size(), maxLookupFileBytes)
		}
		return readSmallFile(srcRoot, cleaned, maxLookupFileBytes)
	}
}

// cleanLookupPath normalises a `lookupFile` argument and rejects the
// forms that would escape the alias root: empty paths, absolute
// paths, and any path that resolves to (or to a descendant of) the
// parent directory. The os.Root in newLookupFile catches anything
// that slips through, but rejecting early gives the spec author a
// clearer error message tied to their argument verbatim.
func cleanLookupPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("lookupFile path is empty")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("lookupFile path %q must be relative", p)
	}
	cleaned := filepath.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("lookupFile path %q escapes the alias root", p)
	}
	return cleaned, nil
}

// readSmallFile reads up to maxBytes from rel under root, returning
// an error if the file grew past the cap between Stat and Read (the
// LimitReader+1 trick). Used by both partial discovery and the
// `lookupFile` helper so they share the same bounded-read shape.
func readSmallFile(root *os.Root, rel string, maxBytes int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("file exceeded the per-file limit of %d bytes during read", maxBytes)
	}
	return b, nil
}
