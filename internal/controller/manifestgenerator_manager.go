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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gotkpredicates "github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
)

// sourceRefIndexKey indexes ManifestGenerators by `<kind>/<ns>/<name>`
// of every referenced source, so source-change events can be fanned
// out efficiently.
const sourceRefIndexKey string = ".metadata.sourceRef"

// valuesFromRefIndexKey indexes ManifestGenerators by `<ns>/<name>` of
// every referenced ConfigMap in spec.valuesFrom, so a ConfigMap change
// only re-enqueues the generators that actually consume it.
const valuesFromRefIndexKey string = ".metadata.valuesFromRef"

// ManifestGeneratorReconcilerOptions configures the controller
// builder; mirrors source-watcher's pattern.
type ManifestGeneratorReconcilerOptions struct {
	RateLimiter workqueue.TypedRateLimiter[reconcile.Request]
}

// SetupWithManager registers the reconciler and its source watches.
func (r *ManifestGeneratorReconciler) SetupWithManager(ctx context.Context,
	mgr ctrl.Manager,
	opts ManifestGeneratorReconcilerOptions) error {
	if err := mgr.GetCache().IndexField(ctx,
		&mgapi.ManifestGenerator{},
		sourceRefIndexKey,
		r.indexBySourceRef); err != nil {
		return fmt.Errorf("failed to set index field %q: %w", sourceRefIndexKey, err)
	}
	if err := mgr.GetCache().IndexField(ctx,
		&mgapi.ManifestGenerator{},
		valuesFromRefIndexKey,
		r.indexByValuesFromRef); err != nil {
		return fmt.Errorf("failed to set index field %q: %w", valuesFromRefIndexKey, err)
	}

	// ConfigMaps are watched metadata-only so the project's
	// no-cache-for-ConfigMaps policy (cmd/main.go Client.Cache.DisableFor)
	// is preserved: the change-event informer only carries object
	// metadata, and the reconciler re-fetches the live payload
	// through the APIReader when it actually needs the data.
	configMapMeta := &metav1.PartialObjectMetadata{}
	configMapMeta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))

	return ctrl.NewControllerManagedBy(mgr).
		For(&mgapi.ManifestGenerator{},
			builder.WithPredicates(
				predicate.Or(
					predicate.GenerationChangedPredicate{},
					gotkpredicates.ReconcileRequestedPredicate{},
				),
			)).
		Watches(
			&sourcev1.GitRepository{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForSourceChange),
			builder.WithPredicates(sourceChangePredicate),
		).
		Watches(
			&sourcev1.OCIRepository{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForSourceChange),
			builder.WithPredicates(sourceChangePredicate),
		).
		Watches(
			&sourcev1.Bucket{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForSourceChange),
			builder.WithPredicates(sourceChangePredicate),
		).
		Watches(
			&sourcev1.HelmChart{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForSourceChange),
			builder.WithPredicates(sourceChangePredicate),
		).
		Watches(
			&sourcev1.ExternalArtifact{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForSourceChange),
			builder.WithPredicates(sourceChangePredicate),
		).
		Watches(
			configMapMeta,
			handler.EnqueueRequestsFromMapFunc(r.requestsForConfigMapChange),
			builder.WithPredicates(configMapChangePredicate),
		).
		WithOptions(controller.Options{RateLimiter: opts.RateLimiter}).
		Complete(r)
}

// requestsForSourceChange enqueues every ManifestGenerator that
// references the changed source.
func (r *ManifestGeneratorReconciler) requestsForSourceChange(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrl.LoggerFrom(ctx)
	source, ok := obj.(sourcev1.Source)
	if !ok {
		log.Error(fmt.Errorf("expected Source, got %T", obj), "failed to enqueue source change")
		return nil
	}
	if source.GetArtifact() == nil {
		return nil
	}

	gvk, err := r.GroupVersionKindFor(obj)
	if err != nil {
		log.Error(err, "failed to get GVK for source change")
		return nil
	}

	var list mgapi.ManifestGeneratorList
	if err := r.List(ctx, &list, client.MatchingFields{
		sourceRefIndexKey: fmt.Sprintf("%s/%s", gvk.Kind, client.ObjectKeyFromObject(obj).String()),
	}); err != nil {
		log.Error(err, "failed to list ManifestGenerators for source change")
		return nil
	}

	reqs := make([]reconcile.Request, len(list.Items))
	for i, mg := range list.Items {
		reqs[i].NamespacedName = types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}
	}
	return reqs
}

// indexBySourceRef indexes a ManifestGenerator by every source
// reference in its spec.
func (r *ManifestGeneratorReconciler) indexBySourceRef(o client.Object) []string {
	mg, ok := o.(*mgapi.ManifestGenerator)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(mg.Spec.Sources))
	for _, src := range mg.Spec.Sources {
		ns := src.Namespace
		if ns == "" {
			ns = mg.Namespace
		}
		keys = append(keys, fmt.Sprintf("%s/%s/%s", src.Kind, ns, src.Name))
	}
	return keys
}

// indexByValuesFromRef indexes a ManifestGenerator by every ConfigMap
// reference in spec.valuesFrom.
func (r *ManifestGeneratorReconciler) indexByValuesFromRef(o client.Object) []string {
	mg, ok := o.(*mgapi.ManifestGenerator)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(mg.Spec.ValuesFrom))
	for _, ref := range mg.Spec.ValuesFrom {
		ns := ref.Namespace
		if ns == "" {
			ns = mg.Namespace
		}
		keys = append(keys, fmt.Sprintf("%s/%s", ns, ref.Name))
	}
	return keys
}

// requestsForConfigMapChange enqueues every ManifestGenerator that
// references the changed ConfigMap via spec.valuesFrom.
func (r *ManifestGeneratorReconciler) requestsForConfigMapChange(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrl.LoggerFrom(ctx)

	var list mgapi.ManifestGeneratorList
	if err := r.List(ctx, &list, client.MatchingFields{
		valuesFromRefIndexKey: client.ObjectKeyFromObject(obj).String(),
	}); err != nil {
		log.Error(err, "failed to list ManifestGenerators for ConfigMap change")
		return nil
	}

	reqs := make([]reconcile.Request, len(list.Items))
	for i, mg := range list.Items {
		reqs[i].NamespacedName = types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}
	}
	return reqs
}

// configMapChangePredicate filters out non-data ConfigMap updates so
// label-only / annotation-only churn does not trigger reconciles. The
// metadata-only watch hands us PartialObjectMetadata payloads, so we
// can only compare resource versions — but a resource-version change
// is exactly the signal we want (any update touches it).
var configMapChangePredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld == nil || e.ObjectNew == nil {
			return false
		}
		return e.ObjectOld.GetResourceVersion() != e.ObjectNew.GetResourceVersion()
	},
}

// sourceChangePredicate fires only when the source artifact revision
// has actually moved forward.
var sourceChangePredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldSrc, ok := e.ObjectOld.(sourcev1.Source)
		if !ok {
			return false
		}
		newSrc, ok := e.ObjectNew.(sourcev1.Source)
		if !ok {
			return false
		}
		if newSrc.GetArtifact() == nil {
			return false
		}
		if oldSrc.GetArtifact() == nil {
			return true
		}
		return !oldSrc.GetArtifact().HasRevision(newSrc.GetArtifact().Revision)
	},
}
