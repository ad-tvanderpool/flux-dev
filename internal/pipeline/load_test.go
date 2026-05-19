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
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// writeTree materialises the alias→relative-path→body map under a temp
// directory and returns the alias→source-dir map ready to feed into
// Evaluator.Run.
func writeTree(t *testing.T, files map[string]map[string]string) map[string]string {
	t.Helper()
	root := t.TempDir()
	dirs := make(map[string]string, len(files))
	for alias, entries := range files {
		aliasDir := filepath.Join(root, alias)
		if err := os.MkdirAll(aliasDir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", aliasDir, err)
		}
		for rel, body := range entries {
			full := filepath.Join(aliasDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatalf("write %q: %v", full, err)
			}
		}
		dirs[alias] = aliasDir
	}
	return dirs
}

func run(t *testing.T, steps []mgapi.PipelineStep, aliases map[string]bool, sources map[string]string) (Outputs, error) {
	t.Helper()
	ev, err := Compile(steps, aliases)
	if err != nil {
		return nil, err
	}
	return ev.Run(context.Background(), sources, nil)
}

func TestCompile_RejectsDuplicateStepNames(t *testing.T) {
	g := NewWithT(t)
	steps := []mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
		{Name: "x", Load: &mgapi.LoadStep{From: "@repo/b.yaml"}},
	}
	_, err := Compile(steps, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("duplicate step name")))
}

func TestCompile_RejectsUnknownAlias(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{From: "@ghost/a.yaml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("source alias \"ghost\" not declared")))
}

func TestCompile_RejectsMalformedFromRef(t *testing.T) {
	g := NewWithT(t)
	for _, bad := range []string{"repo/a", "@/a", "@repo/", "@repo"} {
		_, err := Compile([]mgapi.PipelineStep{
			{Name: "x", Load: &mgapi.LoadStep{From: bad}},
		}, map[string]bool{"repo": true})
		g.Expect(err).To(HaveOccurred(), "expected %q to be rejected", bad)
	}
}

func TestCompile_RejectsParentSegments(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{From: "@repo/../etc/passwd"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("'..'")))
}

func TestCompile_RejectsUnsupportedFormat(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{From: "@repo/a.yaml", Format: "xml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("unsupported format")))
}

func TestCompile_AsMapRequiresKeyExpr(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{From: "@repo/*.yaml", As: mgapi.LoadAsMap}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("requires `keyExpr`")))
}

func TestCompile_KeyExprWithoutAsMapIsRejected(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{
			From: "@repo/*.yaml", As: mgapi.LoadAsList, KeyExpr: "{{ .path }}",
		}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("only valid with `as: map`")))
}

func TestCompile_KeyExprMustParse(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{
			From: "@repo/*.yaml", As: mgapi.LoadAsMap, KeyExpr: "{{ .path",
		}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("keyExpr")))
}

func TestLoad_SingleYAML(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"cluster.yaml": "name: campus-a\ncore: core-1\n"},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "cluster", Load: &mgapi.LoadStep{From: "@repo/cluster.yaml"}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["cluster"]).To(Equal(map[string]any{"name": "campus-a", "core": "core-1"}))
}

func TestLoad_SingleJSON(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"cluster.json": `{"name":"campus-a","replicas":3}`},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "cluster", Load: &mgapi.LoadStep{
			From: "@repo/cluster.json", Format: mgapi.LoadFormatJSON,
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["cluster"]).To(Equal(map[string]any{
		"name": "campus-a", "replicas": float64(3),
	}))
}

func TestLoad_SingleText(t *testing.T) {
	g := NewWithT(t)
	body := "# hello\n"
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"NOTES.md": body},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "notes", Load: &mgapi.LoadStep{
			From: "@repo/NOTES.md", Format: mgapi.LoadFormatText,
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["notes"]).To(Equal(body))
}

func TestLoad_SingleRaw(t *testing.T) {
	g := NewWithT(t)
	body := "binary\x00bytes\n"
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"blob.bin": body},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "blob", Load: &mgapi.LoadStep{
			From: "@repo/blob.bin", Format: mgapi.LoadFormatRaw,
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["blob"]).To(Equal([]byte(body)))
}

func TestLoad_GlobAsList(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"clusters/campus/a/talos/cluster.yaml": "name: a\ncore: c1\n",
			"clusters/campus/b/talos/cluster.yaml": "name: b\ncore: c1\n",
			"clusters/campus/c/talos/cluster.yaml": "name: c\ncore: c2\n",
			"unrelated/file.yaml":                  "ignore: true\n",
		},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "campusClusters", Load: &mgapi.LoadStep{
			From: "@repo/clusters/campus/*/talos/cluster.yaml",
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["campusClusters"]).To(HaveLen(3))
	list, _ := outs["campusClusters"].([]any)
	g.Expect(list[0]).To(Equal(map[string]any{"name": "a", "core": "c1"}))
	g.Expect(list[1]).To(Equal(map[string]any{"name": "b", "core": "c1"}))
	g.Expect(list[2]).To(Equal(map[string]any{"name": "c", "core": "c2"}))
}

