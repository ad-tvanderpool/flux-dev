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

package values

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

func newFakeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func cm(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       data,
	}
}

func mg(ns string, inline string, refs ...mgapi.ValuesReference) *mgapi.ManifestGenerator {
	out := &mgapi.ManifestGenerator{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "test"},
		Spec:       mgapi.ManifestGeneratorSpec{ValuesFrom: refs},
	}
	if inline != "" {
		out.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(inline)}
	}
	return out
}

func TestResolver_InlineOnly(t *testing.T) {
	r := NewResolver(newFakeClient(), false)
	got, err := r.Resolve(context.Background(),
		mg("ns", `{"replicas":3,"image":{"tag":"v1"}}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := map[string]any{
		"replicas": float64(3),
		"image":    map[string]any{"tag": "v1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolver_InlineNullIsNoop(t *testing.T) {
	r := NewResolver(newFakeClient(), false)
	got, err := r.Resolve(context.Background(), mg("ns", `null`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty tree, got %#v", got)
	}
}

func TestResolver_InlineNonObjectRejected(t *testing.T) {
	r := NewResolver(newFakeClient(), false)
	_, err := r.Resolve(context.Background(), mg("ns", `42`))
	if err == nil || !contains(err.Error(), "must be an object") {
		t.Fatalf("expected rejection of scalar values; got %v", err)
	}
}

func TestResolver_ValuesFromConfigMap_WithKey(t *testing.T) {
	src := cm("ns", "cluster-vars", map[string]string{
		"cluster.yaml": "name: core\nreplicas: 2\n",
	})
	r := NewResolver(newFakeClient(src), false)
	got, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:       "ConfigMap",
			Name:       "cluster-vars",
			ValuesKey:  "cluster.yaml",
			TargetPath: "cluster",
		}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := map[string]any{
		"cluster": map[string]any{
			"name":     "core",
			"replicas": float64(2),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolver_ValuesFromConfigMap_AllKeysAsStrings(t *testing.T) {
	src := cm("ns", "env-vars", map[string]string{
		"HOST": "example.com",
		"PORT": "8443",
	})
	r := NewResolver(newFakeClient(src), false)
	got, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:       "ConfigMap",
			Name:       "env-vars",
			TargetPath: "env",
		}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := map[string]any{
		"env": map[string]any{
			"HOST": "example.com",
			"PORT": "8443",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolver_ValuesFrom_RootMergeRequiresMap(t *testing.T) {
	// valuesKey value is a scalar; empty targetPath means "merge at the
	// root" which is only legal for object values.
	src := cm("ns", "scalar", map[string]string{"k": "42"})
	r := NewResolver(newFakeClient(src), false)
	_, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:      "ConfigMap",
			Name:      "scalar",
			ValuesKey: "k",
		}))
	if err == nil || !contains(err.Error(), "only objects merge at the values root") {
		t.Fatalf("expected root-merge rejection of scalar; got %v", err)
	}
}

func TestResolver_OptionalMissingConfigMap(t *testing.T) {
	r := NewResolver(newFakeClient(), false)
	got, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:     "ConfigMap",
			Name:     "absent",
			Optional: true,
		}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty tree, got %#v", got)
	}
}

func TestResolver_NonOptionalMissingConfigMapIsFetchError(t *testing.T) {
	r := NewResolver(newFakeClient(), false)
	_, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{Kind: "ConfigMap", Name: "absent"}))
	if err == nil {
		t.Fatal("expected error for missing required ConfigMap")
	}
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T: %v", err, err)
	}
}

func TestResolver_OptionalMissingKey(t *testing.T) {
	src := cm("ns", "partial", map[string]string{"other": "x"})
	r := NewResolver(newFakeClient(src), false)
	got, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:      "ConfigMap",
			Name:      "partial",
			ValuesKey: "missing",
			Optional:  true,
		}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty tree, got %#v", got)
	}
}

func TestResolver_NonOptionalMissingKeyIsFetchError(t *testing.T) {
	src := cm("ns", "partial", map[string]string{"other": "x"})
	r := NewResolver(newFakeClient(src), false)
	_, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:      "ConfigMap",
			Name:      "partial",
			ValuesKey: "missing",
		}))
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T: %v", err, err)
	}
}

func TestResolver_InlineOverridesValuesFrom(t *testing.T) {
	src := cm("ns", "base", map[string]string{
		"v.yaml": "image:\n  tag: from-cm\nreplicas: 1\n",
	})
	r := NewResolver(newFakeClient(src), false)
	got, err := r.Resolve(context.Background(), mg("ns",
		`{"image":{"tag":"from-inline"}}`,
		mgapi.ValuesReference{
			Kind:      "ConfigMap",
			Name:      "base",
			ValuesKey: "v.yaml",
		}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := map[string]any{
		"image":    map[string]any{"tag": "from-inline"},
		"replicas": float64(1),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolver_ValuesFromMergedInSpecOrder(t *testing.T) {
	first := cm("ns", "first", map[string]string{
		"v.yaml": "image:\n  tag: from-first\nshared:\n  a: 1\n",
	})
	second := cm("ns", "second", map[string]string{
		"v.yaml": "image:\n  tag: from-second\nshared:\n  b: 2\n",
	})
	r := NewResolver(newFakeClient(first, second), false)
	got, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind: "ConfigMap", Name: "first", ValuesKey: "v.yaml",
		},
		mgapi.ValuesReference{
			Kind: "ConfigMap", Name: "second", ValuesKey: "v.yaml",
		},
	))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := map[string]any{
		"image": map[string]any{"tag": "from-second"},
		"shared": map[string]any{
			"a": float64(1),
			"b": float64(2),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolver_CrossNamespaceACL(t *testing.T) {
	src := cm("other", "vars", map[string]string{"v.yaml": "k: v"})
	r := NewResolver(newFakeClient(src), true /* NoCrossNamespaceRefs */)
	_, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:      "ConfigMap",
			Name:      "vars",
			Namespace: "other",
			ValuesKey: "v.yaml",
		}))
	var ae *AccessError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AccessError, got %T: %v", err, err)
	}
}

func TestResolver_TargetPathNested(t *testing.T) {
	src := cm("ns", "nested", map[string]string{
		"v.yaml": "tag: v1\n",
	})
	r := NewResolver(newFakeClient(src), false)
	got, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:       "ConfigMap",
			Name:       "nested",
			ValuesKey:  "v.yaml",
			TargetPath: "image",
		}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := map[string]any{
		"image": map[string]any{"tag": "v1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolver_TargetPathEmptySegmentRejected(t *testing.T) {
	src := cm("ns", "x", map[string]string{"v.yaml": "k: v"})
	r := NewResolver(newFakeClient(src), false)
	_, err := r.Resolve(context.Background(), mg("ns", "",
		mgapi.ValuesReference{
			Kind:       "ConfigMap",
			Name:       "x",
			ValuesKey:  "v.yaml",
			TargetPath: "a..b",
		}))
	if err == nil || !contains(err.Error(), "empty segment") {
		t.Fatalf("expected empty-segment error; got %v", err)
	}
}

func TestResolver_NoInputs(t *testing.T) {
	r := NewResolver(newFakeClient(), false)
	got, err := r.Resolve(context.Background(), mg("ns", ""))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("expected empty non-nil map, got %#v", got)
	}
}

func TestValidateInline(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"empty", "", true},
		{"object", `{"a":1}`, true},
		{"null", `null`, true},
		{"array", `[1,2]`, false},
		{"scalar", `"x"`, false},
		{"malformed", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInline([]byte(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
}

func TestValidateTargetPath(t *testing.T) {
	if err := ValidateTargetPath(""); err != nil {
		t.Fatalf("empty path should be valid: %v", err)
	}
	if err := ValidateTargetPath("a.b.c"); err != nil {
		t.Fatalf("dotted path should be valid: %v", err)
	}
	if err := ValidateTargetPath(".bad"); err == nil {
		t.Fatal("leading dot should be invalid")
	}
	if err := ValidateTargetPath("a..b"); err == nil {
		t.Fatal("doubled dot should be invalid")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
