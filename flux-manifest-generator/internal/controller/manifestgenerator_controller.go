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

// Package controller hosts the ManifestGenerator reconciler. The
// reconciler observes referenced source-controller artifacts, fetches
// them locally, assembles output tarballs via internal/builder, and
// publishes one ExternalArtifact per spec.artifacts entry.
package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	kuberecorder "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	eventv1 "github.com/fluxcd/pkg/apis/event/v1beta1"
	gotkmeta "github.com/fluxcd/pkg/apis/meta"
	gotkstorage "github.com/fluxcd/pkg/artifact/storage"
	gotkfetch "github.com/fluxcd/pkg/http/fetch"
	gotkconditions "github.com/fluxcd/pkg/runtime/conditions"
	gotkjitter "github.com/fluxcd/pkg/runtime/jitter"
	gotkpatch "github.com/fluxcd/pkg/runtime/patch"
	gotktar "github.com/fluxcd/pkg/tar"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/builder"
	"github.com/tvanderpool/flux-manifest-generator/internal/render"
)

// ManifestGeneratorReconciler reconciles a ManifestGenerator object.
type ManifestGeneratorReconciler struct {
	client.Client
	kuberecorder.EventRecorder

	ControllerName            string
	Scheme                    *runtime.Scheme
	Storage                   *gotkstorage.Storage
	APIReader                 client.Reader
	ArtifactFetchRetries      int
	DependencyRequeueInterval time.Duration
	NoCrossNamespaceRefs      bool

	// Engine renders artifact templates. Optional; defaults to
	// render.NewGoEngine() the first time a reconcile needs it so
	// callers that do not set it (most tests, the manager binary's
	// default wiring) get a working renderer for free.
	Engine render.Engine
}

// observedSource is the runtime view of a referenced source artifact;
// kept private to the controller until pipeline code needs it.
type observedSource struct {
	Digest         string
	Revision       string
	URL            string
	OriginRevision string
}

// +kubebuilder:rbac:groups=manifests.fluxcd.tooling,resources=manifestgenerators,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=manifests.fluxcd.tooling,resources=manifestgenerators/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=manifests.fluxcd.tooling,resources=manifestgenerators/finalizers,verbs=update
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a ManifestGenerator towards its desired state.
func (r *ManifestGeneratorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)

	obj := &mgapi.ManifestGenerator{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patcher := gotkpatch.NewSerialPatcher(obj, r.Client)

	defer func() {
		if err := r.summarizeStatus(ctx, obj, patcher); err != nil {
			log.Error(err, "failed to update status")
			retErr = kerrors.NewAggregate([]error{retErr, err})
		}
	}()

	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, obj)
	}

	if !controllerutil.ContainsFinalizer(obj, mgapi.Finalizer) {
		log.Info("Adding finalizer", "finalizer", mgapi.Finalizer)
		return r.addFinalizer(obj)
	}

	if obj.IsDisabled() {
		log.Error(errors.New("can't reconcile"), msgReconciliationDisabled)
		r.Event(obj, eventv1.EventTypeTrace, mgapi.ReconciliationDisabledReason, msgReconciliationDisabled)
		return ctrl.Result{}, nil
	}

	if err := r.validateSpec(obj); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcile(ctx, obj, patcher)
}