func TestLoad_GlobAsMapWithKeyExpr(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"clusters/campus/a/talos/cluster.yaml": "name: a\n",
			"clusters/campus/b/talos/cluster.yaml": "name: b\n",
		},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "byName", Load: &mgapi.LoadStep{
			From:    "@repo/clusters/campus/*/talos/cluster.yaml",
			As:      mgapi.LoadAsMap,
			KeyExpr: "{{ .path | dir | dir | base }}",
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["byName"]).To(Equal(map[string]any{
		"a": map[string]any{"name": "a"},
		"b": map[string]any{"name": "b"},
	}))
}

func TestLoad_DoubleStarGlob(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/x.yaml":   "v: 1\n",
			"a/b/y.yaml": "v: 2\n",
			"a/b/c/z.md": "ignored\n",
		},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "all", Load: &mgapi.LoadStep{
			From: "@repo/a/**/*.yaml",
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["all"]).To(HaveLen(2))
}

func TestLoad_GlobNoMatches(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"only.txt": "nothing yaml here\n"},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{
			From: "@repo/clusters/*.yaml",
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["x"]).To(Equal([]any{}))
}

func TestLoad_GlobNoMatchesAsMap(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"only.txt": "ignored\n"},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{
			From: "@repo/none/*.yaml", As: mgapi.LoadAsMap, KeyExpr: "{{ .path }}",
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["x"]).To(Equal(map[string]any{}))
}

func TestLoad_SingleFileMissing(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"other.yaml": "hi: there\n"},
	})
	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{From: "@repo/ghost.yaml"}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("source path")))
}

func TestLoad_RejectsDirectoryAsSingleFile(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"sub/inner.yaml": "x: y\n"},
	})
	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{From: "@repo/sub"}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("not a regular file")))
}

func TestLoad_InvalidYAML(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"bad.yaml": "name: a\n  bad: : :\n"},
	})
	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{From: "@repo/bad.yaml"}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("decode")))
}

func TestLoad_InvalidJSON(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"bad.json": "{ not json"},
	})
	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{
			From: "@repo/bad.json", Format: mgapi.LoadFormatJSON,
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("decode")))
}

func TestLoad_KeyExprDuplicateKey(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/cluster.yaml": "name: a\n",
			"b/cluster.yaml": "name: b\n",
		},
	})
	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "byName", Load: &mgapi.LoadStep{
			From:    "@repo/*/cluster.yaml",
			As:      mgapi.LoadAsMap,
			KeyExpr: `{{ "same" }}`,
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("duplicate key")))
}

func TestLoad_KeyExprEmptyKey(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"a/cluster.yaml": "name: a\n"},
	})
	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{
			From:    "@repo/*/cluster.yaml",
			As:      mgapi.LoadAsMap,
			KeyExpr: `{{ "   " }}`,
		}}},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("empty key")))
}

func TestLoad_KeyExprCanReferenceEarlierOutput(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"prefix.yaml":    "prefix: cluster\n",
			"a/cluster.yaml": "name: a\n",
		},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{
			{Name: "meta", Load: &mgapi.LoadStep{From: "@repo/prefix.yaml"}},
			{Name: "byName", Load: &mgapi.LoadStep{
				From:    "@repo/*/cluster.yaml",
				As:      mgapi.LoadAsMap,
				KeyExpr: `{{ .meta.prefix }}-{{ .path | dir | base }}`,
			}},
		},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["byName"]).To(HaveKey("cluster-a"))
}

func TestLoad_RejectsFileOverSizeLimit(t *testing.T) {
	g := NewWithT(t)
	root := t.TempDir()
	aliasDir := filepath.Join(root, "repo")
	g.Expect(os.MkdirAll(aliasDir, 0o755)).To(Succeed())
	big := filepath.Join(aliasDir, "big.txt")
	g.Expect(os.WriteFile(big, []byte(strings.Repeat("a", int(maxLoadFileBytes)+1)), 0o644)).To(Succeed())

	_, err := run(t,
		[]mgapi.PipelineStep{{Name: "x", Load: &mgapi.LoadStep{
			From: "@repo/big.txt", Format: mgapi.LoadFormatText,
		}}},
		map[string]bool{"repo": true}, map[string]string{"repo": aliasDir})
	g.Expect(err).To(MatchError(ContainSubstring("exceeds the per-file load limit")))
}

func TestLoad_RunReturnsAllStepOutputs(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"one.yaml": "k: 1\n",
			"two.yaml": "k: 2\n",
		},
	})
	outs, err := run(t,
		[]mgapi.PipelineStep{
			{Name: "first", Load: &mgapi.LoadStep{From: "@repo/one.yaml"}},
			{Name: "second", Load: &mgapi.LoadStep{From: "@repo/two.yaml"}},
		},
		map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs).To(HaveKey("first"))
	g.Expect(outs).To(HaveKey("second"))
}

func TestLoad_MissingSourceAtRuntime(t *testing.T) {
	g := NewWithT(t)
	ev, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).ToNot(HaveOccurred())

	_, err = ev.Run(context.Background(), map[string]string{}, nil)
	g.Expect(err).To(MatchError(ContainSubstring("no fetched artifact")))
}
