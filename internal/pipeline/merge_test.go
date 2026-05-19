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
	"testing"

	. "github.com/onsi/gomega"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

func TestMerge_DeepMergesMaps(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"base.yaml":     "image:\n  repo: nginx\n  tag: v1\nreplicas: 1\n",
			"override.yaml": "image:\n  tag: v2\nresources:\n  cpu: 100m\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "base", Load: &mgapi.LoadStep{From: "@repo/base.yaml"}},
		{Name: "override", Load: &mgapi.LoadStep{From: "@repo/override.yaml"}},
		{Name: "merged", Merge: &mgapi.MergeStep{From: []string{"base", "override"}}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["merged"]).To(Equal(map[string]any{
		"image": map[string]any{
			"repo": "nginx",
			"tag":  "v2",
		},
		"replicas": float64(1),
		"resources": map[string]any{
			"cpu": "100m",
		},
	}))
}

func TestMerge_LaterWinsOnTypeMismatch(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a.yaml": "x:\n  nested: true\n",
			"b.yaml": "x: replaced\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "a", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
		{Name: "b", Load: &mgapi.LoadStep{From: "@repo/b.yaml"}},
		{Name: "out", Merge: &mgapi.MergeStep{From: []string{"a", "b"}}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["out"]).To(Equal(map[string]any{"x": "replaced"}))
}

func TestMerge_ListsAreReplacedNotConcatenated(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a.yaml": "tags:\n  - one\n  - two\n",
			"b.yaml": "tags:\n  - three\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "a", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
		{Name: "b", Load: &mgapi.LoadStep{From: "@repo/b.yaml"}},
		{Name: "out", Merge: &mgapi.MergeStep{From: []string{"a", "b"}}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["out"]).To(Equal(map[string]any{
		"tags": []any{"three"},
	}))
}

func TestMerge_DoesNotMutateInputs(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a.yaml": "image:\n  tag: v1\n",
			"b.yaml": "image:\n  tag: v2\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "a", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
		{Name: "b", Load: &mgapi.LoadStep{From: "@repo/b.yaml"}},
		{Name: "out", Merge: &mgapi.MergeStep{From: []string{"a", "b"}}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["a"]).To(Equal(map[string]any{"image": map[string]any{"tag": "v1"}}))
	g.Expect(outs["b"]).To(Equal(map[string]any{"image": map[string]any{"tag": "v2"}}))
}

func TestMerge_NonMapInputRejected(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"obj.yaml":  "k: v\n",
			"list.yaml": "- a\n- b\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "obj", Load: &mgapi.LoadStep{From: "@repo/obj.yaml"}},
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/list.yaml"}},
		{Name: "out", Merge: &mgapi.MergeStep{From: []string{"obj", "list"}}},
	}
	_, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("not a map")))
}

func TestMerge_RejectsForwardFrom(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "a", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
		{Name: "bad", Merge: &mgapi.MergeStep{From: []string{"a", "later"}}},
		{Name: "later", Load: &mgapi.LoadStep{From: "@repo/b.yaml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("does not reference a prior step")))
}

func TestMerge_RejectsDuplicateFromEntry(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "a", Load: &mgapi.LoadStep{From: "@repo/a.yaml"}},
		{Name: "b", Load: &mgapi.LoadStep{From: "@repo/b.yaml"}},
		{Name: "bad", Merge: &mgapi.MergeStep{From: []string{"a", "a"}}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("more than once")))
}
