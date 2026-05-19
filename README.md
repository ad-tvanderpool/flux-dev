# flux-manifest-generator

A Flux GitOps Toolkit controller that generates Kubernetes manifests
dynamically from templates, source data, and pipeline rules, and publishes
them as `ExternalArtifact` objects consumable by `Kustomization` and
`HelmRelease`.

## Status

Pre-alpha. The `ManifestGenerator` CRD
(`manifests.fluxcd.tooling/v1alpha1`) has its full type surface in
place — sources, inline `values`, `valuesFrom` (`ConfigMap` only),
declarative pipeline (`load`/`filter`/`map`/`group`/`merge`),
artifacts with `forEach` and templates, plus status with conditions
and inventory. The reconciler observes referenced source-controller
artifacts, fetches them, evaluates the full pipeline
(`load`/`filter`/`map`/`group`/`merge`) against the fetched files,
and publishes the configured templates as `ExternalArtifact` objects
(pass-through copy, no templating yet). `values` / `valuesFrom`, the
template engine, `forEach`, and Helm-style partials are not yet
wired and are rejected by the validator.

For the full design-of-record (decisions, CRD sketch, pipeline
semantics, template helpers, slice roadmap, current state),
see [`docs/DESIGN.md`](docs/DESIGN.md).

## Concept

A `ManifestGenerator` describes:

1. **Sources** — Flux source-controller objects whose artifacts hold
   input files (`GitRepository`, `OCIRepository`, `Bucket`, `HelmChart`,
   `ExternalArtifact`), referenced by `@alias` as in `ArtifactGenerator`.
2. **Values** — inline data and (later) `ConfigMap` references that
   merge into a single `.Values`-like tree available in templates.
3. **Pipeline** — a declarative list of steps that load data out of
   source artifacts and shape it: `load | filter | map | group | merge`.
   No imperative state, no `exec`, no remote calls.
4. **Artifacts** — one or more rendered outputs. Each artifact may
   render multiple templates and may `forEach` over a pipeline value
   to fan a single template across a collection.

Each output is published as an `ExternalArtifact` served by this
controller's own HTTP endpoint (same pattern as `source-watcher`).
Downstream `Kustomization` and `HelmRelease` resources reference it
via `sourceRef`.

## Relationship to existing Flux APIs

| | Templating | Pipeline / data shaping | Publishes artifact | Inputs from files in source artifacts |
|---|---|---|---|---|
| `helm-controller` `HelmRelease` | Yes (Helm chart) | Limited | No | No |
| `flux-operator` `ResourceSet` | Yes (Go template, single block) | No (just an inputs matrix) | No (applies directly) | No |
| `source-watcher` `ArtifactGenerator` | No | No | Yes | Byte-level copy/merge only |
| `flux-manifest-generator` `ManifestGenerator` | Yes (Go template + Sprig + Helm-style helpers) | Yes (`load/filter/map/group/merge`) | Yes | Yes (glob across source artifacts) |

## Templating

Go `text/template` + Sprig + a small set of Helm-style helpers
(`toYaml`, `fromYaml`, `include`, `required`, `tpl`, `lookupFile`).
The renderer sits behind a `TemplateEngine` interface so an alternative
engine can be added later without changing the CRD.

## Build

```sh
make tidy fmt vet manager
./bin/manager --help
```

## License

Apache 2.0. See [`LICENSE`](LICENSE).
