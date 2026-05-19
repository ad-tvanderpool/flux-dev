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

// Package values materialises the `.values` template scope from
// `spec.values` (inline) and `spec.valuesFrom` (ConfigMap references).
// Each `valuesFrom` entry is read live from the API server (the
// controller intentionally does not cache ConfigMaps) and deep-merged
// in spec order, then `spec.values` is layered on top so inline data
// always wins. The result is the value bound to the `.values` key in
// pipeline expressions and artifact templates.
package values

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// ReservedName is the key under which the merged values tree is
// published into the template scope. Pipeline step names and
// `forEach.as` bindings must not collide with it, so the validator
// rejects any spec that would shadow `.values`.
const ReservedName = "values"

// FetchError marks an error whose root cause is a live ConfigMap read
// failing or a referenced data key being absent. The controller wraps
// errors of this kind into a SourceFetchFailedReason condition so
// transient API-server or operator-side gaps requeue instead of
// stalling.
type FetchError struct {
	Err error
}

func (e *FetchError) Error() string { return e.Err.Error() }
func (e *FetchError) Unwrap() error { return e.Err }

// AccessError marks a cross-namespace reference rejected by the
// runtime ACL. The controller surfaces it as AccessDeniedReason.
type AccessError struct {
	Err error
}

func (e *AccessError) Error() string { return e.Err.Error() }
func (e *AccessError) Unwrap() error { return e.Err }

// Resolver materialises the `.values` tree for a ManifestGenerator. It
// is constructed once per controller and is safe for concurrent use:
// every Resolve call gets a fresh output tree, and the underlying
// client.Reader is expected to be the controller's API-server-direct
// reader so ConfigMap state is never read from a stale informer cache.
type Resolver struct {
	Reader               client.Reader
	NoCrossNamespaceRefs bool
}

// NewResolver returns a Resolver bound to the supplied API-server
// reader. The reader is used to fetch every ConfigMap referenced by
// `spec.valuesFrom`; pass the controller's APIReader (or the
// cache-bypassing client) to honour the project's
// `Client.Cache.DisableFor` policy.
func NewResolver(reader client.Reader, noCrossNamespaceRefs bool) *Resolver {
	return &Resolver{Reader: reader, NoCrossNamespaceRefs: noCrossNamespaceRefs}
}

// Resolve walks `spec.valuesFrom` (in spec order) followed by
// `spec.values`, deep-merging each contribution into a single tree.
// Helm-style semantics: later entries win on key collisions, nested
// maps recurse, lists/scalars are replaced. The result is a fresh tree
// whose substructure does not alias caller-owned objects, so it is
// safe to hand to template engines that retain references.
func (r *Resolver) Resolve(ctx context.Context, obj *mgapi.ManifestGenerator) (map[string]any, error) {
	out := map[string]any{}

	for i := range obj.Spec.ValuesFrom {
		ref := &obj.Spec.ValuesFrom[i]
		contrib, err := r.resolveOne(ctx, ref, obj.Namespace)
		if err != nil {
			return nil, fmt.Errorf("valuesFrom[%d] %s %q: %w",
				i, ref.Kind, ref.Name, err)
		}
		if contrib == nil {
			continue
		}
		out = deepMerge(out, contrib).(map[string]any)
	}

	if obj.Spec.Values != nil && len(obj.Spec.Values.Raw) > 0 {
		inline, err := decodeInline(obj.Spec.Values.Raw)
		if err != nil {
			return nil, fmt.Errorf("spec.values: %w", err)
		}
		if inline != nil {
			out = deepMerge(out, inline).(map[string]any)
		}
	}

	return out, nil
}

// resolveOne reads one ConfigMap and turns it into a values
// contribution (a map[string]any wrapped at the requested targetPath).
// Returns (nil, nil) when the reference is Optional and the underlying
// object or key is absent.
func (r *Resolver) resolveOne(ctx context.Context,
	ref *mgapi.ValuesReference,
	mgNamespace string) (map[string]any, error) {

	if ref.Kind != "" && ref.Kind != "ConfigMap" {
		// Defensive: the CRD already pins Kind to ConfigMap.
		return nil, fmt.Errorf("unsupported kind %q", ref.Kind)
	}

	ns := ref.Namespace
	if ns == "" {
		ns = mgNamespace
	}
	if r.NoCrossNamespaceRefs && ns != mgNamespace {
		return nil, &AccessError{
			Err: fmt.Errorf("cross-namespace reference to %s/%s is not allowed", ns, ref.Name),
		}
	}

	cm := &corev1.ConfigMap{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, cm); err != nil {
		if apierrors.IsNotFound(err) && ref.Optional {
			return nil, nil
		}
		return nil, &FetchError{Err: err}
	}

	injected, err := selectContent(cm, ref)
	if err != nil {
		if ref.Optional {
			return nil, nil
		}
		return nil, err
	}
	if injected == nil {
		return map[string]any{}, nil
	}

	return placeAtPath(ref.TargetPath, injected)
}

