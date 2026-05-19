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

func TestFilter_ListKeepsTruthy(t *testing.T) {
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
		{Name: "core1", Filter: &mgapi.FilterStep{
			From:  "clusters",
			Where: `{{ eq .value.core "core-1" }}`,
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["core1"]).To(Equal([]any{
		map[string]any{"name": "a", "core": "core-1"},
		map[string]any{"name": "c", "core": "core-1"},
	}))
}

func TestFilter_MapKeepsTruthy(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/cluster.yaml": "core: core-1\n",
			"b/cluster.yaml": "core: core-2\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "byName", Load: &mgapi.LoadStep{
			From:    "@repo/*/cluster.yaml",
			As:      mgapi.LoadAsMap,
			KeyExpr: "{{ .path | dir | base }}",
		}},
		{Name: "core1ByName", Filter: &mgapi.FilterStep{
			From:  "byName",
			Where: `{{ eq .value.core "core-1" }}`,
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["core1ByName"]).To(Equal(map[string]any{
		"a": map[string]any{"core": "core-1"},
	}))
}

func TestFilter_EmptyOutputIsFalse(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"a/x.yaml": "v: 1\n",
			"b/x.yaml": "v: 2\n",
		},
	})
	// An empty rendered string is treated as false, so this filter drops
	// every item without erroring.
	steps := []mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*/x.yaml"}},
		{Name: "none", Filter: &mgapi.FilterStep{
			From:  "list",
			Where: `{{ if false }}true{{ end }}`,
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["none"]).To(Equal([]any{}))
}

func TestFilter_NonBooleanRenderRejected(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"a/x.yaml": "v: 1\n"},
	})
	steps := []mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*/x.yaml"}},
		{Name: "bad", Filter: &mgapi.FilterStep{
			From:  "list",
			Where: `nope`,
		}},
	}
	_, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("did not produce a boolean")))
}

func TestFilter_CanReferencePriorOutputs(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {
			"settings.yaml":  "core: core-1\n",
			"a/cluster.yaml": "name: a\ncore: core-1\n",
			"b/cluster.yaml": "name: b\ncore: core-2\n",
		},
	})
	steps := []mgapi.PipelineStep{
		{Name: "settings", Load: &mgapi.LoadStep{From: "@repo/settings.yaml"}},
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*/cluster.yaml"}},
		{Name: "matching", Filter: &mgapi.FilterStep{
			From:  "list",
			Where: `{{ eq .value.core .settings.core }}`,
		}},
	}
	outs, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(outs["matching"]).To(HaveLen(1))
}

func TestFilter_RejectsForwardFrom(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "bad", Filter: &mgapi.FilterStep{From: "later", Where: "{{ true }}"}},
		{Name: "later", Load: &mgapi.LoadStep{From: "@repo/x.yaml"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("does not reference a prior step")))
}

func TestFilter_RejectsSelfFrom(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "x", Filter: &mgapi.FilterStep{From: "x", Where: "{{ true }}"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("does not reference a prior step")))
}

func TestFilter_RejectsNonCollectionInput(t *testing.T) {
	g := NewWithT(t)
	sources := writeTree(t, map[string]map[string]string{
		"repo": {"single.txt": "hello\n"},
	})
	steps := []mgapi.PipelineStep{
		{Name: "single", Load: &mgapi.LoadStep{
			From: "@repo/single.txt", Format: mgapi.LoadFormatText,
		}},
		{Name: "bad", Filter: &mgapi.FilterStep{From: "single", Where: "{{ true }}"}},
	}
	_, err := run(t, steps, map[string]bool{"repo": true}, sources)
	g.Expect(err).To(MatchError(ContainSubstring("not a list or map")))
}

func TestFilter_BadWhereTemplateFailsCompile(t *testing.T) {
	g := NewWithT(t)
	_, err := Compile([]mgapi.PipelineStep{
		{Name: "list", Load: &mgapi.LoadStep{From: "@repo/*.yaml"}},
		{Name: "bad", Filter: &mgapi.FilterStep{From: "list", Where: "{{ .value"}},
	}, map[string]bool{"repo": true})
	g.Expect(err).To(MatchError(ContainSubstring("where")))
}
