## High-level structure of the GitOps repo

Three top-level layers compose to produce a running cluster. Flux walks them top-down, with each layer answering a different question:

```
base/         ──  what + how       (the parts bin)
templates/    ──  which            (cluster-type assemblies)
clusters/     ──  where + how much (concrete instances)
```

The flow is:

```
clusters/<type>/<name>  ──▶  templates/<type>  ──▶  base/
   (instance)                (cluster-type shape)   (parts bin)
```

### `base/` — the parts bin

Every shippable unit lives here, environment-neutral, with production defaults. Cluster-specific values are *not* in `base/` — they appear only as `${VAR}` placeholders resolved later.

```
base/
├── template/        plumbing Kustomizations every template installs:
│                      base-parts.yaml, template-parts.yaml, template-vars.yaml
└── parts/<group>/   shippable groups (cert-manager, kafka, powerdns, …)
    ├── artifact-generator.yaml   one AG per group (the atomic packaging unit)
    ├── infra/ + infra.yaml       group prerequisites: Namespace, HelmRepository,
    │                             pull secrets, ImageRepositories, SOPS secrets
    ├── <unit>/                   payload manifests (HelmRelease, etc.)
    ├── <unit>.yaml               per-unit Flux Kustomization wrapper
    └── vars.yaml                 (optional) group-scoped defaults
```

Conventions worth knowing:
- Everything is keyed `<group>--<unit>` (`powerdns--admin`, `kafka--mirrormaker`, `cert-manager--issuer`).
- Each group's `infra` unit is gated by `wait: true` so siblings block on namespaces/repos/secrets being ready.
- Each wrapper inlines its own `postBuild.substituteFrom` (seeded with `template-vars` + `cluster-vars`, optionally extended with `group-vars-*`).
- Each group has exactly one ArtifactGenerator, in one of two flavors: **Option A** (one artifact per group, default) or **Option B** (one artifact per unit, used only when units must ship per cluster type — e.g. Kafka's `mirrormaker`).

### `templates/` — cluster-type shapes

A template decides *which* groups (and units) a class of cluster ships, and carries any manifests common to every instance of that type but not worth packaging as a reusable `base/` group.

```
templates/
├── _common/                     shared bits (flux-instance.yaml, namespace.yaml)
├── core/                        core cluster type (slc03 prod, irv02 dev, …)
│   ├── kustomization.yaml       pins the two plumbing files below
│   ├── base.yaml                Kustomization → ./base/template
│   ├── artifact-generator.yaml  produces 3 ExternalArtifacts:
│   │                              base-parts, template-parts, template-vars
│   ├── parts/                   (optional) template overlays — manifests
│   │                            shipped to every instance of this type
│   └── vars/<group>.yaml        per-group overrides for this cluster type
└── campus/                      same shape, different group selection
    └── kafka/                   campus-specific Kafka topics, MM2 config, etc.
```

The `base-parts` AG stanza is where selection happens: each `base/parts/<group>/*.yaml` glob you include pulls that group into this cluster type. Templates do **not** carry instance-specific values, dependency wiring, health checks, or intervals — those properties belong to the unit in `base/`.

### `clusters/` — concrete instances

A cluster directory is intentionally thin: pin a git revision, supply this instance's variables, optionally patch architecture.

```
clusters/
├── core/                        instances of templates/core
│   ├── prod/
│   ├── dev/
└── campus/                      instances of templates/campus
    ├── eln/                       per remote datacenter site
    └── far/
```

Each instance dir contains:

```
clusters/<type>/<name>/
├── kustomization.yaml      plain index over the files below
├── flux-instance.yaml      Flux Operator FluxInstance: controllers + GitRepository sync
├── cluster-config.yaml     ConfigMap "cluster-vars" — the only file that should
│                           differ between two otherwise-identical instances
├── vars/<group>.yaml       (optional) per-group override for this cluster only
├── template.yaml           Kustomization → ./templates/<type>
└── talos/                  Talos OS / machine-config inputs (cluster-specific)
```

Bootstrap is via `tools/deploy-flux-controller <cluster-path>`; there is no `flux-system/` dir — the Flux Operator + `FluxInstance` CR replace the old `gotk-components.yaml` / `gotk-sync.yaml` pair.

### The guiding rule

> **`base/` is shared and knows *what* and *how*; `templates/` decides *which*; `clusters/` supplies *where* and *how much*.**

