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
| 5 | Pipeline verbs: `filter`, `map`, `group`, `merge` | **Done** |
| 6 | Template engine interface + Go/Sprig implementation + Helm-style helpers | **Done** |
| 7 | `artifacts.forEach` + multi-template artifacts + output-path templating | **Done** |
| 8 | `values` + `valuesFrom` (`ConfigMap` only) | **Done** |
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

## 14. Slice 5 — done

The remaining four pipeline verbs land. `Compile` now accepts every
verb defined in §6, and the reconciler evaluates them end-to-end after
the source fetch. Pipeline outputs are still not consumed by the
artifact builder — that wiring lands with the template engine and
`forEach` in slices 6/7 — so the published `ExternalArtifact` remains
a pass-through copy. Files added or changed in this slice:

- `internal/pipeline/filter.go` — the `filter` verb. Renders the
  `where` template against each item of a `[]any` or `map[string]any`
  input, trims, and interprets the result via `strconv.ParseBool`
  (with empty → false). Anything else is a hard error so a typo
  stalls the pipeline rather than silently keeping every item.
  Output preserves the input shape.
- `internal/pipeline/mapverb.go` — the `map` verb (file named
  `mapverb.go` because `map` is a Go keyword). Renders `expr` per
  item, then parses the rendered string as YAML to form the new item
  value, mirroring Helm's `tpl`-then-parse contract. Output keeps
  the input shape (list-in/list-out, map-in/map-out).
- `internal/pipeline/group.go` — the `group` verb. Buckets a list
  input into a `map[string]any` of `[]any` by `keyExpr`. Within each
  bucket items keep input order. Map inputs are rejected — they're
  already keyed; use `map` first if you need to flatten one.
- `internal/pipeline/merge.go` — the `merge` verb. Deep-merges 2–100
  prior outputs left-to-right with Helm values semantics (later wins
  on conflicts, nested maps recurse, lists are replaced not
  concatenated). Implemented around a copy-on-write `deepMerge`/
  `deepClone` pair so merge can never mutate the inputs that earlier
  outputs publish — the no-mutation rule in §6 is enforced
  structurally, not by convention.
- `internal/pipeline/iter.go` — small helpers shared by the
  collection-iterating verbs (`listInput`, `sortedKeys`).
- `internal/pipeline/template.go` — refactored so a single
  `itemTemplate` backs every per-item expression (load.keyExpr,
  filter.where, map.expr, group.keyExpr). `keyExprEvaluator` becomes
  a thin wrapper enforcing the trim + non-empty contract. The
  per-item bindings are unioned with prior step outputs into the
  template data; user bindings (`value`, `key`, `index`, `path`)
  shadow output names on collision.
- `internal/pipeline/pipeline.go` — `Compile` now passes the set of
  already-compiled step names into each verb compiler and enforces
  the forward-reference rule: every `from` (including each entry of
  `merge.from`) must name a step earlier in the spec. A step cannot
  reference itself because the name is recorded only after a
  successful compile. `compileStep` no longer returns "not yet
  supported" for any verb.
- `internal/pipeline/filter_test.go` / `mapverb_test.go` /
  `group_test.go` / `merge_test.go` — table-driven unit tests for
  each verb covering shape preservation, prior-output access,
  template-error surfaces, runtime type rejections (non-collection
  inputs, non-map merge sources), compile-time forward-reference
  rejections, deterministic ordering, and the no-mutation guarantee
  on merge.
- `internal/controller/manifestgenerator_validation.go` — comment
  refresh; the body is unchanged. Pipeline validation continues to
  flow through `pipeline.Compile`, so all slice-5 verbs now compile
  there too; `values`, `valuesFrom`, and `forEach` remain rejected
  until slices 7/8 land.
- `internal/controller_test/manifestgenerator_pipeline_verbs_test.go`
  — envtest integration test: a `ManifestGenerator` that exercises
  every verb (load → merge, load + load → filter → map, load →
  group) reconciles Ready=True; a non-map `merge` source surfaces
  `PipelineFailedReason` at runtime; a forward `filter.from`
  reference stalls with `ValidationFailedReason`.

