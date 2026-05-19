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

func TestMap_ListProjectsScalar(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/cluster.yaml": "name: a\n",
			"b/cluster.yaml": "name: b\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "clusters", Load: &mgapi.LoadStep{From: "@repo/*/cluster.yaml"}},
		{Name: "names", Map: &mgapi.MapStep{
			From: "clusters",
			Expr: `{{ .value.name }}`,
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["names"]).To(Equal([]any{"a", "b"}))
}

func TestMap_ListProjectsStructured(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/cluster.yaml": "name: a\ncore: c1\n",
			"b/cluster.yaml": "name: b\ncore: c1\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "clusters", Load: &mgapi.LoadStep{From: "@repo/*/cluster.yaml"}},
		{Name: "renamed", Map: &mgapi.MapStep{
			From: "clusters",
			Expr: "id: {{ .value.name }}\ngroup: {{ .value.core }}\n",
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["renamed"]).To(Equal([]any{
		map[string]any{"id": "a", "group": "c1"},
		map[string]any{"id": "b", "group": "c1"},
	}))
}

func TestMap_MapPreservesKeys(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/cluster.yaml": "name: a\n",
			"b/cluster.yaml": "name: b\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "byName", Load: &mgapi.LoadStep{
			From:    "@repo/*/cluster.yaml",
			As:      mgapi.LoadAsMap,
			KeyExpr: "{{ .path | dir | base }}",
		}},
		{Name: "tagged", Map: &mgapi.MapStep{
			From: "byName",
			Expr: "name: {{ .value.name }}\nkey: {{ .key }}\n",
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["tagged"]).To(Equal(map[string]any{
		"a": map[string]any{"name": "a", "key": "a"},
		"b": map[string]any{"name": "b", "key": "b"},
	}))
}

func TestMap_InvalidYAMLOutput(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"a/x.yaml": "v: 1\n"},
	})
	steps := []mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*/x.yaml"}},
		{Name: "bad", Map: &mgapi.MapStep{
			From: "list",
			Expr: "name: a\n  bad: : :\n",
		}},
	}
	_, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("decode rendered expr as yaml")))
}

func TestMap_DoesNotMutateInput(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/x.yaml": "name: a\n",
			"b/x.yaml": "name: b\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*/x.yaml"}},
		{Name: "names", Map: &mgapi.MapStep{From: "list", Expr: "{{ .value.name }}"}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["list"]).To(Equal([]any{
		map[string]any{"name": "a"},
		map[string]any{"name": "b"},
	}))
	g.Expect(outs["names"]).To(Equal([]any{"a", "b"}))
}

func TestMap_RejectsForwardFrom(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "bad", Map: &mgapi.MapStep{From: "later", Expr: "{{ .value }}"}},
		{Name: "later", Load: &mgapi.LoadStep{From: "@repo/x.yaml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("does not reference a prior step")))
}

func TestMap_BadExprTemplateFailsCompile(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*.yaml"}},
		{Name: "bad", Map: &mgapi.MapStep{From: "list", Expr: "{{ .value"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("expr")))
}
