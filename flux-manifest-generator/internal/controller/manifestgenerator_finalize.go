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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkstorage "github.com/fluxcd/pkg/artifact/storage"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// finalize removes any owned ExternalArtifacts plus their stored
// tarballs, then drops the finalizer.
func (r *ManifestGeneratorReconciler) finalize(ctx context.Context,
	obj *mgapi.ManifestGenerator) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	r.finalizeExternalArtifacts(ctx, obj.Status.Inventory)

	controllerutil.RemoveFinalizer(obj, mgapi.Finalizer)
	log.Info("Removed finalizer", "finalizer", mgapi.Finalizer)
	return ctrl.Result{}, nil
}

// finalizeExternalArtifacts deletes the ExternalArtifact objects and
// their backing storage entries listed in refs.
func (r *ManifestGeneratorReconciler) finalizeExternalArtifacts(ctx context.Context,
	refs []mgapi.ExternalArtifactReference) {
	log := ctrl.LoggerFrom(ctx)

	for _, ref := range refs {
		path := gotkstorage.ArtifactPath(sourcev1.ExternalArtifactKind, ref.Namespace, ref.Name, "*")
		if rmDir, err := r.Storage.RemoveAll(gotkmeta.Artifact{Path: path}); err != nil {
			log.Error(err, "failed to delete artifact from storage", "path", path)
		} else if rmDir != "" {
			log.Info(fmt.Sprintf("%s/%s/%s deleted from storage", sourcev1.ExternalArtifactKind, ref.Namespace, ref.Name), "path", rmDir)
		}

		ea := &sourcev1.ExternalArtifact{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
		}
		if err := r.Client.Delete(ctx, ea); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete ExternalArtifact",
				"namespace", ref.Namespace, "name", ref.Name)
		} else {
			log.Info(fmt.Sprintf("%s/%s/%s deleted from cluster",
				sourcev1.ExternalArtifactKind, ref.Namespace, ref.Name))
		}
	}
}

// addFinalizer seeds the Ready/Reconciling conditions and asks
// controller-runtime to requeue immediately so the freshly persisted
// finalizer is in place before the reconcile loop runs.
func (r *ManifestGeneratorReconciler) addFinalizer(obj *mgapi.ManifestGenerator) (ctrl.Result, error) {
	controllerutil.AddFinalizer(obj, mgapi.Finalizer)
	if obj.IsDisabled() {
		gotkconditions.MarkTrue(obj, gotkmeta.ReadyCondition,
			mgapi.ReconciliationDisabledReason, "%s", msgInitSuspended)
	} else {
		gotkconditions.MarkUnknown(obj, gotkmeta.ReadyCondition,
			gotkmeta.ProgressingReason, "%s", msgInProgress)
		gotkconditions.MarkReconciling(obj, gotkmeta.ProgressingReason,
			"%s", msgInProgress)
	}
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}
