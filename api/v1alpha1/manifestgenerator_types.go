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

package v1alpha1

import (
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gotkmeta "github.com/fluxcd/pkg/apis/meta"
)

// Constants that are part of the wire format. Renaming any of these is a
// breaking API change.
const (
	// ManifestGeneratorKind is the kind name reported in TypeMeta and used
	// by ownerReferences.
	ManifestGeneratorKind = "ManifestGenerator"

	// Finalizer is set on a ManifestGenerator while it owns generated
	// ExternalArtifacts. Removed only after orphan cleanup succeeds.
	Finalizer = "manifests.fluxcd.tooling/finalizer"

	// ManifestGeneratorLabel is applied to every generated ExternalArtifact
	// so the owner can be discovered via label selector for GC.
	ManifestGeneratorLabel = "manifests.fluxcd.tooling/generator"

	// ArtifactOriginRevisionAnnotation is the standard OCI annotation key
	// used to record the upstream revision of generated artifacts.
	ArtifactOriginRevisionAnnotation = "org.opencontainers.image.revision"

	// ReconcileAnnotation pauses reconciliation when set to DisabledValue.
	ReconcileAnnotation = "manifests.fluxcd.tooling/reconcile"

	EnabledValue  = "enabled"
	DisabledValue = "disabled"

	// Reason values surfaced in Ready conditions and events.
	ReconciliationDisabledReason = "ReconciliationDisabled"
	AccessDeniedReason           = "AccessDenied"
	ValidationFailedReason       = "ValidationFailed"
	SourceFetchFailedReason      = "SourceFetchFailed"
	PipelineFailedReason         = "PipelineFailed"
	RenderFailedReason           = "RenderFailed"
	BuildFailedReason            = "BuildFailed"
	StorageOperationFailedReason = "StorageOperationFailed"
)

// Source kinds accepted in `spec.sources[].kind`. Tracked here so the
// controller and validation share a single source of truth.
const (
	SourceKindBucket           = "Bucket"
	SourceKindGitRepository    = "GitRepository"
	SourceKindOCIRepository    = "OCIRepository"
	SourceKindHelmChart        = "HelmChart"
	SourceKindExternalArtifact = "ExternalArtifact"
)

// Load formats accepted in `pipeline[].load.format`.
const (
	LoadFormatYAML = "yaml"
	LoadFormatJSON = "json"
	LoadFormatText = "text"
	LoadFormatRaw  = "raw"
)

// Load output shapes accepted in `pipeline[].load.as`.
const (
	LoadAsList = "list"
	LoadAsMap  = "map"
)

// ManifestGeneratorSpec defines the desired state of a ManifestGenerator.
type ManifestGeneratorSpec struct {
	// Interval at which to reconcile the ManifestGenerator. The actual
	// interval is jittered to spread load across many objects. Parsing
	// follows Go's time.ParseDuration via metav1.Duration, matching the
	// behaviour of every other Flux API; no extra pattern is enforced.
	// +required
	Interval metav1.Duration `json:"interval"`

	// Suspend tells the controller to stop reconciling this object.
	// Existing ExternalArtifacts are not touched while suspended.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Sources is the list of Flux source-controller resources whose
	// artifacts provide input files for the pipeline and templates. Each
	// entry is referenced elsewhere in the spec by `@alias`.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	// +required
	Sources []SourceReference `json:"sources"`

	// Values is an inline data tree merged into the root `.Values`
	// namespace that pipeline expressions and templates can read.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// ValuesFrom selects ConfigMaps whose data is merged into the
	// `.Values` namespace. Entries are merged in order; inline `values`
	// override anything injected via `valuesFrom`. Secret references are
	// intentionally not supported in v1alpha1.
	// +kubebuilder:validation:MaxItems=100
	// +optional
	ValuesFrom []ValuesReference `json:"valuesFrom,omitempty"`

	// Pipeline is the list of declarative data-shaping steps. Each step
	// produces a single named output that later steps reference by name;
	// ordering is implied by data dependency, not list position.
	// +kubebuilder:validation:MaxItems=1000
	// +optional
	Pipeline []PipelineStep `json:"pipeline,omitempty"`

	// Artifacts is the list of ExternalArtifacts the controller publishes.
	// Each entry produces exactly one ExternalArtifact tarball.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	// +required
	Artifacts []ManifestArtifact `json:"artifacts"`
}

