# flux-manifest-generator — design and roadmap

This document is the design-of-record for the `ManifestGenerator` CRD
and its controller. It supersedes any earlier informal discussion. Keep
it updated as decisions evolve.

A fresh agent or contributor should be able to pick up the project by
reading, in order: [`README.md`](../README.md), [`AGENTS.md`](../AGENTS.md),
this file. Anything not yet decided is called out in **Open questions**.

## 1. Problem statement

We need a Flux controller that:

1. Loads structured data from files inside source-controller artifacts
   (`GitRepository` / `OCIRepository` / `Bucket` / `HelmChart` /
   `ExternalArtifact`), referenced by `@alias`, with glob support.
2. Shapes that data through a declarative pipeline (no imperative state,
   no shell-out, no remote calls).
3. Renders the result through Go templates (Sprig + Helm-style helpers)
   into multi-document YAML and/or a directory tree of files.
4. Publishes the rendered output as an `ExternalArtifact` served from
   the controller's own HTTP endpoint, so downstream `Kustomization` /
   `HelmRelease` resources can consume it via `sourceRef`.

Driving use case: dynamic PowerDNS RRSet generation in a core cluster
from each campus cluster's `talos/cluster.yaml`, and the reverse — per-
campus zone records derived from the parent core cluster's
`cluster.yaml`. Both directions are pure data-pipeline-into-template
work that doesn't fit `ResourceSet` (no artifact output, no glob loading
of source-artifact files) or `ArtifactGenerator` (no templating, no
data-aware rules).

## 2. Comparison to existing Flux APIs

| API | Templating | Pipeline / data shaping | Publishes artifact | Inputs from files in source artifacts |
|---|---|---|---|---|
| `helm-controller` `HelmRelease` | Yes (Helm chart) | Limited | No | No |
| `flux-operator` `ResourceSet` | Yes (Go template) | No (inputs matrix only) | No (applies directly) | No |
| `source-watcher` `ArtifactGenerator` | No | No | Yes | Byte-level copy/merge only |
| **`ManifestGenerator`** | Yes | Yes | Yes | Yes |

## 3. Decisions (locked)

| # | Topic | Decision |
|---|---|---|
| 1 | Name & API group | `ManifestGenerator`, group `manifests.fluxcd.tooling`, version `v1alpha1`, shortName `mg`. Standard Flux top-level fields apply: `spec.interval` (`metav1.Duration`, required) and `spec.suspend` (`bool`, optional), with the usual jittered requeue and pause semantics. |
| 2 | Template engine | Go `text/template` + Sprig + Helm-style helpers, behind a `TemplateEngine` interface so an alternative engine can be added later without changing the CRD |
| 3 | Artifact integration | Controller serves its own `ExternalArtifact` HTTP endpoint (same pattern as `source-watcher`); does not modify `source-controller` |
| 4 | Repo layout | Project lives in `flux-manifest-generator/` subdir of a monorepo for now; will be extracted with `git-filter-repo` later |
| 5 | Input sources (v1) | `@alias` source artifacts (`GitRepository`/`OCIRepository`/`Bucket`/`HelmChart`/`ExternalArtifact`); inline `values`; `valuesFrom` `ConfigMap`. **No** `Secret` in v1 — deferred to v2. |
| 6 | Cross-namespace refs | Allowed by default; opt-in `--no-cross-namespace-refs` flag mirrors `source-watcher`'s ACL pattern |
| 7 | Pipeline verbs | `load`, `filter`, `map`, `group`, `merge`. **No** `exec`, `script`, or remote HTTP. |
| 8 | License & ownership | Apache 2.0; copyright `Travis Vanderpool`; Go module path `github.com/tvanderpool/flux-manifest-generator` |
| 9 | Go toolchain | `go 1.26.0` declared in both `go.mod` files; dev container pins matching image. All `k8s.io/*` modules aligned to a single minor (currently `v0.36.0`); `sigs.k8s.io/controller-runtime` tracks the matching release line. |
| 10 | Validation | Out of scope for v1. Rendered output is not schema-validated against discovered CRDs. Revisit in a later release if needed. |
| 11 | Ready condition reasons | Standard Flux reason set plus pipeline/render-specific reasons: `ReconciliationDisabled`, `AccessDenied`, `ValidationFailed`, `SourceFetchFailed`, `PipelineFailed`, `RenderFailed`, `BuildFailed`, `StorageOperationFailed`. Names are part of the wire format and not renamed once released. |

