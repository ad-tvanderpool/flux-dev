# Dev container

This dev container pins the Go toolchain to the version declared in
`flux-manifest-generator/go.mod` and includes `kubectl` + `kustomize`
so every Makefile target works out of the box.

It is intentionally installed at the **workspace root** (`flux/`) rather
than inside `flux-manifest-generator/` so Cursor can open the parent
monorepo (with its source-watcher / flux-operator / source-controller
submodules used as reference) and immediately "Reopen in Container".

## Using it from Cursor

1. Open the `flux/` workspace in Cursor.
2. `Ctrl+Shift+P` → "Dev Containers: Reopen in Container".
3. First build will take a few minutes (image pull + features + module
   pre-warm).

## What's inside

| Tool | Version | Provided by |
|---|---|---|
| Go | matches `flux-manifest-generator/go.mod` (`1.26`) | base image |
| make, git, curl | latest stable | base image |
| kubectl | latest stable | feature |
| kustomize | latest stable | feature |
| controller-gen, setup-envtest | pinned in `flux-manifest-generator/Makefile` | downloaded by `postCreateCommand` |

Docker is intentionally not installed inside the container; build
images on the host with `make docker-build`.

## Verifying the toolchain

After "Reopen in Container" finishes:

```sh
cd flux-manifest-generator
go version
make tidy fmt vet manager
./bin/manager --help
```

## Before extracting flux-manifest-generator into its own repo

This `.devcontainer/` directory lives at the parent-workspace root so
it would be **lost** by a naive `git-filter-repo --subdirectory-filter
flux-manifest-generator`. Before running the extraction:

1. Move `.devcontainer/` into `flux-manifest-generator/.devcontainer/`.
2. Edit `.devcontainer/devcontainer.json` to remove the
   `cd flux-manifest-generator &&` prefix from `postCreateCommand`
   (the project will now be the workspace root).
3. Update this README accordingly.
4. Then run `git-filter-repo`.