### Slice-5 spec interpretation (temporary)

The CRD still admits `values`, `valuesFrom`, and
`artifacts[*].forEach`; the validator continues to mark the object
Stalled with `ValidationFailedReason` if any are set. Slices 6/7 will
plumb pipeline outputs into the template render path and add
`forEach`; slice 8 will wire `values` + `valuesFrom`.

### Verified

Inside the dev container:

```sh
make tidy fmt vet manifests generate manager test clean
kustomize build config/default
```

`make test` brings up envtest and runs the slice-3 / slice-4 suites
alongside the slice-5 verb suite; the `internal/pipeline` unit tests
run under `go test ./...`.

## 16. Slice 6 — done

The artifact render engine lands. Each `spec.artifacts[*].templates[*]`
is now read out of its source artifact, rendered through a Go
text/template engine with Sprig + Helm-style helpers, and written to
the staging tarball. Pipeline outputs are exposed at the top of the
template scope so a template fragment reads a prior step's value as
`.<stepName>`. `forEach`, `values`, `valuesFrom`, and output-path
templating continue to be rejected by the validator until their slices
land (7 and 8). Files added or changed in this slice:

- `internal/template/funcmap.go` (new) — shared Sprig `FuncMap` used
  by both the pipeline expression evaluator and the render engine.
  Resolves the slice-5 open question of where to host the helpers
  once a second consumer arrived. Sprig minus the host-leaking
  helpers (`env`, `expandenv`) and minus the recursion-only helpers
  (`include`, `tpl`); the render engine re-binds the recursion-only
  helpers to its own `*template.Template` so they can reach the
  parsed template tree.
- `internal/pipeline/template.go` — `pipelineFuncMap` deleted; the
  per-item evaluator now calls `mgtemplate.FuncMap` directly. No
  behavioural change to pipeline expressions.
- `internal/render/render.go` (new) — `Engine` interface + `Error`
  type. The interface is intentionally tiny (one method) so a future
  engine (CEL, Jsonnet) is a drop-in. The `Error` type wraps parse
  and execute failures so the reconciler can tell render failures
  apart from I/O / staging failures via `errors.As` and surface
  `RenderFailedReason` instead of the generic `BuildFailedReason`.
- `internal/render/gotemplate.go` (new) — `GoEngine`, the default
  implementation. `missingkey=zero` matches Helm: a missing scope
  key renders as the zero value rather than aborting. Strict
  missing-key checks are an explicit `required` call per value.
- `internal/render/helpers.go` (new) — Helm-style helpers:
  - `toYaml` / `toJson` — marshal a value, empty string on error
    (matches Helm so a malformed sub-tree does not abort the
    surrounding template).
  - `fromYaml` / `fromJson` — unmarshal into a map; decode errors
    are surfaced under the `Error` key (matches Helm).
  - `required` — fails the render with a clear message when the
    value is `nil` or an empty string.
  - `tpl` — re-parses a string as a template against a clone of the
    parent so per-call partials do not leak into the parent tree.
  - `include` — executes a named sub-template parsed into the same
    tree. Slice 6 ships no implicit sub-templates; slice 9 will
    discover `_helpers.tpl` siblings and parse them into the same
    tree so authors get the Helm partials experience for free.
  - `lookupFile` is intentionally deferred to slice 9 alongside
    partial discovery — it has no useful semantics without it.
- `internal/render/gotemplate_test.go` /
  `internal/render/helpers_test.go` (new) — table-driven unit
  coverage for every helper plus the engine contract: literal
  passthrough, missing-key policy, `env`/`expandenv` are absent,
  parse / execute failures wrap into `*render.Error`, and the
  engine is safe for concurrent use.
- `internal/builder/builder.go` — pass-through copy replaced by
  single-file template rendering. `from:` must resolve to a regular
  file; directory references are explicitly rejected (the only
  coherent fan-a-template-across-files semantics is `forEach`, which
  lands in slice 7). Each rendered file is size-capped at
  `maxTemplateFileBytes` (10 MiB) mirroring the pipeline `load`
  ceiling. `ArtifactBuilder` now carries a `render.Engine` and
  `Build` accepts a `data map[string]any` template scope.