## 4. Architecture

```
ManifestGenerator (CRD)
    ↓ reconciler watches: GitRepository / OCIRepository / Bucket /
    ↓                     HelmChart / ExternalArtifact / ConfigMap
    ↓ fetches source artifacts via fluxcd/pkg/artifact/fetch
    ↓ runs pipeline (load → transform → render)
    ↓ writes tar.gz via fluxcd/pkg/artifact/storage
    ↓ creates/updates ExternalArtifact (owned by source-controller's API)
    ↓ serves artifact over HTTP (fluxcd/pkg/artifact/server)
Downstream Kustomization / HelmRelease references the ExternalArtifact,
exactly as it does for ArtifactGenerator.
```

Reused Flux libraries (already pinned in `go.mod`):

- `github.com/fluxcd/pkg/runtime/*` — manager scaffolding, patch helper, conditions, jitter, ACL, leader election, probes, logger, events
- `github.com/fluxcd/pkg/artifact/fetch` — pulls source tarballs from source-controller
- `github.com/fluxcd/pkg/artifact/storage` + `.../server` — local storage + HTTP server
- `github.com/fluxcd/pkg/tar` — safe extraction (path-traversal protection)
- `github.com/fluxcd/pkg/apis/meta` — shared conditions/types
- `github.com/fluxcd/source-controller/api` — `ExternalArtifact` and source kinds

## 5. CRD sketch

This sample matches the v1alpha1 type surface landed in slice 2.

```yaml
apiVersion: manifests.fluxcd.tooling/v1alpha1
kind: ManifestGenerator
metadata:
  name: campus-dns-delegations
  namespace: flux-system
spec:
  interval: 5m                      # required; parsed via metav1.Duration
  suspend: false                    # optional pause switch

  sources:                          # @alias source-controller refs
    - alias: repo
      kind: GitRepository
      name: flux-system
      namespace: flux-system

  values:                           # inline values (.Values root)
    domain: example.com
    ttl: 300

  valuesFrom:                       # ConfigMap value injection
    - kind: ConfigMap
      name: cluster-vars
      valuesKey: cluster.yaml
      targetPath: cluster

  pipeline:                         # declarative data shaping
    - name: cluster
      load:
        from: "@repo/${CLUSTER_SOURCE_PATH}/talos/cluster.yaml"
        format: yaml
    - name: campusClusters
      load:
        from: "@repo/clusters/campus/*/talos/cluster.yaml"
        format: yaml
        as: map
        keyExpr: "{{ .path | base | dir | base }}"
    - name: campusForCore
      filter:
        from: campusClusters
        where: "{{ eq .value.core $.values.cluster.name }}"

  artifacts:                        # one ExternalArtifact per entry
    - name: campus-delegations
      forEach: { from: campusForCore, as: campus }
      templates:
        - from: "@repo/templates/core/templates/delegation.yaml.tpl"
          to:   "@artifact/{{ .campus.key }}-delegation.yaml"
```

`spec.interval` uses `metav1.Duration` alone (no extra `Pattern`
validator) so any string accepted by Go's `time.ParseDuration`
parses — matching the validation surface of `HelmRelease.spec.interval`,
`ArtifactGenerator.spec.interval`, and the rest of the Flux APIs.

## 6. Pipeline semantics

- **Steps are named outputs.** Each step has a unique `name`, and later
  steps reference earlier outputs by that name. There is no implicit
  ordering — only data dependency.
- **No mutation.** Steps cannot rewrite earlier outputs. A step can only
  produce a new named output.
- **No imperative escape hatches.** No `set_fact`, `exec`, `script`,
  `http`. The only place template-language fragments are accepted in
  the pipeline is per-item expressions: `keyExpr`, `where`, and similar
  scalar-shaped fields documented per verb.
- **Identifier regex.** Step names (and `forEach.as` bindings) follow
  the Go-identifier regex `^[a-zA-Z_][a-zA-Z0-9_]*$` because they
  become keys in the template scope and have to round-trip through
  `text/template`. Source aliases (and the `@alias` path tokens that
  reference them) follow the DNS-label-like kebab-lowercase regex
  `^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$` because they appear inside
  artifact tarball paths and source-controller name strings. The two
  regexes are intentionally different and not unified.