// selectContent picks the value injected by a ValuesReference. With
// `valuesKey` set, that single key's value is parsed as YAML; with it
// empty, every entry in `data` is injected as a string keyed by its
// name. A FetchError is returned when the requested key is absent so
// the caller can decide whether `optional` applies.
func selectContent(cm *corev1.ConfigMap, ref *mgapi.ValuesReference) (any, error) {
	if ref.ValuesKey != "" {
		raw, ok := cm.Data[ref.ValuesKey]
		if !ok {
			return nil, &FetchError{
				Err: fmt.Errorf("key %q not found in ConfigMap data", ref.ValuesKey),
			}
		}
		var v any
		if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("decode key %q: %w", ref.ValuesKey, err)
		}
		return v, nil
	}
	out := make(map[string]any, len(cm.Data))
	for k, v := range cm.Data {
		out[k] = v
	}
	return out, nil
}

// decodeInline parses spec.values bytes as YAML (a superset of JSON
// thanks to sigs.k8s.io/yaml) and validates the result is either an
// object or null. Non-object scalars (arrays, numbers, strings) have
// no coherent merge semantics at the .values root so they are
// rejected.
func decodeInline(raw []byte) (map[string]any, error) {
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	switch tv := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return tv, nil
	default:
		return nil, fmt.Errorf("must be an object, got %T", v)
	}
}

// placeAtPath wraps value into a nested map at the dot-separated
// targetPath. With an empty path the value must itself be a
// map[string]any because only maps can deep-merge at the .values root.
// An empty segment (leading, trailing, or doubled dot) is rejected so
// typos surface as a clear validation error rather than a silently
// orphaned key.
func placeAtPath(path string, value any) (map[string]any, error) {
	if path == "" {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("injected value is %T but targetPath is empty "+
				"(only objects merge at the values root)", value)
		}
		return m, nil
	}
	parts := strings.Split(path, ".")
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("invalid targetPath %q: empty segment", path)
		}
	}
	out := value
	for i := len(parts) - 1; i >= 0; i-- {
		out = map[string]any{parts[i]: out}
	}
	return out.(map[string]any), nil
}

// ValidateTargetPath reports whether path is a syntactically valid
// dot-separated target path. The validator uses it so a typo lands as
// ValidationFailedReason at admission instead of a runtime resolve
// failure. An empty path is allowed (it means "merge at the root").
func ValidateTargetPath(path string) error {
	if path == "" {
		return nil
	}
	for _, p := range strings.Split(path, ".") {
		if p == "" {
			return fmt.Errorf("invalid targetPath %q: empty segment", path)
		}
	}
	return nil
}

// ValidateInline parses raw and confirms it decodes to an object (or
// null), with no other shape allowed at the values root. Used by the
// validator to surface malformed inline values at admission rather
// than at reconcile time.
func ValidateInline(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	_, err := decodeInline(raw)
	return err
}

// deepMerge returns a new value combining a and b. When both are maps,
// keys recurse; on any other type pair, b wins. The implementation
// never mutates a or b: every map and slice put into the result is a
// deep clone, so writers on either side cannot reach back into a
// caller-owned tree through aliasing.
func deepMerge(a, b any) any {
	am, aOk := a.(map[string]any)
	bm, bOk := b.(map[string]any)
	if !aOk || !bOk {
		return deepClone(b)
	}
	out := make(map[string]any, len(am)+len(bm))
	for k, v := range am {
		out[k] = deepClone(v)
	}
	for k, v := range bm {
		if cur, ok := out[k]; ok {
			out[k] = deepMerge(cur, v)
		} else {
			out[k] = deepClone(v)
		}
	}
	return out
}

// deepClone recursively copies the YAML-decoder-shaped tree v.
func deepClone(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = deepClone(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = deepClone(vv)
		}
		return out
	default:
		return v
	}
}