- `internal/controller/manifestgenerator_controller.go` — threads
  pipeline outputs into the builder as the top-level template scope
  and lazily allocates a `render.NewGoEngine()` if the reconciler
  was constructed without one. Build failures whose root cause is a
  `*render.Error` now surface as `RenderFailedReason`; everything
  else stays `BuildFailedReason`.
- `internal/controller/manifestgenerator_pipeline.go` — comment
  refresh; pipeline outputs are now consumed by the render engine.
- `internal/controller_test/manifestgenerator_render_test.go` (new)
  — envtest integration test: a `ManifestGenerator` whose template
  exercises field lookup, Sprig (`upper`), and `toYaml | indent`
  against a pipeline output reconciles `Ready=True` and the
  resulting tarball contains the rendered (not source) bytes; a
  `required` failure with the value absent stalls the object with
  `RenderFailedReason` rather than `BuildFailedReason`.

### Slice-6 spec interpretation (temporary)

The CRD still admits `values`, `valuesFrom`, and
`artifacts[*].forEach`; the validator continues to mark the object
Stalled with `ValidationFailedReason` if any are set. The template
`to:` field is still treated as a literal path — output-path
templating lands with `forEach` in slice 7. Slice 8 wires `values` +
`valuesFrom`; slice 9 adds `_helpers.tpl` discovery so `include` /
`lookupFile` have something to resolve against.

### Verified

Inside the dev container:

```sh
make tidy fmt vet manifests generate manager test clean
kustomize build config/default
```

`make test` brings up envtest and runs the slice-3 / slice-4 /
slice-5 suites alongside the slice-6 render suite; the
`internal/render` and `internal/pipeline` unit tests run under
`go test ./...`.

## 17. Slice 7 — done

The artifact-build surface for v1alpha1 reaches its final shape (minus
`values` / `valuesFrom` in slice 8 and `_helpers.tpl` partial discovery
in slice 9): per-artifact `forEach`, multi-template artifacts, and
output-path templating all land in this slice. The validator's
last temporary rejection — `artifacts[*].forEach` — is removed; the
slice-7 spec interpretation is "everything in §5 except `values` and
`valuesFrom`". Files added or changed in this slice:

- `internal/builder/builder.go` — refactored so artifact assembly is
  driven by an iteration plan. `applyArtifact` resolves the plan
  (one iteration without `forEach`, N iterations with it),
  `buildIterations` expands a `forEach` over a pipeline output (a
  `[]any` or `map[string]any`) into a deterministic slice of
  iterations (sorted-key order for maps, input order for lists),
  and `applyTemplate` renders one template entry per iteration with
  the iteration overlay (`{key, value}` for maps, `{index, value}`
  for lists) applied under `forEach.as`. Per-iteration error
  messages include the iteration label (`list index:N`, `map key:K`)
  so a render failure points back at the offending entry.
- `internal/builder/builder.go` — `renderDestPath` template-expands
  the `templates[*].to` path against the iteration scope after
  stripping the fixed `@artifact/` prefix, then cleans the result
  and rejects `.`, `..` segments, and absolute paths as a defence-
  in-depth layer on top of the `os.Root`-based jailed write. An
  in-memory `writtenPaths` map detects two iterations rendering to
  the same destination so authors get a clear "destination X
  already written" error instead of a silent overwrite.
- `internal/builder/desttemplate.go` (new) —
  `parseDestTemplate` parses-only the path-template fragment using
  the shared `internal/template.FuncMap`. Used by the validator so
  syntax errors in `to:` stall with `ValidationFailedReason` at
  admission rather than at build time. `builder.ValidateDestTemplate`
  is the exported entry point.
- `internal/controller/manifestgenerator_validation.go` — drops the
  `forEach` rejection; instead, when `forEach != nil` the validator
  checks that `forEach.from` names a declared pipeline step
  (cheaper than full pipeline forward-reference handling because
  artifacts run after the entire pipeline). Each `templates[*].to`
  also flows through `builder.ValidateDestTemplate` for parse-only
  syntax checks. `values` and `valuesFrom` remain the only rejected
  spec features.
