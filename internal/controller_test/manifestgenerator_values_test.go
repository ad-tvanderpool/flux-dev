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
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	gotktestsrv "github.com/fluxcd/pkg/testserver"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// TestManifestGenerator_Values exercises slice 8 end-to-end:
//   - spec.valuesFrom pulls cluster.yaml out of a ConfigMap under
//     targetPath: cluster.
//   - spec.values lays an inline image override on top so the merge
//     order (valuesFrom first, inline last) is observable.
//   - A pipeline filter step references .values to prove the merged
//     tree is visible to per-item expressions, not just templates.
//   - Updating the ConfigMap triggers a re-render via the slice-8
//     ConfigMap watch.
//   - Removing the ConfigMap with optional: true still reconciles.
func TestManifestGenerator_Values(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "values")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000008"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "values-mg"}

	files := []gotktestsrv.File{
		{Name: "clusters/a.yaml", Body: "name: a\ntier: prod\n"},
		{Name: "clusters/b.yaml", Body: "name: b\ntier: staging\n"},
		{Name: "templates/out.yaml", Body: `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .values.cluster.name }}
data:
  image: {{ .values.image.repository }}:{{ .values.image.tag }}
  tier: {{ .values.cluster.tier }}
  selected: {{ range $i, $c := .selected }}{{ if $i }},{{ end }}{{ $c.name }}{{ end }}
`},
	}
	g.Expect(applyGitRepository(objKey, revision, files)).To(Succeed())

	cmObj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-vars", Namespace: ns.Name},
		Data: map[string]string{
			"cluster.yaml": "name: core-1\ntier: prod\n",
		},
	}
	g.Expect(testClient.Create(ctx, cmObj)).To(Succeed())

	// An "optional" ConfigMap that intentionally does not exist; the
	// reconciler must treat it as absent rather than failing.
	mg := &mgapi.ManifestGenerator{
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
			Values: &apiextensionsv1.JSON{
				Raw: []byte(`{"image":{"repository":"example","tag":"v1.2.3"}}`),
			},
			ValuesFrom: []mgapi.ValuesReference{
				{
					Kind:       "ConfigMap",
					Name:       "cluster-vars",
					ValuesKey:  "cluster.yaml",
					TargetPath: "cluster",
				},
				{
					Kind:     "ConfigMap",
					Name:     "absent-overrides",
					Optional: true,
				},
			},
			Pipeline: []mgapi.PipelineStep{
				{Name: "clusters", Load: &mgapi.LoadStep{
					From: "@repo/clusters/*.yaml",
					As:   mgapi.LoadAsList,
				}},
				{Name: "selected", Filter: &mgapi.FilterStep{
					From:  "clusters",
					Where: `{{ eq .value.tier .values.cluster.tier }}`,
				}},
			},
			Artifacts: []mgapi.ManifestArtifact{{
				Name:     "out",
				Revision: "@repo",
				Templates: []mgapi.TemplateSpec{{
					From: "@repo/templates/out.yaml",
					To:   "@artifact/out.yaml",
				}},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, mg)).To(Succeed())

	t.Run("merges spec.values + spec.valuesFrom into the render scope", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(mg), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		gt.Expect(gotkconditions.GetReason(result, gotkmeta.ReadyCondition)).
			To(Equal(gotkmeta.SucceededReason))
		gt.Expect(result.Status.Inventory).To(HaveLen(1))

		ea := &sourcev1.ExternalArtifact{}
		gt.Expect(testClient.Get(ctx,
			client.ObjectKey{Name: result.Status.Inventory[0].Name, Namespace: result.Status.Inventory[0].Namespace},
			ea)).To(Succeed())

		body := readTarballFile(gt, filepath.Join(testServer.Root(), ea.Status.Artifact.Path), "out.yaml")
		// `.values.cluster.name` came from the ConfigMap.
		gt.Expect(body).To(ContainSubstring("name: core-1"))
		// `.values.image.*` came from spec.values inline.
		gt.Expect(body).To(ContainSubstring("image: example:v1.2.3"))
		// `.values.cluster.tier` arrived via valuesFrom and drove the
		// pipeline filter — the rendered list reflects only prod
		// clusters (a is prod, b is staging).
		gt.Expect(body).To(ContainSubstring("tier: prod"))
		gt.Expect(body).To(ContainSubstring("selected: a"))
		gt.Expect(body).ToNot(ContainSubstring("selected: a,b"))
	})

	t.Run("inline values override valuesFrom on key collision", func(t *testing.T) {
		gt := NewWithT(t)

		// Add a collision: the same `cluster.name` key in inline
		// values must beat the ConfigMap. Retry on conflict because
		// the controller is concurrently patching status.
		gt.Eventually(func() error {
			cur := &mgapi.ManifestGenerator{}
			if err := testClient.Get(ctx, objKey, cur); err != nil {
				return err
			}
			cur.Spec.Values = &apiextensionsv1.JSON{
				Raw: []byte(`{"image":{"repository":"example","tag":"v1.2.3"},"cluster":{"name":"inline-wins"}}`),
			}
			return testClient.Update(ctx, cur)
		}, timeout, 500*time.Millisecond).Should(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, objKey, result)
			if !gotkconditions.IsTrue(result, gotkmeta.ReadyCondition) ||
				result.Status.ObservedGeneration != result.Generation {
				return ""
			}
			ea := &sourcev1.ExternalArtifact{}
			if err := testClient.Get(ctx,
				client.ObjectKey{Name: "out", Namespace: ns.Name}, ea); err != nil {
				return ""
			}
			if ea.Status.Artifact == nil {
				return ""
			}
			return readTarballFile(gt,
				filepath.Join(testServer.Root(), ea.Status.Artifact.Path), "out.yaml")
		}, timeout, time.Second).Should(ContainSubstring("name: inline-wins"))
	})

	t.Run("ConfigMap data change triggers re-render", func(t *testing.T) {
		gt := NewWithT(t)

		gt.Eventually(func() error {
			updated := &corev1.ConfigMap{}
			if err := testClient.Get(ctx,
				client.ObjectKey{Name: "cluster-vars", Namespace: ns.Name}, updated); err != nil {
				return err
			}
			updated.Data["cluster.yaml"] = "name: cm-updated\ntier: staging\n"
			return testClient.Update(ctx, updated)
		}, timeout, 500*time.Millisecond).Should(Succeed())

		// Restore inline values so cluster.name is governed by the CM
		// (the previous sub-test set an inline cluster.name override).
		gt.Eventually(func() error {
			cur := &mgapi.ManifestGenerator{}
			if err := testClient.Get(ctx, objKey, cur); err != nil {
				return err
			}
			cur.Spec.Values = &apiextensionsv1.JSON{
				Raw: []byte(`{"image":{"repository":"example","tag":"v1.2.3"}}`),
			}
			return testClient.Update(ctx, cur)
		}, timeout, 500*time.Millisecond).Should(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, objKey, result)
			if !gotkconditions.IsTrue(result, gotkmeta.ReadyCondition) ||
				result.Status.ObservedGeneration != result.Generation {
				return ""
			}
			ea := &sourcev1.ExternalArtifact{}
			if err := testClient.Get(ctx,
				client.ObjectKey{Name: "out", Namespace: ns.Name}, ea); err != nil {
				return ""
			}
			if ea.Status.Artifact == nil {
				return ""
			}
			return readTarballFile(gt,
				filepath.Join(testServer.Root(), ea.Status.Artifact.Path), "out.yaml")
		}, timeout, time.Second).Should(ContainSubstring("name: cm-updated"))
	})

	t.Run("non-optional missing ConfigMap surfaces SourceFetchFailedReason", func(t *testing.T) {
		gt := NewWithT(t)

		gt.Eventually(func() error {
			cur := &mgapi.ManifestGenerator{}
			if err := testClient.Get(ctx, objKey, cur); err != nil {
				return err
			}
			cur.Spec.ValuesFrom = append(cur.Spec.ValuesFrom, mgapi.ValuesReference{
				Kind:      "ConfigMap",
				Name:      "definitely-missing",
				ValuesKey: "cluster.yaml",
			})
			return testClient.Update(ctx, cur)
		}, timeout, 500*time.Millisecond).Should(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, objKey, result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.SourceFetchFailedReason))
	})

	t.Run("inline values that are not an object stall with ValidationFailedReason", func(t *testing.T) {
		gt := NewWithT(t)

		// Drop the now-broken extra ConfigMap ref from the previous
		// sub-test so we are not also tripping SourceFetchFailed.
		gt.Eventually(func() error {
			cur := &mgapi.ManifestGenerator{}
			if err := testClient.Get(ctx, objKey, cur); err != nil {
				return err
			}
			cur.Spec.ValuesFrom = cur.Spec.ValuesFrom[:2]
			cur.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`[1,2,3]`)}
			return testClient.Update(ctx, cur)
		}, timeout, 500*time.Millisecond).Should(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, objKey, result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.ValidationFailedReason))
	})
}
