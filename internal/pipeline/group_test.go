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

func TestGroup_BucketsListByKeyExpr(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/cluster.yaml": "name: a\ncore: core-1\n",
			"b/cluster.yaml": "name: b\ncore: core-2\n",
			"c/cluster.yaml": "name: c\ncore: core-1\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "clusters", Load: &mgapi.LoadStep{From: "@repo/*/cluster.yaml"}},
		{Name: "byCore", Group: &mgapi.GroupStep{
			From:    "clusters",
			KeyExpr: "{{ .value.core }}",
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["byCore"]).To(Equal(map[string]any{
		"core-1": []any{
			map[string]any{"name": "a", "core": "core-1"},
			map[string]any{"name": "c", "core": "core-1"},
		},
		"core-2": []any{
			map[string]any{"name": "b", "core": "core-2"},
		},
	}))
}

func TestGroup_PreservesInputOrderInBuckets(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"01.yaml": "core: c\nidx: 1\n",
			"02.yaml": "core: c\nidx: 2\n",
			"03.yaml": "core: c\nidx: 3\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "all", Load: &mgapi.LoadStep{From: "@repo/*.yaml"}},
		{Name: "grouped", Group: &mgapi.GroupStep{
			From:    "all",
			KeyExpr: "{{ .value.core }}",
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	bucket, _ := outs["grouped"].(map[string]any)["c"].([]any)
	g.Expect(bucket).To(HaveLen(3))
	g.Expect(bucket[0]).To(HaveKeyWithValue("idx", float64(1)))
	g.Expect(bucket[1]).To(HaveKeyWithValue("idx", float64(2)))
	g.Expect(bucket[2]).To(HaveKeyWithValue("idx", float64(3)))
}

func TestGroup_RejectsMapInput(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"a/x.yaml": "v: 1\n"},
	})
	steps := []mgapi.PipelineStep{
		{Name: "m", Load: &mgapi.LoadStep{
			From:    "@repo/*/x.yaml",
			As:      mgapi.LoadAsMap,
			KeyExpr: "{{ .path }}",
		}},
		{Name: "bad", Group: &mgapi.GroupStep{From: "m", KeyExpr: "{{ .value.v }}"}},
	}
	_, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("not a list")))
}

func TestGroup_EmptyKeyExprIsRejected(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"a/x.yaml": "v: 1\n"},
	})
	steps := []mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*/x.yaml"}},
		{Name: "bad", Group: &mgapi.GroupStep{
			From:    "list",
			KeyExpr: `{{ "   " }}`,
		}},
	}
	_, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("empty key")))
}

func TestGroup_RejectsForwardFrom(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "bad", Group: &mgapi.GroupStep{From: "later", KeyExpr: "{{ .value }}"}},
		{Name: "later", Load: &mgapi.LoadStep{From: "@repo/x.yaml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("does not reference a prior step")))
}