- `internal/controller_test/manifestgenerator_foreach_test.go` (new)
  — envtest integration coverage: map iteration produces one
  rendered file per entry per template with `.<as>.key`/`.value`
  bindings; list iteration exposes `.<as>.index`/`.value`;
  multi-template artifacts emit every template per iteration with
  templated `to:` paths; an unknown `forEach.from` stalls with
  `ValidationFailedReason`; a scalar `forEach.from` fails with
  `BuildFailedReason`; two iterations colliding on the same
  rendered `to:` path fail with `BuildFailedReason`. A separate
  `TestManifestGenerator_MultiTemplateNoForEach` test pins the
  multi-template surface without `forEach` so the no-iteration code
  path stays exercised end-to-end.
- `config/samples/manifests_v1alpha1_manifestgenerator.yaml` —
  rewritten around the slice-7 surface: a `pipeline` load step
  builds a map keyed by directory and the artifact fans two
  templates across the map with templated `to:` paths, exercising
  every new piece in a single example.

### Slice-7 spec interpretation (temporary)

The CRD still admits `values` and `valuesFrom`; the validator
continues to mark the object Stalled with `ValidationFailedReason`
if either is set. Slice 8 will replace those rejections. Slice 9
will add `_helpers.tpl` discovery so `include` / `lookupFile` have
something to resolve against.

### Verified

Inside the dev container:

```sh
make tidy fmt vet manifests generate manager test clean
kustomize build config/default
```

`make test` brings up envtest and runs the slice-3 / slice-4 /
slice-5 / slice-6 suites alongside the slice-7 `forEach` and
multi-template suites; the `internal/builder`, `internal/render`,
and `internal/pipeline` unit tests run under `go test ./...`.

## 18. Slice 8 — done

`spec.values` (inline) and `spec.valuesFrom` (`ConfigMap` only) land
in this slice. The validator's last temporary rejections are gone; the
only piece of §5 still missing from v1alpha1 is Helm-style partial
discovery, which arrives with slice 9. Files added or changed in this
slice:

- `internal/values/values.go` (new) — `Resolver` materialises the
  merged `.values` tree from `spec.valuesFrom` (in spec order, deep-
  merged with Helm-style "later wins" semantics) followed by
  `spec.values` inline. Each `valuesFrom` ConfigMap is fetched live
  through the supplied `client.Reader` (the controller passes its
  `APIReader` so the `Client.Cache.DisableFor` policy in `cmd/main.go`
  is preserved). The `valuesKey` / `targetPath` / `optional` semantics
  match the CRD docstrings: a single key parses as YAML, no key
  injects every entry as a string-valued map, and an absent ConfigMap
  or missing key with `optional: true` collapses to a no-op rather
  than failing. Cross-namespace ACL is enforced via `AccessError` so
  the reconciler can surface `AccessDeniedReason` as a terminal
  condition; live fetch failures become `FetchError` and requeue at
  the dependency interval as `SourceFetchFailedReason`. The package
  ships its own `deepMerge` / `deepClone` so a contributing
  ConfigMap's payload is never mutated through aliasing.
- `internal/values/values_test.go` (new) — table-driven unit tests
  covering inline-only, valuesFrom with `valuesKey`, valuesFrom with
  all keys, targetPath wrapping (single + nested), inline-overrides-
  valuesFrom, spec-order merging, optional vs non-optional missing CM
  and key, cross-namespace ACL, empty-segment targetPath rejection,
  and the inline-non-object rejection. Exported `ValidateInline` and
  `ValidateTargetPath` cover the admission-time path.
- `internal/pipeline/pipeline.go` / `template.go` — `Scope` gains a
  `Values` field; `Evaluator.Run` now accepts the resolved values
  tree, and `withOutputs` publishes it under the reserved `values`
  key (also exported as `pipeline.ValuesKey`) so per-item expressions
  see `.values.<...>` exactly like artifact templates do. `Compile`
  refuses any step named `values` so the merged tree cannot be
  shadowed.
