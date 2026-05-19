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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	gotktestsrv "github.com/fluxcd/pkg/testserver"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// TestManifestGenerator_PartialsAndLookupFile is the slice-9
// integration test. It pins three pieces end-to-end:
//
//  1. `_helpers.tpl` discovery: a partial dropped into the alias root
//     is auto-parsed into the template tree so an `include` in the
//     main template renders its `{{ define }}` block.
//  2. Layered partials: a `_helpers.tpl` closer to the template
//     overrides a same-named `define` from a parent directory. This
//     proves the builder's root-first → template-closest ordering
//     drives text/template's last-define-wins semantics.
//  3. `lookupFile`: the helper reads a static file out of the same
//     alias as the template, jailed to the alias root.
//
// A second sub-test exercises the failure mode: a template that
// `include`s an undefined name stalls with RenderFailedReason rather
// than the generic BuildFailedReason, so authors get the right
// debugging breadcrumb.
func TestManifestGenerator_PartialsAndLookupFile(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "partials")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000009"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "partials-mg"}

	files := []gotktestsrv.File{
		// Pipeline input. Loaded as the `config` step so the template
		// has something to reference.
		{Name: "config.yaml", Body: "name: example\nimage:\n  tag: v9.0.0\n"},

		// Root-level _helpers.tpl publishes `app.fullname` and `app.labels`.
		// `app.labels` will be shadowed by the closer partial below.
		{Name: "_helpers.tpl", Body: `{{- define "app.fullname" -}}
{{ .config.name }}-{{ .config.image.tag }}
{{- end -}}
{{- define "app.labels" -}}
from: root
{{- end -}}
`},

		// Sibling _helpers.tpl in the template's own directory wins
		// the `app.labels` define and adds a new partial.
		{Name: "templates/_helpers.tpl", Body: `{{- define "app.labels" -}}
from: leaf
app.kubernetes.io/managed-by: flux-manifest-generator
{{- end -}}
{{- define "app.image" -}}
{{ .config.image.tag }}
{{- end -}}
`},

		// A static file the template will read via `lookupFile`.
		{Name: "snippets/extra.yaml", Body: "extra: pulled-via-lookupFile\n"},

		// The main template exercises:
		//   - the root-only `app.fullname` partial
		//   - the leaf-overridden `app.labels` partial
		//   - the leaf-only `app.image` partial
		//   - `lookupFile` against a sibling-directory file
		{Name: "templates/app.yaml", Body: `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "app.fullname" . }}
  labels:
{{ include "app.labels" . | indent 4 }}
data:
  image: {{ include "app.image" . }}
{{ lookupFile "snippets/extra.yaml" }}
`},
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
			Pipeline: []mgapi.PipelineStep{{
				Name: "config", Load: &mgapi.LoadStep{From: "@repo/config.yaml"},
			}},
			Artifacts: []mgapi.ManifestArtifact{{
				Name:     "partials-out",
				Revision: "@repo",
				Templates: []mgapi.TemplateSpec{{
					From: "@repo/templates/app.yaml",
					To:   "@artifact/app.yaml",
				}},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("partials and lookupFile resolve in render", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		gt.Expect(result.Status.Inventory).To(HaveLen(1))
		ea := &sourcev1.ExternalArtifact{}
		gt.Expect(testClient.Get(ctx,
			client.ObjectKey{Name: result.Status.Inventory[0].Name, Namespace: result.Status.Inventory[0].Namespace},
			ea)).To(Succeed())
		gt.Expect(ea.Status.Artifact).ToNot(BeNil())

		body := readTarballFile(gt, filepath.Join(testServer.Root(), ea.Status.Artifact.Path), "app.yaml")

		// app.fullname is published only by the root _helpers.tpl —
		// proves root-level partial discovery.
		gt.Expect(body).To(ContainSubstring("name: example-v9.0.0"))
		// app.image is published only by the leaf _helpers.tpl —
		// proves sibling discovery.
		gt.Expect(body).To(ContainSubstring("image: v9.0.0"))
		// app.labels is defined in both; the leaf override must win
		// (text/template last-define-wins, root-first chain order).
		gt.Expect(body).To(ContainSubstring("from: leaf"))
		gt.Expect(body).ToNot(ContainSubstring("from: root"))
		// And the leaf-only extra label that root never published.
		gt.Expect(body).To(ContainSubstring("app.kubernetes.io/managed-by: flux-manifest-generator"))
		// lookupFile content lands verbatim in the rendered output.
		gt.Expect(body).To(ContainSubstring("extra: pulled-via-lookupFile"))
	})

	t.Run("unknown include surfaces RenderFailedReason", func(t *testing.T) {
		gt := NewWithT(t)

		brokenFiles := append([]gotktestsrv.File{}, files...)
		for i := range brokenFiles {
			if brokenFiles[i].Name == "templates/app.yaml" {
				// A name no partial defines — the engine must fail at
				// execute time with the includeFunc "not defined" error
				// and the controller must classify the failure as
				// RenderFailedReason (not the generic BuildFailedReason).
				brokenFiles[i].Body = `data: {{ include "no.such.partial" . }}`
			}
		}
		brokenRev := "main@sha1:000000000000000000000000000000000000000d"
		gt.Expect(applyGitRepository(objKey, brokenRev, brokenFiles)).To(Succeed())

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.RenderFailedReason))
	})
}
