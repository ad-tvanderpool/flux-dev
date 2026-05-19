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

// TestManifestGenerator_PipelineLoad exercises slice 4: a
// ManifestGenerator whose spec.pipeline only uses the `load` verb is
// compiled, evaluated, and reconciles to Ready=True. Pipeline outputs
// are not yet consumed by the artifact builder — that wiring lands in
// slices 6/7 — so the artifact is still a pass-through copy. The test
// verifies the pipeline runs (covers single yaml + glob-as-map with
// keyExpr) and that an evaluation error flips the object Ready=False
// with PipelineFailedReason.
func TestManifestGenerator_PipelineLoad(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "pipeline-load")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000004"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "pipeline-mg"}

	files := []gotktestsrv.File{
		{Name: "talos/cluster.yaml", Body: "name: core\ncore: core\n"},
		{Name: "clusters/campus/a/talos/cluster.yaml", Body: "name: a\ncore: core\n"},
		{Name: "clusters/campus/b/talos/cluster.yaml", Body: "name: b\ncore: core\n"},
		{Name: "values.yaml", Body: "image:\n  tag: v1.0.0\n"},
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
				{Name: "cluster", Load: &mgapi.LoadStep{
					From:   "@repo/talos/cluster.yaml",
					Format: mgapi.LoadFormatYAML,
				}},
				{Name: "campusClusters", Load: &mgapi.LoadStep{
					From:    "@repo/clusters/campus/*/talos/cluster.yaml",
					Format:  mgapi.LoadFormatYAML,
					As:      mgapi.LoadAsMap,
					KeyExpr: "{{ .path | dir | dir | base }}",
				}},
			},
			Artifacts: []mgapi.ManifestArtifact{{
				Name:     "pipeline-out",
				Revision: "@repo",
				Templates: []mgapi.TemplateSpec{{
					From: "@repo/values.yaml",
					To:   "@artifact/values.yaml",
				}},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("reconciles with pipeline evaluated", func(t *testing.T) {
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

	t.Run("invalid pipeline keyExpr stalls with ValidationFailedReason", func(t *testing.T) {
		gt := NewWithT(t)

		bad := &mgapi.ManifestGenerator{}
		gt.Expect(testClient.Get(ctx, client.ObjectKeyFromObject(obj), bad)).To(Succeed())
		bad.Spec.Pipeline = []mgapi.PipelineStep{{
			Name: "campusClusters", Load: &mgapi.LoadStep{
				From:    "@repo/clusters/campus/*/talos/cluster.yaml",
				As:      mgapi.LoadAsMap,
				KeyExpr: "{{ .path", // unterminated action
			},
		}}
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

	t.Run("missing file at runtime fails with PipelineFailedReason", func(t *testing.T) {
		gt := NewWithT(t)

		fix := &mgapi.ManifestGenerator{}
		gt.Expect(testClient.Get(ctx, client.ObjectKeyFromObject(obj), fix)).To(Succeed())
		fix.Spec.Pipeline = []mgapi.PipelineStep{{
			Name: "missing", Load: &mgapi.LoadStep{
				From: "@repo/no/such/file.yaml",
			},
		}}
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
}