// SourceReference identifies a Flux source-controller object by alias.
type SourceReference struct {
	// Alias is the local name used to reference this source elsewhere in
	// the spec via `@<alias>`. Must be unique per ManifestGenerator.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	Alias string `json:"alias"`

	// Name of the referenced source object.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Namespace of the referenced source object. Defaults to the
	// ManifestGenerator's namespace when empty.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Kind of the referenced source object.
	// +kubebuilder:validation:Enum=Bucket;GitRepository;OCIRepository;HelmChart;ExternalArtifact
	// +required
	Kind string `json:"kind"`
}

// ValuesReference selects a ConfigMap as a source of values data.
type ValuesReference struct {
	// Kind of the values source. Only ConfigMap is supported in v1alpha1.
	// +kubebuilder:validation:Enum=ConfigMap
	// +required
	Kind string `json:"kind"`

	// Name of the ConfigMap.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Namespace of the ConfigMap. Defaults to the ManifestGenerator's
	// namespace when empty.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ValuesKey selects a single key inside the ConfigMap data; its
	// value is parsed as YAML and merged. When empty, every key in
	// `data` is injected as a string keyed by its name.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ValuesKey string `json:"valuesKey,omitempty"`

	// TargetPath is the dot-separated path within `.Values` where the
	// injected data is placed. Empty means merge at the root of `.Values`.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	TargetPath string `json:"targetPath,omitempty"`

	// Optional marks the reference as non-fatal when the ConfigMap or
	// the referenced key cannot be resolved.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// PipelineStep is one declarative data-shaping operation. Each step has
// a unique name and exactly one verb. Later steps reference earlier
// outputs by name.
// +kubebuilder:validation:XValidation:rule="[has(self.load), has(self.filter), has(self.map), has(self.group), has(self.merge)].filter(x, x).size() == 1",message="pipeline step must set exactly one of load, filter, map, group, merge"
type PipelineStep struct {
	// Name of the step. Must be unique within the pipeline and a valid
	// Go-style identifier; it becomes the key under which later steps
	// see this step's output.
	// +kubebuilder:validation:Pattern="^[a-zA-Z_][a-zA-Z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// Load reads file(s) from a source artifact.
	// +optional
	Load *LoadStep `json:"load,omitempty"`

	// Filter keeps items of a previous step matching `where`.
	// +optional
	Filter *FilterStep `json:"filter,omitempty"`

	// Map transforms each item of a previous step via `expr`.
	// +optional
	Map *MapStep `json:"map,omitempty"`

	// Group buckets a list into a map of lists by `keyExpr`.
	// +optional
	Group *GroupStep `json:"group,omitempty"`

	// Merge deep-merges multiple previous outputs into a single object.
	// +optional
	Merge *MergeStep `json:"merge,omitempty"`
}

// LoadStep reads one or more files from a source-controller artifact and
// decodes them. A single file yields one value; a glob yields a list
// (default) or a map (with `keyExpr` when `as: map`).
type LoadStep struct {
	// From is the `@alias/<glob>` reference into a source artifact.
	// +kubebuilder:validation:Pattern="^@([a-z0-9]([a-z0-9_-]*[a-z0-9])?)/(.+)$"
	// +kubebuilder:validation:MaxLength=1024
	// +required
	From string `json:"from"`

	// Format selects the file decoder.
	// +kubebuilder:validation:Enum=yaml;json;text;raw
	// +kubebuilder:default=yaml
	// +optional
	Format string `json:"format,omitempty"`

	// As selects the output collection shape when `from` is a glob.
	// Ignored when `from` resolves to a single file.
	// +kubebuilder:validation:Enum=list;map
	// +kubebuilder:default=list
	// +optional
	As string `json:"as,omitempty"`

	// KeyExpr is a Go template fragment evaluated per loaded item to
	// produce a map key. Required when `as: map`; ignored otherwise.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	KeyExpr string `json:"keyExpr,omitempty"`
}

// FilterStep keeps the items of a previous step for which `where`
// evaluates to a Go truthy value.
type FilterStep struct {
	// From references a previous pipeline step by name.
	// +kubebuilder:validation:Pattern="^[a-zA-Z_][a-zA-Z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	From string `json:"from"`

	// Where is a Go template fragment evaluated per item. Bindings:
	// `.value` (the item), `.key` (map key) or `.index` (list index), and
	// the standard root context (`.values`, prior step outputs).
	// +kubebuilder:validation:MaxLength=4096
	// +required
	Where string `json:"where"`
}

// MapStep transforms each item of a previous step.
type MapStep struct {
	// From references a previous pipeline step by name.
	// +kubebuilder:validation:Pattern="^[a-zA-Z_][a-zA-Z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	From string `json:"from"`

	// Expr is a Go template fragment evaluated per item; the rendered
	// string is parsed as YAML to form the new item value.
	// +kubebuilder:validation:MaxLength=4096
	// +required
	Expr string `json:"expr"`
}

// GroupStep groups a list into a map of lists by `keyExpr`.
type GroupStep struct {
	// From references a previous pipeline step by name.
	// +kubebuilder:validation:Pattern="^[a-zA-Z_][a-zA-Z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	From string `json:"from"`

	// KeyExpr is a Go template fragment evaluated per item to produce a
	// group key. The result is converted to a string.
	// +kubebuilder:validation:MaxLength=1024
	// +required
	KeyExpr string `json:"keyExpr"`
}

// MergeStep deep-merges a set of previous step outputs into a single
// object using Helm values merge semantics. Later entries in `from`
// override earlier entries on key collisions.
type MergeStep struct {
	// From lists the previous pipeline steps to merge, in order.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=100
	// +required
	From []string `json:"from"`
}

// ManifestArtifact describes a single ExternalArtifact to publish.
type ManifestArtifact struct {
	// Name of the generated ExternalArtifact object. Must be unique
	// across the ManifestGenerator's `spec.artifacts`.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Revision optionally mirrors the revision of a source alias. When
	// unset, the controller computes a content-addressed revision from
	// the rendered tarball.
	// +kubebuilder:validation:Pattern="^@([a-z0-9]([a-z0-9_-]*[a-z0-9])?)$"
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Revision string `json:"revision,omitempty"`

	// OriginRevision sets the `org.opencontainers.image.revision`
	// annotation on the generated artifact metadata when the referenced
	// source alias has an origin revision. Ignored otherwise.
	// +kubebuilder:validation:Pattern="^@([a-z0-9]([a-z0-9_-]*[a-z0-9])?)$"
	// +kubebuilder:validation:MaxLength=64
	// +optional
	OriginRevision string `json:"originRevision,omitempty"`

	// ForEach fans the templates across the entries of a pipeline value
	// (a list or map). Each iteration evaluates `templates[].to` with
	// the iteration bindings in scope so each item writes to a distinct
	// output path.
	// +optional
	ForEach *ForEachSpec `json:"forEach,omitempty"`

	// Templates is the ordered list of template files rendered into the
	// artifact tarball.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	// +required
	Templates []TemplateSpec `json:"templates"`
}

// ForEachSpec iterates over the entries of a pipeline output, binding
// each item to a name available in templates as `.<as>`.
type ForEachSpec struct {
	// From references a pipeline step by name.
	// +kubebuilder:validation:Pattern="^[a-zA-Z_][a-zA-Z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	From string `json:"from"`

	// As is the binding name made available to templates. The bound
	// value is `{key, value}` when iterating a map and `{index, value}`
	// when iterating a list.
	// +kubebuilder:validation:Pattern="^[a-zA-Z_][a-zA-Z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	As string `json:"as"`
}

// TemplateSpec describes one template render within an artifact.
type TemplateSpec struct {
	// From is the `@alias/<path>` reference to the template file inside
	// a source artifact. `@artifact/...` is not valid here (templates
	// are inputs, not outputs).
	// +kubebuilder:validation:Pattern="^@([a-z0-9]([a-z0-9_-]*[a-z0-9])?)/(.+)$"
	// +kubebuilder:validation:MaxLength=1024
	// +required
	From string `json:"from"`

	// To is the path within the output artifact tarball; must begin
	// with `@artifact/`. May contain Go template fragments expanded
	// against the current iteration bindings when `forEach` is set.
	// +kubebuilder:validation:Pattern="^@(artifact)/(.+)$"
	// +kubebuilder:validation:MaxLength=1024
	// +required
	To string `json:"to"`
}

// ManifestGeneratorStatus defines the observed state of a ManifestGenerator.
type ManifestGeneratorStatus struct {
	gotkmeta.ReconcileRequestStatus `json:",inline"`

	// Conditions holds the readiness conditions of the object.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled generation of the spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Inventory is the list of ExternalArtifacts currently owned by this
	// ManifestGenerator; used to garbage-collect orphans on spec changes.
	// +optional
	Inventory []ExternalArtifactReference `json:"inventory,omitempty"`

	// ObservedSourcesDigest is a stable hash of the source revisions
	// observed during the most recent successful reconcile.
	// +optional
	ObservedSourcesDigest string `json:"observedSourcesDigest,omitempty"`
}

// ExternalArtifactReference identifies a generated ExternalArtifact by
// name/namespace and records its content digest and filename.
type ExternalArtifactReference struct {
	// Name of the referenced ExternalArtifact.
	// +required
	Name string `json:"name"`

	// Namespace of the referenced ExternalArtifact.
	// +required
	Namespace string `json:"namespace"`

	// Digest of the artifact content.
	// +required
	Digest string `json:"digest"`

	// Filename of the artifact tarball.
	// +required
	Filename string `json:"filename"`
}

// GetConditions returns the status conditions of the object.
func (in *ManifestGenerator) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (in *ManifestGenerator) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// GetRequeueAfter returns the duration after which the ManifestGenerator
// must be reconciled again. Falls back to one hour when interval is unset
// (which should not happen given the required field, but is defensive).
func (in *ManifestGenerator) GetRequeueAfter() time.Duration {
	if in.Spec.Interval.Duration > 0 {
		return in.Spec.Interval.Duration
	}
	return time.Hour
}

// SetLastHandledReconcileAt records the timestamp of the last manual
// reconcile request handled by the controller.
func (in *ManifestGenerator) SetLastHandledReconcileAt(value string) {
	in.Status.LastHandledReconcileAt = value
}

// IsDisabled reports whether reconciliation is currently paused, either
// via `spec.suspend` or via the reconcile annotation.
func (in *ManifestGenerator) IsDisabled() bool {
	if in.Spec.Suspend {
		return true
	}
	val, ok := in.GetAnnotations()[ReconcileAnnotation]
	return ok && strings.ToLower(val) == DisabledValue
}

// HasArtifactInInventory reports whether the inventory already contains
// an entry matching the given name/namespace/digest.
func (in *ManifestGenerator) HasArtifactInInventory(name, namespace, digest string) bool {
	for _, ref := range in.Status.Inventory {
		if ref.Name == name && ref.Namespace == namespace && ref.Digest == digest {
			return true
		}
	}
	return false
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mg
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""

// ManifestGenerator is the schema for the manifestgenerators API.
type ManifestGenerator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManifestGeneratorSpec   `json:"spec,omitempty"`
	Status ManifestGeneratorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ManifestGeneratorList contains a list of ManifestGenerator.
type ManifestGeneratorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManifestGenerator `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManifestGenerator{}, &ManifestGeneratorList{})
}
