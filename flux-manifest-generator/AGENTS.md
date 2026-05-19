# AGENTS.md

Guidance for AI coding assistants working in `flux-manifest-generator`.
Read this file before making changes.

## Project overview

A Kubernetes controller in the Flux GitOps Toolkit ecosystem. It
reconciles the `ManifestGenerator` CRD under `manifests.fluxcd.tooling`,
which loads data from source-controller artifacts, runs it through a
declarative pipeline (`load | filter | map | group | merge`), and renders
the result through Go templates (with Sprig and Helm-style helpers) into
an `ExternalArtifact` consumable by `Kustomization` and `HelmRelease`.

Modeled architecturally after `fluxcd/source-watcher`:

- Reconciler fetches source artifacts over HTTP from source-controller
  using `github.com/fluxcd/pkg/artifact/fetch`.
- Output tarballs are written via `github.com/fluxcd/pkg/artifact/storage`
  and served by `github.com/fluxcd/pkg/artifact/server` once this pod is
  the elected leader.
- The output object is an `ExternalArtifact` from
  `source.toolkit.fluxcd.io/v1` (we do not own that CRD; we only produce
  instances of it).

## Repository layout

```
.
├── api/                     # Separate Go module for the CRD types
│   └── v1alpha1/            # ManifestGenerator API package
├── cmd/main.go              # Manager entrypoint
├── config/                  # Kustomize overlays (manager, rbac, crd, samples)
├── hack/boilerplate.go.txt  # License header injected by controller-gen
├── internal/
│   ├── builder/             # Pure-Go artifact + manifest assembly
│   ├── controller/          # Reconciler (split across multiple files)
│   ├── pipeline/            # Pipeline verb implementations
│   └── render/              # Template engine interface + Go/Sprig impl
└── Makefile
```

`api/` is a **separate Go module** so that downstream consumers can
import the types without pulling in controller dependencies. After
adding imports in `api/v1alpha1/*.go` that are not already in
`api/go.mod`, run `make tidy` (which tidies both modules).

## Conventions

- License: Apache 2.0. Every new `.go` file starts with the header from
  `hack/boilerplate.go.txt`, with the year set to the current year.
- Go style: standard `gofmt`. Exported names and top-level declarations
  get doc comments. Keep comments terse and explain intent, not
  mechanics.
- Use the `any` type alias, not `interface{}`.
- Use `%w` to wrap errors. Never swallow errors silently.
- Never use `panic` in runtime code paths.
- The controller must not cache `Secret` or `ConfigMap` (`mgrConfig.Client.Cache.DisableFor` in `cmd/main.go`). Preserve that.
- Patterns to reuse: `gotkpatch.Helper` for status, `gotkconditions`
  helpers, `gotkmeta` reason constants, `gotkjitter` for requeue jitter,
  finalizer-based cleanup. Do not invent new mechanics.

## Build / test

```sh
make tidy fmt vet manager     # build the controller binary
make manifests                # regenerate CRDs and RBAC
make generate                 # regenerate zz_generated.deepcopy.go
make test                     # full test run (envtest)
```

After changing API types or kubebuilder markers, run
`make generate manifests`. Commit the regenerated files in the same PR.

## Generated files (never hand-edit)

- `api/v1alpha1/zz_generated.deepcopy.go`
- `config/crd/bases/manifests.fluxcd.tooling_*.yaml`
- Downloaded source-controller CRDs (`gitrepositories.yaml`, etc.) under
  `config/crd/bases/`, fetched by `make download-crd-deps`.

## Safety rules

- **No path traversal.** File paths from source tarballs and glob
  patterns must stay within the expected working directory. Validate
  extracted paths before writing.
- **No unbounded reads.** Use `io.LimitReader` when reading from
  network or archive sources.
- **No command injection.** Do not shell out via `os/exec`.
- **No secrets in logs or events.** Use `fluxcd/pkg/masktoken` to scrub
  any strings that may contain credentials.
- **Deterministic output.** Identical inputs must produce identical
  tarballs — sorted file walks, stable timestamps, no randomness.
- **Resource cleanup.** Defer-close all readers, writers, and tempdirs.
  Use `t.TempDir()` in tests.

## Pipeline design constraints

- Pipeline is **declarative**, not imperative. Steps name their outputs;
  later steps reference earlier outputs by name. There is no
  `set_fact`-style mutation, no ordering tricks beyond data dependency.
- Verbs supported in v1: `load`, `filter`, `map`, `group`, `merge`.
  Do **not** add `exec`, `script`, or HTTP fetches to the pipeline.
- The template render is the **only** place where Go template syntax is
  meaningful. Pipeline step `where:` and `keyExpr:` accept template
  fragments because their evaluation is per-item and well-bounded; do
  not generalize this to other fields without explicit approval.