func (r *ManifestGeneratorReconciler) reconcile(ctx context.Context,
	obj *mgapi.ManifestGenerator,
	patcher *gotkpatch.SerialPatcher) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	oldObj := obj.DeepCopy()

	tmpDir, err := builder.MkdirTempAbs("", "mg-")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create tmp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			log.Error(err, "failed to remove tmp dir", "dir", tmpDir)
		}
	}()

	gotkconditions.MarkReconciling(obj, gotkmeta.ProgressingReason, "%s", msgInProgress)
	gotkconditions.MarkUnknown(obj, gotkmeta.ReadyCondition, gotkmeta.ProgressingReason, "%s", msgInProgress)
	if err := r.patchStatus(ctx, obj, patcher); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	remoteSources, err := r.observeSources(ctx, obj)
	if err != nil {
		msg := fmt.Sprintf("get sources failed: %s", err.Error())
		gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, mgapi.SourceFetchFailedReason, "%s", msg)
		r.Event(obj, corev1.EventTypeWarning, mgapi.SourceFetchFailedReason, msg)
		log.Error(err, "failed to get sources, retrying")
		return ctrl.Result{RequeueAfter: r.DependencyRequeueInterval}, nil
	}

	observedSourcesDigest := hashObservedSources(remoteSources)

	localSources, err := r.fetchSources(ctx, remoteSources, tmpDir)
	if err != nil {
		msg := fmt.Sprintf("fetch sources failed: %s", err.Error())
		gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, mgapi.SourceFetchFailedReason, "%s", msg)
		r.Event(obj, corev1.EventTypeWarning, mgapi.SourceFetchFailedReason, msg)
		log.Error(err, "failed to fetch sources, retrying")
		return ctrl.Result{RequeueAfter: r.DependencyRequeueInterval}, nil
	}

	pipelineOutputs, err := r.runPipeline(ctx, obj, localSources)
	if err != nil {
		msg := fmt.Sprintf("pipeline failed: %s", err.Error())
		gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, mgapi.PipelineFailedReason, "%s", msg)
		r.Event(obj, corev1.EventTypeWarning, mgapi.PipelineFailedReason, msg)
		log.Error(err, "pipeline evaluation failed")
		return ctrl.Result{}, err
	}

	// Pipeline outputs become the top-level template scope so a
	// template can read a step's value as `.<stepName>`. Slices 7/8
	// will fold in forEach bindings and Values/ValuesFrom on top of
	// this same map before handing it to the builder.
	templateData := make(map[string]any, len(pipelineOutputs))
	for k, v := range pipelineOutputs {
		templateData[k] = v
	}

	eaRefs := make([]mgapi.ExternalArtifactReference, 0, len(obj.Spec.Artifacts))
	if r.Engine == nil {
		r.Engine = render.NewGoEngine()
	}
	artifactBuilder := builder.New(r.Storage, r.Engine)

	for i := range obj.Spec.Artifacts {
		spec := &obj.Spec.Artifacts[i]
		artifact, err := artifactBuilder.Build(ctx, spec, localSources, templateData, obj.Namespace, tmpDir)
		if err != nil {
			reason := mgapi.BuildFailedReason
			var renderErr *render.Error
			if errors.As(err, &renderErr) {
				reason = mgapi.RenderFailedReason
			}
			msg := fmt.Sprintf("%s build failed: %s", spec.Name, err.Error())
			gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, reason, "%s", msg)
			r.Event(obj, corev1.EventTypeWarning, reason, msg)
			return ctrl.Result{}, err
		}

		r.setArtifactRevisions(artifact, spec, remoteSources)

		eaRef, err := r.reconcileExternalArtifact(ctx, obj, spec, artifact)
		if err != nil {
			msg := fmt.Sprintf("%s reconcile failed: %s", spec.Name, err.Error())
			gotkconditions.MarkFalse(obj, gotkmeta.ReadyCondition, gotkmeta.ReconciliationFailedReason, "%s", msg)
			r.Event(obj, corev1.EventTypeWarning, gotkmeta.ReconciliationFailedReason, msg)
			return ctrl.Result{}, err
		}
		eaRefs = append(eaRefs, *eaRef)
	}

	if orphans := findOrphanedReferences(obj.Status.Inventory, eaRefs); len(orphans) > 0 {
		r.finalizeExternalArtifacts(ctx, orphans)
	}

	for _, eaRef := range eaRefs {
		storagePath := gotkstorage.ArtifactPath(sourcev1.ExternalArtifactKind, eaRef.Namespace, eaRef.Name, "*")
		delFiles, err := r.Storage.GarbageCollect(ctx, gotkmeta.Artifact{Path: storagePath}, 5*time.Minute)
		if err != nil {
			log.Error(err, "failed to garbage collect artifacts", "path", storagePath)
		} else if len(delFiles) > 0 {
			log.Info(fmt.Sprintf("garbage collected %d old artifact(s)", len(delFiles)), "artifacts", delFiles)
		}
	}

	obj.Status.Inventory = eaRefs
	obj.Status.ObservedSourcesDigest = observedSourcesDigest
	obj.Status.ObservedGeneration = obj.GetGeneration()

	msg := fmt.Sprintf("reconciliation succeeded, generated %d artifact(s)", len(eaRefs))
	gotkconditions.MarkTrue(obj, gotkmeta.ReadyCondition, gotkmeta.SucceededReason, "%s", msg)
	r.Event(obj, eventv1.EventTypeTrace, gotkmeta.ReadyCondition, msg)

	r.notify(oldObj, obj, eaRefs)

	return ctrl.Result{RequeueAfter: gotkjitter.JitteredIntervalDuration(obj.GetRequeueAfter())}, nil
}