### Verbs

| Verb | Purpose |
|---|---|
| `load` | Read file(s) from a source artifact by glob; parse as `yaml` (default), `json`, `text`, or `raw` (bytes). Single file → object; glob → list (default) or map (with `keyExpr`). |
| `filter` | Select items from a list or map by a `where:` template predicate that resolves to truthy/falsy. |
| `map` | Transform each item via an `expr:` template fragment. The fragment is rendered to a string and the rendered string is then parsed as YAML to form the new item value. (Mostly for projection / renaming; matches Helm's `tpl`-then-parse contract.) |
| `group` | Group a list by a `keyExpr:` into a map of lists. |
| `merge` | Deep-merge a set of inputs into one object. Helm values merge semantics. |

## 7. Template engine

- **Default engine:** Go `text/template` with Sprig functions plus the
  Helm-style helpers below, behind a `render.Engine` interface.
- **Helpers added on top of Sprig:**
  - `toYaml`, `fromYaml`, `toJson`, `fromJson`
  - `include` (Helm-style partial expansion)
  - `required` (fail rendering with a clear message)
  - `tpl` (recursive template expansion of a string)
  - `lookupFile` (read a file from the artifact bundle by path; used for `_helpers.tpl` siblings)
- **Per-template engine selector:** out of scope for v1; the interface
  exists so an alternative engine can be plugged in later without an
  API break.

## 8. Output

- Each entry in `spec.artifacts` produces exactly one `ExternalArtifact`.
- Within an artifact, one or more `templates` are rendered; each writes
  to a `@artifact/<path>` location in the resulting tarball.
- `forEach` fans a single template across the entries of a pipeline
  value (list or map); the output path is template-expanded so each
  iteration writes to a distinct file.
- A single template may emit multi-document YAML; both single- and
  multi-document outputs are first-class.
- Content-addressed revision computed from the final tarball, with
  `originRevision` annotation derived from the source alias (mirrors
  `ArtifactGenerator`'s semantics).

## 9. Roadmap

Each slice is a self-contained, mergeable increment. Acceptance criteria
for each slice live in the slice itself.

| # | Slice | State |
|---|---|---|
| 1 | Repo scaffolding (layout, modules, boilerplate, Makefile, empty manager binary) | Done |
| 2 | CRD + types: real `ManifestGenerator` spec (sources, values, valuesFrom, pipeline, artifacts) + status, with kubebuilder validation, deepcopy, CRD generation | **Done** |
| 3 | End-to-end source-fetch + `ExternalArtifact` publishing (pass-through copy, no pipeline yet). Proves the source-controller integration end-to-end before any pipeline logic. | **Done** |
| 4 | Pipeline verb: `load` (`yaml` / `json` / `text` / `raw`; single + glob; `as: list` / `as: map` with `keyExpr`) | **Done** |
| 5 | Pipeline verbs: `filter`, `map`, `group`, `merge` | Not started |
| 6 | Template engine interface + Go/Sprig implementation + Helm-style helpers | Not started |
| 7 | `artifacts.forEach` + multi-template artifacts + output-path templating | Not started |
| 8 | `values` + `valuesFrom` (`ConfigMap` only) | Not started |
| 9 | Helm-style partial templates (`_helpers.tpl` resolution) | Not started |

Each slice has unit tests in `internal/builder` / `internal/pipeline` /
`internal/render` (pure-Go) and envtest-backed integration tests in
`internal/controller`, mirroring `source-watcher`'s layout.

## 10. Slice 1 — done

Scaffolding in place and the manager binary builds and prints flag help
inside the dev container:

- `flux-manifest-generator/.gitignore`
- `flux-manifest-generator/LICENSE` (Apache 2.0)
- `flux-manifest-generator/PROJECT` (kubebuilder project descriptor)
- `flux-manifest-generator/README.md`, `AGENTS.md`
- `flux-manifest-generator/Makefile` (`tidy`, `fmt`, `vet`, `generate`,
  `manifests`, `manager`, `run`, `test`, `download-crd-deps`,
  `controller-gen`, `setup-envtest`)
- `flux-manifest-generator/hack/boilerplate.go.txt` (Apache 2.0 header
  injected by `controller-gen`)
- `flux-manifest-generator/go.mod` + `flux-manifest-generator/api/go.mod`
  (two-module layout; root has `replace ./api` and BLAKE3 digest pin)
- `flux-manifest-generator/api/v1alpha1/groupversion_info.go`
- `flux-manifest-generator/cmd/main.go` (manager + probes + GotK flag
  plumbing + artifact storage + HTTP server; no reconciler registered
  yet, `// +kubebuilder:scaffold:builder` placeholder is ready)
- `.devcontainer/devcontainer.json` (Go 1.26 trixie, kubectl Feature,
  kustomize via post-create script)
- `.devcontainer/post-create.sh` (installs kustomize, pre-warms
  `go mod download` in both modules, fetches `controller-gen` and
  `setup-envtest`)
- `.devcontainer/README.md`

Verified by running, inside the dev container:

```sh
cd flux-manifest-generator
go mod tidy && (cd api && go mod tidy)
make manager      # runs generate + fmt + vet, then builds ./bin/manager
./bin/manager --help
```

## 11. Slice 2 — done

`ManifestGenerator` types are complete and codegen passes. Files
touched in this slice:

- `api/v1alpha1/manifestgenerator_types.go` — full `Spec`/`Status`,
  helpers, wire-format constants
- `api/v1alpha1/zz_generated.deepcopy.go` — regenerated by
  `controller-gen object`
- `config/crd/bases/manifests.fluxcd.tooling_manifestgenerators.yaml` —
  generated CRD with kubebuilder validation embedded

### Type surface

- `ManifestGeneratorSpec` (top-level required: `interval`, `sources`,
  `artifacts`; optional: `suspend`, `values`, `valuesFrom`, `pipeline`)
- `SourceReference{alias, name, namespace?, kind}` with the kind enum
  pinned to `Bucket|GitRepository|OCIRepository|HelmChart|ExternalArtifact`
  and patterns/lengths matching `ArtifactGenerator`
- `ValuesReference{kind=ConfigMap, name, namespace?, valuesKey?, targetPath?, optional?}`
  — Secret is intentionally absent in v1alpha1 (decision §3 row 5)
- `PipelineStep{name, load? | filter? | map? | group? | merge?}` with a
  CEL `XValidation` rule forcing exactly one verb per step
  - `LoadStep{from, format=yaml|json|text|raw, as=list|map, keyExpr?}`
  - `FilterStep{from, where}`
  - `MapStep{from, expr}`
  - `GroupStep{from, keyExpr}`
  - `MergeStep{from []string, ≥2 ≤100}`
- `ManifestArtifact{name, revision?, originRevision?, forEach?, templates[]}`
  - `ForEachSpec{from, as}`
  - `TemplateSpec{from "@alias/<path>", to "@artifact/<path>"}`
- `ManifestGeneratorStatus`: embeds
  `gotkmeta.ReconcileRequestStatus` (provides `lastHandledReconcileAt`),
  plus `conditions`, `observedGeneration`, `inventory`,
  `observedSourcesDigest`. `ExternalArtifactReference{name, namespace,
  digest, filename}` matches `ArtifactGenerator`'s shape so consumers
  can share code.
- Helpers on `*ManifestGenerator`: `GetConditions`, `SetConditions`,
  `GetRequeueAfter`, `SetLastHandledReconcileAt`, `IsDisabled`
  (suspend ∨ `manifests.fluxcd.tooling/reconcile=disabled` annotation),
  `HasArtifactInInventory`.
- Wire-format constants: `ManifestGeneratorKind`, `Finalizer`,
  `ManifestGeneratorLabel`, `ArtifactOriginRevisionAnnotation`,
  `ReconcileAnnotation`, `EnabledValue`/`DisabledValue`, plus reason
  constants for ready conditions
  (`ValidationFailedReason`, `SourceFetchFailedReason`,
  `PipelineFailedReason`, `RenderFailedReason`, `BuildFailedReason`,
  `StorageOperationFailedReason`, `AccessDeniedReason`,
  `ReconciliationDisabledReason`) and source-kind / load-format /
  load-shape string constants used by the upcoming pipeline code.

### Verified

Inside the dev container:

```sh
make tidy             # tidies both modules (root + api)
make generate         # regenerates zz_generated.deepcopy.go
make manifests        # regenerates the CRD under config/crd/bases
make manager          # generate + fmt + vet + build → ./bin/manager
./bin/manager --help
```

All four commands exit zero; `./bin/manager --help` prints the GotK
flag set. Generated files (`zz_generated.deepcopy.go` and
`config/crd/bases/manifests.fluxcd.tooling_manifestgenerators.yaml`)
are committed alongside the type changes.

## 12. Slice 3 — done

End-to-end reconciliation lands as a pass-through copy that proves the
source-controller integration before any pipeline or templating
arrives. Files added in this slice:

- `internal/controller/manifestgenerator_controller.go` — reconcile
  loop, source observation, source fetch via
  `github.com/fluxcd/pkg/http/fetch`, ExternalArtifact apply + status
  patch, orphan GC, storage retention, jittered requeue, deterministic
  source-set digest hashing
- `internal/controller/manifestgenerator_manager.go` — `SetupWithManager`
  with the `.metadata.sourceRef` cache index, `predicate.Or` on
  generation + reconcile-requested annotation, watches on all five
  source kinds gated by a revision-change predicate
- `internal/controller/manifestgenerator_status.go` — `summarizeStatus`
  + `patchStatus` via `gotkpatch.SerialPatcher`, plus
  `newTerminalErrorFor` to fail fast on validation
- `internal/controller/manifestgenerator_finalize.go` — finalizer add,
  inventory-driven cleanup of ExternalArtifact objects and their
  storage entries
- `internal/controller/manifestgenerator_validation.go` — runtime
  checks beyond CRD markers: alias uniqueness, multi-tenancy ACL,
  artifact-name uniqueness, alias resolution for `revision` /
  `originRevision` / per-template `from`, plus explicit slice-3
  rejections of `pipeline` / `values` / `valuesFrom` / `forEach`
- `internal/builder/builder.go` + `internal/builder/dirhash.go` —
  pass-through copy builder. Each `templates[].from` is interpreted
  literally as a path (file or directory) inside the named source
  artifact and copied to `templates[].to` inside the staged tarball;
  the staged tree is hashed with the same Adler-32 / `dirhash` scheme
  source-watcher uses, then archived through `gotkstorage.Storage`
- `cmd/main.go` — registers the reconciler at the
  `// +kubebuilder:scaffold:builder` placeholder, wires
  `gotkevents.NewRecorder`, and threads the GotK runtime options
  (rate limiter, ACL, fetch retries, dependency requeue interval)
- `config/manager/`, `config/rbac/`, `config/crd/kustomization.yaml`,
  `config/default/`, `config/samples/` — kustomize overlays. RBAC is
  generated by `make manifests` (`role.yaml`); the static files cover
  bindings, leader-election, end-user editor/viewer roles, the
  manager Deployment + Service, the namespace, and a sample
  `ManifestGenerator` showing the slice-3 minimal spec shape
- `internal/controller_test/suite_test.go` +
  `manifestgenerator_passthrough_test.go` — first envtest integration
  test: a `GitRepository`-style artifact published through
  `fluxcd/pkg/testserver.ArtifactServer`, a `ManifestGenerator`
  referencing it, and assertions that an `ExternalArtifact` lands
  with the expected source-ref, content digest, mirrored revision,
  Ready=True status, and a stored tarball under the storage root;
  finalization removes the `ExternalArtifact`

### Slice-3 spec interpretation (temporary)

Until the pipeline and template-engine slices land, the reconciler
accepts only the minimal CRD subset:

- `spec.sources` — required, used as today
- `spec.artifacts[*].templates[*]` — `from: "@<alias>/<path>"` is
  treated as a literal copy source (file or directory); `to:
  "@artifact/<path>"` is the destination inside the tarball
- `spec.artifacts[*].revision` / `originRevision` — honored as in
  `ArtifactGenerator`

The CRD still admits `pipeline`, `values`, `valuesFrom`, and
`artifacts[*].forEach`, but the controller's validator marks the
object Stalled with `ValidationFailedReason` if any are set. Slices
4–8 will replace those rejections with real implementations.

### Verified

Inside the dev container:

```sh
make tidy fmt vet manifests generate manager test clean
kustomize build config/default
```

`make test` brings up envtest (Kubernetes 1.36), starts the artifact
test server, and exercises the reconcile loop end-to-end. `kustomize
build config/default` produces a single-namespace manifest containing
the Namespace, CRD, manager `ClusterRole` + `ClusterRoleBinding`,
leader-election `Role` + `RoleBinding`, the manager `Deployment`,
and its `Service`.

## 13. Slice 4 — done

The `load` pipeline verb lands. The reconciler now accepts
`spec.pipeline` provided every step uses `load`; the other four verbs
(`filter`, `map`, `group`, `merge`) still fail validation until their
slice arrives. Files added in this slice:

- `internal/pipeline/pipeline.go` — `Compile` / `Run` orchestration,
  `Step` interface, `Scope`, `Outputs`. Steps are evaluated in spec
  order; each step's return value lands in `Outputs[stepName]` and
  becomes visible to later steps via the template `Scope`.
- `internal/pipeline/load.go` — the `load` verb. Parses the
  `@<alias>/<glob>` reference, validates it (no `..`, no absolute
  paths), expands the glob via `bmatcuk/doublestar/v4` over
  `os.DirFS` so matches are confined to the fetched source root, then
  decodes each match by format:
  - `yaml` (default) → `sigs.k8s.io/yaml` (parsed `any`)
  - `json` → `encoding/json` (parsed `any`)
  - `text` → `string`
  - `raw` → `[]byte`
  Single-file references return the value directly. Globs return a
  sorted `[]any` (`as: list`, default) or a `map[string]any` keyed by
  the per-item `keyExpr` (`as: map`). Empty glob matches yield an
  empty list/map rather than failing, mirroring shell glob ergonomics.
  Each file is bounded by `maxLoadFileBytes` (10 MiB) so a hostile
  source can't OOM the controller.
- `internal/pipeline/template.go` — Sprig-backed evaluator for
  per-item `keyExpr` template fragments. The function map is Sprig's
  `TxtFuncMap` with the environment-leaking helpers (`env`,
  `expandenv`) and the render-only helpers (`include`, `tpl`)
  deleted; slice 6's template engine will share the same shape.
- `internal/pipeline/load_test.go` — table-driven unit tests covering
  every format, single + glob + double-star, list and map shapes,
  duplicate / empty keyExpr keys, invalid YAML/JSON, missing files,
  unknown aliases, parent-segment refs, oversize files, and step-to-
  step output propagation.
- `internal/controller/manifestgenerator_pipeline.go` — wires
  `pipeline.Compile`/`Run` into the reconcile loop after sources are
  fetched. Pipeline outputs are not yet consumed by the artifact
  builder (slices 6/7 will), but evaluation failures surface as
  `PipelineFailedReason` against the Ready condition.
- `internal/controller/manifestgenerator_validation.go` — pipeline
  branch now compiles the spec via `pipeline.Compile` at validation
  time, so syntax errors, unknown aliases, malformed keyExprs and
  slice-5+ verbs all stall the object with `ValidationFailedReason`
  before any source fetch.
- `internal/controller_test/manifestgenerator_pipeline_test.go` —
  envtest integration test: a `ManifestGenerator` with both a
  single-file load and a glob-as-map load reconciles Ready=True; a
  syntactically-broken keyExpr stalls with `ValidationFailedReason`;
  a missing runtime file surfaces `PipelineFailedReason`.

### Slice-4 spec interpretation (temporary)

The CRD still admits `values`, `valuesFrom`, and
`artifacts[*].forEach`, but the validator continues to mark the object
Stalled with `ValidationFailedReason` if any are set. Slices 5–8 will
replace those rejections, and slice 6/7 will plumb pipeline outputs
into the template render path.

### Verified

Inside the dev container:

```sh
make tidy fmt vet manifests generate manager test clean
kustomize build config/default
```

`make test` brings up envtest and runs both the slice-3 pass-through
suite and the slice-4 pipeline suite; the `internal/pipeline` unit
tests run alongside under `go test ./...`.

## 14. Open questions

None blocking. Defer until the relevant slice:

- Final shape of pipeline value types in the controller's Go layer:
  the slice-4 implementation uses untyped `any` trees (the YAML
  decoder's natural shape). Revisit if filter/map/group/merge need
  stricter typing.
- Whether `keyExpr` and `where` get a CEL backend in addition to Go
  template fragments. Likely not for v1; revisit if templates feel
  awkward.
- Whether to factor the pipeline's Sprig FuncMap out into a shared
  `internal/template` package once slice 6's render engine wants the
  same surface. Trivial mechanical change; deferred until that slice.
- Manager image base and CI. Out of scope until v0.1.0 release prep.