- `internal/controller/manifestgenerator_values.go` (new) — controller-
  side wiring: `resolveValues` builds a `values.Resolver` against the
  reconciler's `APIReader` and runs it; `handleValuesError` classifies
  the result into terminal `AccessDeniedReason` (cross-namespace) vs
  requeueing `SourceFetchFailedReason` (transient API/data errors)
  using `errors.As` against the typed sentinels exported by
  `internal/values`.
- `internal/controller/manifestgenerator_controller.go` — calls
  `resolveValues` after the source fetch and threads the result into
  both `runPipeline` and the artifact builder's `templateData` under
  the reserved `values` key. The render scope retains its existing
  shape (`templateData[stepName] = output`) with `.values` published
  alongside, matching the design's "`.values` plus prior step
  outputs" root context.
- `internal/controller/manifestgenerator_pipeline.go` — `runPipeline`
  now plumbs the resolved values into `Evaluator.Run` so `keyExpr`,
  `where`, and `expr` fragments can read `.values.<...>`.
- `internal/controller/manifestgenerator_validation.go` — drops the
  `values` / `valuesFrom` rejections. New checks: inline values must
  be an object (or null) via `values.ValidateInline`; every
  `valuesFrom[].targetPath` parses via `values.ValidateTargetPath`;
  cross-namespace `valuesFrom` references stall with
  `AccessDeniedReason` when `--no-cross-namespace-refs` is set; a
  pipeline-step name of `values` and a `forEach.as` of `values` both
  stall with `ValidationFailedReason` because they would shadow the
  merged tree.
- `internal/controller/manifestgenerator_manager.go` — second cache
  index `.metadata.valuesFromRef` maps every MG to the
  `<namespace>/<name>` of each ConfigMap it consumes; a metadata-only
  `Watches(&metav1.PartialObjectMetadata{Kind:"ConfigMap"})` fans
  ConfigMap changes back into the MGs that reference them. Metadata-
  only watching preserves the no-cache-for-ConfigMaps policy: the
  informer carries only metadata so the controller never holds a
  long-lived cache of ConfigMap payloads. A small predicate filters
  out non-resource-version-bumping events so label-only churn does
  not requeue.
- `internal/controller_test/manifestgenerator_values_test.go` (new)
  — envtest integration coverage: inline `spec.values` plus
  `spec.valuesFrom` together drive a template render and a pipeline
  `filter.where` (`.values.cluster.tier` selects clusters by tier);
  an inline key collides with a ConfigMap key and inline wins;
  updating the ConfigMap triggers a re-render via the metadata-only
  watch; a non-optional missing ConfigMap surfaces
  `SourceFetchFailedReason`; an inline non-object value stalls with
  `ValidationFailedReason`. An optional missing ConfigMap stays
  Ready=True across the whole test, exercising that path implicitly.

### Slice-8 spec interpretation

The validator no longer rejects any §5 feature. Slice 9 will add
`_helpers.tpl` discovery so `include` and `lookupFile` have parsed
partials to resolve against; until then those helpers continue to
fail at execute time when invoked.

### Verified

Inside the dev container:

```sh
make tidy fmt vet manifests generate manager test clean
kustomize build config/default
```

`make test` brings up envtest and runs the slice-3 through slice-7
suites alongside the slice-8 values suite; the `internal/values`,
`internal/pipeline`, `internal/render`, and `internal/builder` unit
tests run under `go test ./...`.

## 19. Open questions

None blocking. Defer until the relevant slice:

- Final shape of pipeline value types in the controller's Go layer:
  the slice-4 implementation uses untyped `any` trees (the YAML
  decoder's natural shape). Revisit if filter/map/group/merge need
  stricter typing.
- Whether `keyExpr` and `where` get a CEL backend in addition to Go
  template fragments. Likely not for v1; revisit if templates feel
  awkward.
- Manager image base and CI. Out of scope until v0.1.0 release prep.