// notify emits a normal event when at least one ExternalArtifact in
// the new inventory is new or has changed digest.
func (r *ManifestGeneratorReconciler) notify(oldObj, newObj *mgapi.ManifestGenerator, eaRefs []mgapi.ExternalArtifactReference) {
	changed := make([]string, 0)
	for _, eaRef := range eaRefs {
		if !oldObj.HasArtifactInInventory(eaRef.Name, eaRef.Namespace, eaRef.Digest) {
			changed = append(changed, fmt.Sprintf("%s/%s (%s)", eaRef.Namespace, eaRef.Name, eaRef.Digest))
		}
	}
	if len(changed) > 0 {
		msg := fmt.Sprintf("external artifacts reconciled: %s", strings.Join(changed, "\n"))
		r.Event(newObj, corev1.EventTypeNormal, gotkmeta.ReadyCondition, msg)
	}
}

// observeSources resolves every source reference and returns its
// current artifact metadata keyed by alias.
func (r *ManifestGeneratorReconciler) observeSources(ctx context.Context,
	obj *mgapi.ManifestGenerator) (map[string]observedSource, error) {
	out := make(map[string]observedSource)

	for _, src := range obj.Spec.Sources {
		key := client.ObjectKey{Name: src.Name, Namespace: obj.Namespace}
		if src.Namespace != "" {
			key.Namespace = src.Namespace
		}

		var source sourcev1.Source
		switch src.Kind {
		case mgapi.SourceKindGitRepository:
			gr := &sourcev1.GitRepository{}
			if err := r.Get(ctx, key, gr); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, err
				}
				return nil, fmt.Errorf("unable to get source %q: %w", key, err)
			}
			source = gr
		case mgapi.SourceKindOCIRepository:
			or := &sourcev1.OCIRepository{}
			if err := r.Get(ctx, key, or); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, err
				}
				return nil, fmt.Errorf("unable to get source %q: %w", key, err)
			}
			source = or
		case mgapi.SourceKindBucket:
			b := &sourcev1.Bucket{}
			if err := r.Get(ctx, key, b); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, err
				}
				return nil, fmt.Errorf("unable to get source %q: %w", key, err)
			}
			source = b
		case mgapi.SourceKindHelmChart:
			hc := &sourcev1.HelmChart{}
			if err := r.Get(ctx, key, hc); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, err
				}
				return nil, fmt.Errorf("unable to get source %q: %w", key, err)
			}
			source = hc
		case mgapi.SourceKindExternalArtifact:
			ea := &sourcev1.ExternalArtifact{}
			if err := r.Get(ctx, key, ea); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, err
				}
				return nil, fmt.Errorf("unable to get source %q: %w", key, err)
			}
			source = ea
		default:
			return nil, fmt.Errorf("source %q kind %q not supported", src.Name, src.Kind)
		}

		artifact := source.GetArtifact()
		if artifact == nil {
			return nil, fmt.Errorf("source %q is not ready", key)
		}

		os := observedSource{
			Digest:   artifact.Digest,
			Revision: artifact.Revision,
			URL:      artifact.URL,
		}
		if origin, ok := artifact.Metadata[mgapi.ArtifactOriginRevisionAnnotation]; ok {
			os.OriginRevision = origin
		}
		out[src.Alias] = os
	}

	return out, nil
}

// fetchSources downloads each observed source artifact to its own
// alias-named subdirectory of tmpDir and returns the alias→dir map.
func (r *ManifestGeneratorReconciler) fetchSources(ctx context.Context,
	sources map[string]observedSource,
	tmpDir string) (map[string]string, error) {
	dirs := make(map[string]string, len(sources))
	for alias, src := range sources {
		srcDir := filepath.Join(tmpDir, alias)
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create source dir: %w", err)
		}

		fetcher := gotkfetch.New(
			gotkfetch.WithLogger(ctrl.LoggerFrom(ctx)),
			gotkfetch.WithRetries(r.ArtifactFetchRetries),
			gotkfetch.WithMaxDownloadSize(gotktar.UnlimitedUntarSize),
			gotkfetch.WithUntar(gotktar.WithMaxUntarSize(gotktar.UnlimitedUntarSize)),
			gotkfetch.WithHostnameOverwrite(os.Getenv("SOURCE_CONTROLLER_LOCALHOST")),
		)
		if err := fetcher.FetchWithContext(ctx, src.URL, src.Digest, srcDir); err != nil {
			return nil, fmt.Errorf("fetch %q: %w", alias, err)
		}
		dirs[alias] = srcDir
	}
	return dirs, nil
}

