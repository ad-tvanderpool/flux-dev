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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/opencontainers/go-digest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	gotktestsrv "github.com/fluxcd/pkg/testserver"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// TestManifestGenerator_PassthroughCopy walks the slice 3 happy path:
// stand up a GitRepository with a known artifact, create a
// ManifestGenerator that copies one file through, and verify the
// reconciler publishes a matching ExternalArtifact with content
// digest, revision, and storage payload all populated.
func TestManifestGenerator_PassthroughCopy(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "passthrough")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000001"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "passthrough-mg"}

	files := []gotktestsrv.File{
		{Name: "values.yaml", Body: "image:\n  tag: v1.0.0\nreplicas: 3\n"},
		{Name: "README.md", Body: "# example\n"},
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
			Sources: []mgapi.SourceReference{
				{
					Alias: "repo",
					Kind:  mgapi.SourceKindGitRepository,
					Name:  objKey.Name,
				},
			},
			Artifacts: []mgapi.ManifestArtifact{
				{
					Name:     "passthrough-out",
					Revision: "@repo",
					Templates: []mgapi.TemplateSpec{
						{
							From: "@repo/values.yaml",
							To:   "@artifact/values.yaml",
						},
					},
				},
			},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("publishes ExternalArtifact with content digest", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		gt.Expect(gotkconditions.GetReason(result, gotkmeta.ReadyCondition)).
			To(Equal(gotkmeta.SucceededReason))
		gt.Expect(result.Status.Inventory).To(HaveLen(1))
		gt.Expect(result.Status.ObservedGeneration).To(Equal(result.Generation))
		gt.Expect(result.Status.ObservedSourcesDigest).ToNot(BeEmpty())

		inv := result.Status.Inventory[0]
		gt.Expect(inv.Name).To(Equal("passthrough-out"))
		gt.Expect(inv.Namespace).To(Equal(ns.Name))
		gt.Expect(inv.Digest).ToNot(BeEmpty())

		ea := &sourcev1.ExternalArtifact{}
		gt.Expect(testClient.Get(ctx,
			client.ObjectKey{Name: inv.Name, Namespace: inv.Namespace}, ea)).To(Succeed())
		gt.Expect(ea.Spec.SourceRef).ToNot(BeNil())
		gt.Expect(ea.Spec.SourceRef.Kind).To(Equal(mgapi.ManifestGeneratorKind))
		gt.Expect(ea.Spec.SourceRef.Name).To(Equal(obj.Name))
		gt.Expect(ea.Status.Artifact).ToNot(BeNil())
		gt.Expect(ea.Status.Artifact.Digest).To(Equal(inv.Digest))
		gt.Expect(ea.Status.Artifact.Revision).To(Equal(revision))
		gt.Expect(gotkconditions.IsTrue(ea, gotkmeta.ReadyCondition)).To(BeTrue())

		// Verify the artifact tarball actually exists under the storage
		// path the digest record points to.
		storedPath := filepath.Join(testServer.Root(), ea.Status.Artifact.Path)
		_, statErr := os.Stat(storedPath)
		gt.Expect(statErr).ToNot(HaveOccurred(),
			"expected stored artifact at %q", storedPath)
	})

	t.Run("finalizes and removes ExternalArtifact", func(t *testing.T) {
		gt := NewWithT(t)
		gt.Expect(testClient.Delete(ctx, obj)).To(Succeed())

		gt.Eventually(func() bool {
			err := testClient.Get(ctx, client.ObjectKeyFromObject(obj), &mgapi.ManifestGenerator{})
			return apierrors.IsNotFound(err)
		}, timeout, time.Second).Should(BeTrue(), "controller did not finalize")

		eaList := &sourcev1.ExternalArtifactList{}
		_ = testClient.List(ctx, eaList, client.InNamespace(ns.Name))
		gt.Expect(eaList.Items).To(BeEmpty(),
			"expected ExternalArtifacts to be cleaned up by the finalizer")
	})
}

// applyGitRepository materialises a GitRepository whose status points
// at a freshly-uploaded artifact tarball served by the test server,
// the same shape source-controller would publish.
func applyGitRepository(objKey client.ObjectKey, revision string, files []gotktestsrv.File) error {
	artifactName, err := testServer.ArtifactFromFiles(files)
	if err != nil {
		return err
	}

	repo := &sourcev1.GitRepository{
		TypeMeta: metav1.TypeMeta{
			Kind:       sourcev1.GitRepositoryKind,
			APIVersion: sourcev1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{Name: objKey.Name, Namespace: objKey.Namespace},
		Spec: sourcev1.GitRepositorySpec{
			URL:      "https://example.com/repo.git",
			Interval: metav1.Duration{Duration: time.Minute},
		},
	}
	body, err := os.ReadFile(filepath.Join(testServer.Root(), artifactName))
	if err != nil {
		return err
	}
	dig := digest.SHA256.FromBytes(body)
	url := fmt.Sprintf("%s/%s", testServer.URL(), artifactName)

	patchOpts := []client.PatchOption{
		client.ForceOwnership,
		client.FieldOwner("source-controller"),
	}
	if err := testClient.Patch(context.Background(), repo, client.Apply, patchOpts...); err != nil {
		return err
	}

	repo.ManagedFields = nil
	repo.Status = sourcev1.GitRepositoryStatus{
		Conditions: []metav1.Condition{
			{
				Type:               gotkmeta.ReadyCondition,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             gotkmeta.SucceededReason,
			},
		},
		Artifact: &gotkmeta.Artifact{
			Path:           url,
			URL:            url,
			Revision:       revision,
			Digest:         dig.String(),
			LastUpdateTime: metav1.Now(),
		},
	}
	statusOpts := &client.SubResourcePatchOptions{
		PatchOptions: client.PatchOptions{FieldManager: "source-controller"},
	}
	return testClient.Status().Patch(context.Background(), repo, client.Apply, statusOpts)
}
