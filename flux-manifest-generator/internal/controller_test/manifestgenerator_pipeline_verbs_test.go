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

package controller_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	gotktestsrv "github.com/fluxcd/pkg/testserver"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// TestManifestGenerator_PipelineVerbs is the slice-5 integration test:
// a ManifestGenerator whose spec.pipeline exercises every verb in v1
// (load, filter, map, group, merge) compiles cleanly, reconciles to
// Ready=True, and an invalid downstream verb stalls the object with a
// recognisable reason.
//
// Pipeline outputs are still not wired into the artifact builder
// (slices 6/7), so the published ExternalArtifact remains a pass-through
// copy. The test verifies the pipeline is exercised end-to-end through
// the reconciler — every verb's evaluator gets called against fetched
// source files — and the controller surfaces both success and the new
// failure modes.
func TestManifestGenerator_PipelineVerbs(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "pipeline-verbs")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000005"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "verbs-mg"}

	files := []gotktestsrv.File{
		{Name: "values.yaml", Body: "image:\n  tag: v1.0.0\n"},
		{Name: "overrides.yaml", Body: "image:\n  tag: v2.0.0\nreplicas: 3\n"},
		{Name: "clusters/a/cluster.yaml", Body: "name: a\ncore: core-1\n"},
		{Name: "clusters/b/cluster.yaml", Body: "name: b\ncore: core-2\n"},
		{Name: "clusters/c/cluster.yaml", Body: "name: c\ncore: core-1\n"},
		{Name: "settings.yaml", Body: "selectedCore: core-1\n"},
	}
	g.Expect(applyGitRepository(objKey, revision, files)).To(Succeed())

	obj := &mgapi.ManifestGenerator{
		TypeMeta: metav1.TypeMeta{
			Kind:       mgapi.ManifestGeneratorKind,
			APIVersion: mgapi.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{Name: objKey.Name, Namespace: objKey.Namespace},
		Spec: mgapi.ManifestGeneratorSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Sources: []mgapi.SourceReference{{
				Alias: "repo",
				Kind:  mgapi.SourceKindGitRepository,
				Name:  objKey.Name,
			}},
			Pipeline: []mgapi.PipelineStep{
				{Name: "baseValues", Load: &mgapi.LoadStep{
					From: "@repo/values.yaml",
				}},
				{Name: "overrideValues", Load: &mgapi.LoadStep{
					From: "@repo/overrides.yaml",
				}},
				{Name: "mergedValues", Merge: &mgapi.MergeStep{
					From: []string{"baseValues", "overrideValues"},
				}},
				{Name: "settings", Load: &mgapi.LoadStep{From: "@repo/settings.yaml"}},
				{Name: "clusters", Load: &mgapi.LoadStep{
					From: "@repo/clusters/*/cluster.yaml",
				}},
				{Name: "selectedClusters", Filter: &mgapi.FilterStep{
					From:  "clusters",
					Where: `{{ eq .value.core .settings.selectedCore }}`,
				}},
				{Name: "clusterNames", Map: &mgapi.MapStep{
					From: "selectedClusters",
					Expr: `{{ .value.name }}`,
				}},
				{Name: "clustersByCore", Group: &mgapi.GroupStep{
					From:    "clusters",
					KeyExpr: `{{ .value.core }}`,
				}},
			},
			Artifacts: []mgapi.ManifestArtifact{{
				Name:     "verbs-out",
				Revision: "@repo",
				Templates: []mgapi.TemplateSpec{{
					From: "@repo/values.yaml",
					To:   "@artifact/values.yaml",
				}},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("reconciles Ready=True with every verb evaluated", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		gt.Expect(gotkconditions.GetReason(result, gotkmeta.ReadyCondition)).
			To(Equal(gotkmeta.SucceededReason))
		gt.Expect(result.Status.Inventory).To(HaveLen(1))
	})

	t.Run("non-map merge input stalls with PipelineFailedReason", func(t *testing.T) {
		gt := NewWithT(t)
		fix := &mgapi.ManifestGenerator{}
		gt.Expect(testClient.Get(ctx, client.ObjectKeyFromObject(obj), fix)).To(Succeed())
		// settings.yaml is a map, but clusterNames is a list of strings
		// after the map step; merging a list into the accumulator must
		// surface a runtime PipelineFailedReason.
		fix.Spec.Pipeline = []mgapi.PipelineStep{
			{Name: "settings", Load: &mgapi.LoadStep{From: "@repo/settings.yaml"}},
			{Name: "clusters", Load: &mgapi.LoadStep{
				From: "@repo/clusters/*/cluster.yaml",
			}},
			{Name: "clusterNames", Map: &mgapi.MapStep{
				From: "clusters",
				Expr: `{{ .value.name }}`,
			}},
			{Name: "broken", Merge: &mgapi.MergeStep{
				From: []string{"settings", "clusterNames"},
			}},
		}
		gt.Expect(testClient.Update(ctx, fix)).To(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.PipelineFailedReason))
	})

	t.Run("filter forward-reference stalls with ValidationFailedReason", func(t *testing.T) {
		gt := NewWithT(t)
		bad := &mgapi.ManifestGenerator{}
		gt.Expect(testClient.Get(ctx, client.ObjectKeyFromObject(obj), bad)).To(Succeed())
		// `selected` references `clusters` which is defined later — must
		// be rejected at validation/compile time, not at evaluation.
		bad.Spec.Pipeline = []mgapi.PipelineStep{
			{Name: "selected", Filter: &mgapi.FilterStep{
				From:  "clusters",
				Where: `{{ true }}`,
			}},
			{Name: "clusters", Load: &mgapi.LoadStep{
				From: "@repo/clusters/*/cluster.yaml",
			}},
		}
		gt.Expect(testClient.Update(ctx, bad)).To(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.ValidationFailedReason))
	})
}
