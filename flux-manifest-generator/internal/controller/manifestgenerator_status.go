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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	gotkpatch "github.com/fluxcd/pkg/runtime/patch"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

const (
	msgInProgress             = "Reconciliation in progress"
	msgInitSuspended          = "Initialized with reconciliation suspended"
	msgReconciliationDisabled = "Reconciliation is disabled"
)

// summarizeStatus runs at the end of every reconcile to record manual
// reconcile timestamps, normalise condition reasons, and patch.
func (r *ManifestGeneratorReconciler) summarizeStatus(ctx context.Context,
	obj *mgapi.ManifestGenerator,
	patcher *gotkpatch.SerialPatcher) error {
	if v, ok := gotkmeta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
		obj.SetLastHandledReconcileAt(v)
	}

	if gotkconditions.IsFalse(obj, gotkmeta.ReadyCondition) &&
		gotkconditions.Has(obj, gotkmeta.ReconcilingCondition) {
		rc := gotkconditions.Get(obj, gotkmeta.ReconcilingCondition)
		rc.Reason = gotkmeta.ProgressingWithRetryReason
		gotkconditions.Set(obj, rc)
	}

	if gotkconditions.IsTrue(obj, gotkmeta.ReadyCondition) || gotkconditions.IsTrue(obj, gotkmeta.StalledCondition) {
		gotkconditions.Delete(obj, gotkmeta.ReconcilingCondition)
	}

	return r.patchStatus(ctx, obj, patcher)
}

// patchStatus patches the object's status sub-resource and finalizers.
func (r *ManifestGeneratorReconciler) patchStatus(ctx context.Context,
	obj *mgapi.ManifestGenerator,
	patcher *gotkpatch.SerialPatcher) (retErr error) {
	owned := []string{
		gotkmeta.ReadyCondition,
		gotkmeta.ReconcilingCondition,
		gotkmeta.StalledCondition,
	}
	patchOpts := []gotkpatch.Option{
		gotkpatch.WithOwnedConditions{Conditions: owned},
		gotkpatch.WithForceOverwriteConditions{},
		gotkpatch.WithFieldOwner(r.ControllerName),
	}

	if err := patcher.Patch(ctx, obj, patchOpts...); err != nil {
		if !obj.GetDeletionTimestamp().IsZero() {
			err = kerrors.FilterOut(err, func(e error) bool { return apierrors.IsNotFound(e) })
		}
		retErr = kerrors.NewAggregate([]error{retErr, err})
	}
	return retErr
}

// newTerminalErrorFor sets Ready=False and Stalled=True with the given
// reason, fires a warning event, and returns a reconcile.TerminalError
// so controller-runtime stops requeueing.
func (r *ManifestGeneratorReconciler) newTerminalErrorFor(obj *mgapi.ManifestGenerator,
	reason string, format string, args ...any) error {
	terminal := fmt.Errorf(format, args...)
	gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, reason, "%s", terminal.Error())
	gotkconditions.MarkStalled(obj, reason, "%s", terminal.Error())
	r.Event(obj, corev1.EventTypeWarning, reason, terminal.Error())
	return reconcile.TerminalError(terminal)
}