// reconcileExternalArtifact applies the ExternalArtifact server-side
// and patches its status with the freshly produced artifact.
func (r *ManifestGeneratorReconciler) reconcileExternalArtifact(ctx context.Context,
	obj *mgapi.ManifestGenerator,
	spec *mgapi.ManifestArtifact,
	artifact *gotkmeta.Artifact) (*mgapi.ExternalArtifactReference, error) {
	log := ctrl.LoggerFrom(ctx)

	labels := map[string]string{
		"app.kubernetes.io/managed-by": r.ControllerName,
		mgapi.ManifestGeneratorLabel:   string(obj.GetUID()),
	}

	ea := &sourcev1.ExternalArtifact{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sourcev1.GroupVersion.String(),
			Kind:       sourcev1.ExternalArtifactKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: obj.Namespace,
			Labels:    labels,
		},
		Spec: sourcev1.ExternalArtifactSpec{
			SourceRef: &gotkmeta.NamespacedObjectKindReference{
				APIVersion: mgapi.GroupVersion.String(),
				Kind:       mgapi.ManifestGeneratorKind,
				Name:       obj.Name,
				Namespace:  obj.Namespace,
			},
		},
	}

	force := true
	if err := r.Patch(ctx, ea, client.Apply, &client.PatchOptions{
		FieldManager: r.ControllerName,
		Force:        &force,
	}); err != nil {
		return nil, fmt.Errorf("failed to apply ExternalArtifact: %w", err)
	}

	ea.ManagedFields = nil
	ea.Status = sourcev1.ExternalArtifactStatus{
		Artifact: artifact,
		Conditions: []metav1.Condition{
			{
				ObservedGeneration: ea.GetGeneration(),
				Type:               gotkmeta.ReadyCondition,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             gotkmeta.SucceededReason,
				Message:            "Artifact is ready",
			},
		},
	}
	statusOpts := &client.SubResourcePatchOptions{
		PatchOptions: client.PatchOptions{FieldManager: r.ControllerName},
	}
	if err := r.Status().Patch(ctx, ea, client.Apply, statusOpts); err != nil {
		return nil, fmt.Errorf("failed to patch ExternalArtifact status: %w", err)
	}

	if obj.HasArtifactInInventory(ea.Name, ea.Namespace, artifact.Digest) {
		log.Info(fmt.Sprintf("%s/%s/%s is up to date", ea.Kind, ea.Namespace, ea.Name))
	} else {
		msg := fmt.Sprintf("%s/%s/%s reconciled with revision %s", ea.Kind, ea.Namespace, ea.Name, artifact.Revision)
		log.Info(msg)
		r.Event(obj, eventv1.EventTypeTrace, gotkmeta.ReadyCondition, msg)
	}

	return &mgapi.ExternalArtifactReference{
		Name:      ea.Name,
		Namespace: ea.Namespace,
		Digest:    artifact.Digest,
		Filename:  filepath.Base(artifact.Path),
	}, nil
}

// findOrphanedReferences returns inventory entries no longer present
// in the current set of references.
func findOrphanedReferences(inventory, current []mgapi.ExternalArtifactReference) []mgapi.ExternalArtifactReference {
	currentSet := make(map[string]struct{}, len(current))
	for _, ref := range current {
		currentSet[fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)] = struct{}{}
	}
	var orphaned []mgapi.ExternalArtifactReference
	for _, ref := range inventory {
		if _, ok := currentSet[fmt.Sprintf("%s/%s", ref.Namespace, ref.Name)]; !ok {
			orphaned = append(orphaned, ref)
		}
	}
	return orphaned
}

// setArtifactRevisions overrides the artifact revision and origin
// metadata when the spec mirrors a source alias.
func (r *ManifestGeneratorReconciler) setArtifactRevisions(artifact *gotkmeta.Artifact,
	spec *mgapi.ManifestArtifact,
	remoteSources map[string]observedSource) {
	if spec.Revision != "" {
		if rs, ok := remoteSources[strings.TrimPrefix(spec.Revision, "@")]; ok {
			artifact.Revision = rs.Revision
		}
	}
	if spec.OriginRevision != "" {
		if artifact.Metadata == nil {
			artifact.Metadata = make(map[string]string)
		}
		if rs, ok := remoteSources[strings.TrimPrefix(spec.OriginRevision, "@")]; ok {
			if rs.OriginRevision != "" {
				artifact.Metadata[mgapi.ArtifactOriginRevisionAnnotation] = rs.OriginRevision
			} else {
				artifact.Metadata[mgapi.ArtifactOriginRevisionAnnotation] = rs.Revision
			}
		}
	}
}

// hashObservedSources computes a deterministic SHA-256 over the
// observed source set so revisions trigger drift even when a single
// alias's revision changes.
func hashObservedSources(sources map[string]observedSource) string {
	parts := make([]string, 0, len(sources))
	for alias, s := range sources {
		parts = append(parts, fmt.Sprintf("%s=[digest=%s,revision=%s,url=%s]", alias, s.Digest, s.Revision, s.URL))
	}
	sort.Strings(parts)
	h := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("sha256:%x", h)
}
