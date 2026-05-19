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
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestManifestGenerator_TemplateRender is the slice-6 integration
// test: an artifact template that uses Sprig + project helpers
// renders against pipeline outputs, the resulting tarball contains
// the rendered bytes (not the source), and a broken template surfaces
// RenderFailedReason rather than the generic BuildFailedReason.
func TestManifestGenerator_TemplateRender(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "render")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000006"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "render-mg"}

	files := []gotktestsrv.File{
		// Pipeline input — loaded as the `config` step.
		{Name: "config.yaml", Body: "image:\n  repository: example\n  tag: v1.2.3\nreplicas: 2\n"},
		// Template under render: exercises field lookup, Sprig (`upper`),
		// the `toYaml` helper for an indented sub-tree, and `required`
		// to assert the happy path passes through.
		{Name: "templates/deploy.yaml", Body: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ required "name required" .config.image.repository | upper }}
spec:
  replicas: {{ .config.replicas }}
  template:
    spec:
      containers:
        - name: app
          image: {{ .config.image.repository }}:{{ .config.image.tag }}
          env:
{{ toYaml .config.image | indent 12 }}
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
				Name:     "render-out",
				Revision: "@repo",
				Templates: []mgapi.TemplateSpec{{
					From: "@repo/templates/deploy.yaml",
					To:   "@artifact/deploy.yaml",
				}},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("renders template against pipeline outputs", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		gt.Expect(gotkconditions.GetReason(result, gotkmeta.ReadyCondition)).
			To(Equal(gotkmeta.SucceededReason))
		gt.Expect(result.Status.Inventory).To(HaveLen(1))

		ea := &sourcev1.ExternalArtifact{}
		gt.Expect(testClient.Get(ctx,
			client.ObjectKey{Name: result.Status.Inventory[0].Name, Namespace: result.Status.Inventory[0].Namespace},
			ea)).To(Succeed())
		gt.Expect(ea.Status.Artifact).ToNot(BeNil())

		body := readTarballFile(gt, filepath.Join(testServer.Root(), ea.Status.Artifact.Path), "deploy.yaml")
		// Field substitution + Sprig `upper`.
		gt.Expect(body).To(ContainSubstring("name: EXAMPLE"))
		// Numeric substitution from the pipeline output.
		gt.Expect(body).To(ContainSubstring("replicas: 2"))
		// Composed substitution (repository:tag).
		gt.Expect(body).To(ContainSubstring("image: example:v1.2.3"))
		// `toYaml | indent 12` block — the helper must strip its
		// trailing newline so the indented block sits cleanly under
		// `env:`.
		gt.Expect(body).To(ContainSubstring("            repository: example"))
		gt.Expect(body).To(ContainSubstring("            tag: v1.2.3"))
	})

	t.Run("broken template surfaces RenderFailedReason", func(t *testing.T) {
		gt := NewWithT(t)

		// Re-publish the source artifact with the deploy.yaml replaced
		// by one whose `required` helper rejects the (intentionally
		// absent) value so the render aborts at execute time.
		brokenFiles := append([]gotktestsrv.File{}, files...)
		for i := range brokenFiles {
			if brokenFiles[i].Name == "templates/deploy.yaml" {
				brokenFiles[i].Body = `name: {{ required "missing.value is required" .missing.value }}`
			}
		}
		brokenRev := "main@sha1:000000000000000000000000000000000000000b"
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

// readTarballFile pulls a single file's body out of a gzipped tar
// archive on disk. Fails the test if the archive cannot be opened or
// the entry is missing.
func readTarballFile(g Gomega, tgzPath, entry string) string {
	f, err := os.Open(tgzPath)
	g.Expect(err).ToNot(HaveOccurred(), "open %s", tgzPath)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	g.Expect(err).ToNot(HaveOccurred(), "gzip %s", tgzPath)
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		g.Expect(err).ToNot(HaveOccurred(), "tar %s", tgzPath)
		if strings.TrimPrefix(hdr.Name, "./") == entry {
			body, err := io.ReadAll(tr)
			g.Expect(err).ToNot(HaveOccurred(), "read %s", entry)
			return string(body)
		}
	}
	g.Expect(false).To(BeTrue(), "entry %q not found in %s", entry, tgzPath)
	return ""
}
