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

// TestManifestGenerator_ForEachMap is the slice-7 happy-path
// integration test: an artifact with forEach over a map output and a
// templated `to:` produces one rendered file per map entry, file
// contents reflect the iteration binding, and multi-template artifacts
// emit every template per iteration.
func TestManifestGenerator_ForEachMap(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "foreach-map")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000007"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "foreach-map-mg"}

	files := []gotktestsrv.File{
		// Two campus clusters loaded as a map keyed by directory name.
		{Name: "clusters/campus/east/cluster.yaml", Body: "name: east\nzone: east-1\n"},
		{Name: "clusters/campus/west/cluster.yaml", Body: "name: west\nzone: west-2\n"},
		// Two templates: a delegation document and a sibling
		// per-cluster note. Both should be rendered once per
		// iteration.
		{Name: "templates/delegation.yaml", Body: `apiVersion: dns/v1
kind: Delegation
metadata:
  name: {{ .campus.key }}-delegation
spec:
  zone: {{ .campus.value.zone }}
`},
		{Name: "templates/note.txt", Body: "cluster {{ .campus.key }} -> {{ .campus.value.name }}\n"},
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
				Name: "campusClusters",
				Load: &mgapi.LoadStep{
					From:    "@repo/clusters/campus/*/cluster.yaml",
					As:      mgapi.LoadAsMap,
					KeyExpr: "{{ .path | dir | base }}",
				},
			}},
			Artifacts: []mgapi.ManifestArtifact{{
				Name:    "delegations",
				ForEach: &mgapi.ForEachSpec{From: "campusClusters", As: "campus"},
				Templates: []mgapi.TemplateSpec{
					{
						From: "@repo/templates/delegation.yaml",
						To:   "@artifact/{{ .campus.key }}/delegation.yaml",
					},
					{
						From: "@repo/templates/note.txt",
						To:   "@artifact/notes/{{ .campus.key }}.txt",
					},
				},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("renders one file per map entry per template", func(t *testing.T) {
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

		tarPath := filepath.Join(testServer.Root(), ea.Status.Artifact.Path)
		// Delegation document for the east cluster: key from the map,
		// value from the loaded cluster.yaml.
		east := readTarballFile(gt, tarPath, "east/delegation.yaml")
		gt.Expect(east).To(ContainSubstring("name: east-delegation"))
		gt.Expect(east).To(ContainSubstring("zone: east-1"))

		west := readTarballFile(gt, tarPath, "west/delegation.yaml")
		gt.Expect(west).To(ContainSubstring("name: west-delegation"))
		gt.Expect(west).To(ContainSubstring("zone: west-2"))

		// Second template renders per iteration too.
		eastNote := readTarballFile(gt, tarPath, "notes/east.txt")
		gt.Expect(eastNote).To(Equal("cluster east -> east\n"))
		westNote := readTarballFile(gt, tarPath, "notes/west.txt")
		gt.Expect(westNote).To(Equal("cluster west -> west\n"))
	})
}

// TestManifestGenerator_ForEachList covers list iteration: the binding
// exposes `.index` and `.value`, and the rendered `to:` may use either
// to disambiguate iterations.
func TestManifestGenerator_ForEachList(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "foreach-list")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000071"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "foreach-list-mg"}

	files := []gotktestsrv.File{
		// Glob without keyExpr produces a sorted list of items.
		{Name: "users/alice.yaml", Body: "name: alice\nrole: admin\n"},
		{Name: "users/bob.yaml", Body: "name: bob\nrole: viewer\n"},
		{Name: "templates/user.yaml", Body: `apiVersion: v1
kind: User
metadata:
  name: {{ .user.value.name }}
  annotations:
    index: "{{ .user.index }}"
spec:
  role: {{ .user.value.role }}
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
				Name: "users",
				Load: &mgapi.LoadStep{From: "@repo/users/*.yaml"},
			}},
			Artifacts: []mgapi.ManifestArtifact{{
				Name:    "user-objs",
				ForEach: &mgapi.ForEachSpec{From: "users", As: "user"},
				Templates: []mgapi.TemplateSpec{{
					From: "@repo/templates/user.yaml",
					To:   "@artifact/users/{{ .user.value.name }}.yaml",
				}},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("renders one file per list entry", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		ea := &sourcev1.ExternalArtifact{}
		gt.Expect(testClient.Get(ctx,
			client.ObjectKey{Name: result.Status.Inventory[0].Name, Namespace: result.Status.Inventory[0].Namespace},
			ea)).To(Succeed())
		tarPath := filepath.Join(testServer.Root(), ea.Status.Artifact.Path)

		// Files are loaded in lexicographic order by path, so the
		// list iteration index is alice=0, bob=1.
		alice := readTarballFile(gt, tarPath, "users/alice.yaml")
		gt.Expect(alice).To(ContainSubstring("name: alice"))
		gt.Expect(alice).To(ContainSubstring(`index: "0"`))
		gt.Expect(alice).To(ContainSubstring("role: admin"))

		bob := readTarballFile(gt, tarPath, "users/bob.yaml")
		gt.Expect(bob).To(ContainSubstring("name: bob"))
		gt.Expect(bob).To(ContainSubstring(`index: "1"`))
		gt.Expect(bob).To(ContainSubstring("role: viewer"))
	})
}

// TestManifestGenerator_ForEachErrors exercises the failure modes
// introduced in slice 7: a forEach.from referencing a non-existent
// step is rejected at validation; a forEach.from that exists but
// resolves to a scalar fails the build; and two iterations rendering
// to the same `to:` surface a clear duplicate-path error.
func TestManifestGenerator_ForEachErrors(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "foreach-errors")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000072"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "foreach-err-mg"}

	files := []gotktestsrv.File{
		{Name: "scalar.yaml", Body: "just-a-string\n"},
		{Name: "clusters/a.yaml", Body: "name: a\n"},
		{Name: "clusters/b.yaml", Body: "name: b\n"},
		{Name: "templates/x.yaml", Body: "name: {{ .item.value.name }}\n"},
		// Constant `to:` so two iterations collide on the same path.
		{Name: "templates/static.yaml", Body: "x: 1\n"},
	}
	g.Expect(applyGitRepository(objKey, revision, files)).To(Succeed())

	t.Run("unknown forEach.from stalls with ValidationFailedReason", func(t *testing.T) {
		gt := NewWithT(t)
		obj := &mgapi.ManifestGenerator{
			TypeMeta: metav1.TypeMeta{
				Kind:       mgapi.ManifestGeneratorKind,
				APIVersion: mgapi.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{Name: "unknown-step", Namespace: objKey.Namespace},
			Spec: mgapi.ManifestGeneratorSpec{
				Interval: metav1.Duration{Duration: time.Minute},
				Sources: []mgapi.SourceReference{{
					Alias: "repo", Kind: mgapi.SourceKindGitRepository, Name: objKey.Name,
				}},
				Artifacts: []mgapi.ManifestArtifact{{
					Name:    "out",
					ForEach: &mgapi.ForEachSpec{From: "doesNotExist", As: "x"},
					Templates: []mgapi.TemplateSpec{{
						From: "@repo/templates/x.yaml",
						To:   "@artifact/{{ .x.value.name }}.yaml",
					}},
				}},
			},
		}
		gt.Expect(testClient.Create(ctx, obj)).To(Succeed())
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.ValidationFailedReason))
	})

	t.Run("scalar forEach.from fails at build with BuildFailedReason", func(t *testing.T) {
		gt := NewWithT(t)
		obj := &mgapi.ManifestGenerator{
			TypeMeta: metav1.TypeMeta{
				Kind:       mgapi.ManifestGeneratorKind,
				APIVersion: mgapi.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{Name: "scalar-foreach", Namespace: objKey.Namespace},
			Spec: mgapi.ManifestGeneratorSpec{
				Interval: metav1.Duration{Duration: time.Minute},
				Sources: []mgapi.SourceReference{{
					Alias: "repo", Kind: mgapi.SourceKindGitRepository, Name: objKey.Name,
				}},
				Pipeline: []mgapi.PipelineStep{{
					Name: "scalar",
					Load: &mgapi.LoadStep{From: "@repo/scalar.yaml"},
				}},
				Artifacts: []mgapi.ManifestArtifact{{
					Name:    "out",
					ForEach: &mgapi.ForEachSpec{From: "scalar", As: "x"},
					Templates: []mgapi.TemplateSpec{{
						From: "@repo/templates/x.yaml",
						To:   "@artifact/{{ .x.value }}.yaml",
					}},
				}},
			},
		}
		gt.Expect(testClient.Create(ctx, obj)).To(Succeed())
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.BuildFailedReason))
	})

	t.Run("colliding output paths fail with BuildFailedReason", func(t *testing.T) {
		gt := NewWithT(t)
		obj := &mgapi.ManifestGenerator{
			TypeMeta: metav1.TypeMeta{
				Kind:       mgapi.ManifestGeneratorKind,
				APIVersion: mgapi.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{Name: "colliding-paths", Namespace: objKey.Namespace},
			Spec: mgapi.ManifestGeneratorSpec{
				Interval: metav1.Duration{Duration: time.Minute},
				Sources: []mgapi.SourceReference{{
					Alias: "repo", Kind: mgapi.SourceKindGitRepository, Name: objKey.Name,
				}},
				Pipeline: []mgapi.PipelineStep{{
					Name: "clusters",
					Load: &mgapi.LoadStep{From: "@repo/clusters/*.yaml"},
				}},
				Artifacts: []mgapi.ManifestArtifact{{
					Name:    "out",
					ForEach: &mgapi.ForEachSpec{From: "clusters", As: "x"},
					Templates: []mgapi.TemplateSpec{{
						From: "@repo/templates/static.yaml",
						// No template fragment — every iteration
						// writes to the same path, which must error.
						To: "@artifact/static.yaml",
					}},
				}},
			},
		}
		gt.Expect(testClient.Create(ctx, obj)).To(Succeed())
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })

		gt.Eventually(func() string {
			result := &mgapi.ManifestGenerator{}
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			if !gotkconditions.IsFalse(result, gotkmeta.ReadyCondition) {
				return ""
			}
			return gotkconditions.GetReason(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(Equal(mgapi.BuildFailedReason))
	})
}

// TestManifestGenerator_MultiTemplateNoForEach verifies the
// multi-template surface works without forEach: every templates[]
// entry renders once into a distinct `to:` path inside the same
// artifact.
func TestManifestGenerator_MultiTemplateNoForEach(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "multi-template")
	g.Expect(err).ToNot(HaveOccurred())

	revision := "main@sha1:0000000000000000000000000000000000000073"
	objKey := client.ObjectKey{Namespace: ns.Name, Name: "multi-template-mg"}

	files := []gotktestsrv.File{
		{Name: "config.yaml", Body: "name: example\n"},
		{Name: "templates/a.yaml", Body: "kind: A\nname: {{ .config.name }}\n"},
		{Name: "templates/b.yaml", Body: "kind: B\nname: {{ .config.name }}\n"},
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
				Name: "multi-out",
				Templates: []mgapi.TemplateSpec{
					{From: "@repo/templates/a.yaml", To: "@artifact/dir/a.yaml"},
					{From: "@repo/templates/b.yaml", To: "@artifact/dir/b.yaml"},
				},
			}},
		},
	}
	g.Expect(testClient.Create(ctx, obj)).To(Succeed())

	t.Run("emits one rendered file per template", func(t *testing.T) {
		gt := NewWithT(t)
		result := &mgapi.ManifestGenerator{}
		gt.Eventually(func() bool {
			_ = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
			return gotkconditions.IsTrue(result, gotkmeta.ReadyCondition)
		}, timeout, time.Second).Should(BeTrue(), "controller did not become Ready")

		ea := &sourcev1.ExternalArtifact{}
		gt.Expect(testClient.Get(ctx,
			client.ObjectKey{Name: result.Status.Inventory[0].Name, Namespace: result.Status.Inventory[0].Namespace},
			ea)).To(Succeed())
		tarPath := filepath.Join(testServer.Root(), ea.Status.Artifact.Path)

		gt.Expect(readTarballFile(gt, tarPath, "dir/a.yaml")).To(ContainSubstring("kind: A"))
		gt.Expect(readTarballFile(gt, tarPath, "dir/b.yaml")).To(ContainSubstring("kind: B"))
	})
}
