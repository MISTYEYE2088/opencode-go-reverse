#!/usr/bin/env bash
# Build the Linux plugin library against a glibc 2.17 baseline.
#
# Why a container: `go build` links against the build machine's glibc. Built on a
# current distro (2.34+), the .so refuses to load on RHEL 7/8, Ubuntu 20.04 or
# Debian 11 with "version `GLIBC_2.34' not found" — the host binary loads, the
# plugin never registers. manylinux2014 is CentOS 7 (glibc 2.17), the same
# baseline the CLIProxyAPI release itself is built against.
#
# Requires: docker, and network access for the Go toolchain + modules.
# Usage: scripts/build-linux-container.sh [output-dir]
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
version=$(tr -d '[:space:]' < "$repo_root/plugin/VERSION")
out_dir=${1:-"$repo_root/dist"}
artifact="go-adapter-v${version}.so"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required for the baseline build" >&2
  exit 1
fi

go_version=${GO_VERSION:-}
if [ -z "$go_version" ]; then
  go_version=$(cd "$repo_root/plugin" && go env GOVERSION | sed 's/^go//')
fi

mkdir -p "$out_dir"
# docker -v needs an absolute path: a relative one silently becomes a named
# volume and the artifact never lands in the workspace.
out_dir=$(cd "$out_dir" && pwd)
echo "building $artifact with go $go_version on glibc 2.17 (manylinux2014)"

docker run --rm --entrypoint /bin/bash \
  -v "$repo_root:/src" \
  -v "$out_dir:/out" \
  -e CGO_ENABLED=1 -e HOME=/tmp -e GOPATH=/tmp/gopath -e GOCACHE=/tmp/gocache \
  -e GO_VERSION="$go_version" -e ARTIFACT="$artifact" -e PLUGIN_VERSION="$version" \
  -e HTTPS_PROXY="${HTTPS_PROXY:-}" -e HTTP_PROXY="${HTTP_PROXY:-}" -e NO_PROXY="${NO_PROXY:-}" \
  quay.io/pypa/manylinux2014_x86_64 -lc '
    set -euo pipefail
    export PATH=/opt/rh/devtoolset-10/root/usr/bin:$PATH
    if [ ! -x /tmp/go/bin/go ]; then
      curl -sSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /tmp
    fi
    export PATH=/tmp/go/bin:$PATH
    cd /src/plugin
    # -buildvcs=false: inside the container the mounted checkout belongs to
    # another uid, and git refuses to stamp it ("dubious ownership").
    go build -trimpath -buildvcs=false -buildmode=c-shared \
      -ldflags "-s -w -X main.pluginVersion=${PLUGIN_VERSION}" \
      -o "/out/${ARTIFACT}" .
    baseline=$(readelf --version-info "/out/${ARTIFACT}" | grep -o "GLIBC_[0-9.]*" | sort -uV | tail -1)
    echo "glibc baseline: ${baseline}"
    ls -l "/out/${ARTIFACT}"
  '
