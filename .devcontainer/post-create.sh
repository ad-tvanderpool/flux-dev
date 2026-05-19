#!/usr/bin/env bash
# Post-create hook for the flux-manifest-generator dev container.
#
# Runs once after the container is built. Installs tooling that is not
# provided by a Feature, and pre-warms Go modules and Makefile-managed
# helpers so the first `make manager` / `make test` is fast and offline.

set -euo pipefail

KUSTOMIZE_VERSION="${KUSTOMIZE_VERSION:-5.4.3}"

echo ">>> Installing kustomize ${KUSTOMIZE_VERSION}"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
curl -fsSL https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh \
    | bash -s -- "${KUSTOMIZE_VERSION}" "$tmpdir"
sudo install -m 0755 "$tmpdir/kustomize" /usr/local/bin/kustomize
kustomize version

echo ">>> Pre-warming Go module caches"
cd "$(dirname "$0")/../flux-manifest-generator"
go mod download
( cd api && go mod download )

echo ">>> Pre-fetching controller-gen and setup-envtest"
make controller-gen setup-envtest

echo ">>> Dev container ready."
sudo chown -R vscode:vscode /home/vscode/
