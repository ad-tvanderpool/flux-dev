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

package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/values"
)

// resolveValues materialises the `.values` template scope from
// spec.values + spec.valuesFrom. ConfigMaps are read through the
// controller's APIReader so the cache-disabled-for-ConfigMaps policy
// in cmd/main.go is honoured (every reconcile sees current state, and
// the controller never holds a long-lived informer cache of ConfigMap
// payloads).
func (r *ManifestGeneratorReconciler) resolveValues(ctx context.Context,
	obj *mgapi.ManifestGenerator) (map[string]any, error) {
	if obj.Spec.Values == nil && len(obj.Spec.ValuesFrom) == 0 {
		return map[string]any{}, nil
	}
	reader := r.valuesReader()
	resolver := values.NewResolver(reader, r.NoCrossNamespaceRefs)
	return resolver.Resolve(ctx, obj)
}

// valuesReader returns the reader Resolve uses to fetch ConfigMaps.
// APIReader bypasses any cache; when the reconciler is constructed
// without one (some tests) we fall back to the standard client.
func (r *ManifestGeneratorReconciler) valuesReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// handleValuesError classifies a values-resolution failure into a
// terminal vs requeue condition. Cross-namespace ACL rejections are
// terminal (only a spec change can fix them); every other failure is
// treated as a transient fetch problem so it requeues at the
// dependency interval rather than stalling.
func (r *ManifestGeneratorReconciler) handleValuesError(ctx context.Context,
	obj *mgapi.ManifestGenerator, err error) (ctrl.Result, error) {
	var accessErr *values.AccessError
	if errors.As(err, &accessErr) {
		return ctrl.Result{}, r.newTerminalErrorFor(obj, mgapi.AccessDeniedReason,
			"resolve values: %s", err.Error())
	}
	msg := fmt.Sprintf("resolve values: %s", err.Error())
	gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, mgapi.SourceFetchFailedReason, "%s", msg)
	r.Event(obj, corev1.EventTypeWarning, mgapi.SourceFetchFailedReason, msg)
	ctrl.LoggerFrom(ctx).Error(err, "failed to resolve values, retrying")
	return ctrl.Result{RequeueAfter: r.DependencyRequeueInterval}, nil
}
