#!/bin/bash

GO=${GO-go}
ARCH=${ARCH:-$(${GO} env GOARCH)}
OS=$("${GO}" env GOOS)
SUFFIX="-${ARCH}"
# RELEASE_TAG pins the version for builds that have no git history to read.
#
# Nothing in this repository sets it: every CI job checks out with a tag on the
# commit, so `git tag -l --contains HEAD` below already recovers the release
# version — including in the default shallow `actions/checkout`, which still
# fetches tags that point at the checked-out commit.
#
# It matters when building a copied source tree: a release tarball, a vendored
# or air-gapped build, or a Docker context assembled without .git. There the
# tag lookup is skipped entirely (the `if [ -d .git ]` guard) and, without this
# variable, the version degrades to "$VERSION_K8S+k8e-" — a trailing dash with
# no commit, which is both a lie in `k8e --version` and an invalid image tag
# once '+' becomes '-'.
#
# Quoted for hygiene (and so a future edit that moves this into a command
# argument does not inherit a splitting surprise). Note that bash does not
# word-split an assignment's right-hand side, so this is not fixing a live bug:
# "$RELEASE_TAG" and $RELEASE_TAG already assign identically.
GIT_TAG="${RELEASE_TAG}"
TREE_STATE=clean
COMMIT=$DRONE_COMMIT

if [ -d .git ]; then
    if [ -z "$GIT_TAG" ]; then
        GIT_TAG=$(git tag -l --contains HEAD | head -n 1)
    fi
    if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
        DIRTY="-dirty"
        TREE_STATE=dirty
    fi

    COMMIT=$(git log -n3 --pretty=format:"%H %ae" | grep -v ' drone@localhost$' | cut -f1 -d\  | head -1)
    if [ -z "${COMMIT}" ]; then
    COMMIT=$(git rev-parse HEAD || true)
    fi
fi

get-module-version(){
  go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $1
}

get-module-path(){
  go list -m -f '{{if .Replace}}{{.Replace.Path}}{{else}}{{.Path}}{{end}}' $1
}

PKG_CONTAINERD_K8E=$(get-module-path github.com/containerd/containerd/v2)
VERSION_CONTAINERD=$(get-module-version github.com/containerd/containerd/v2)

VERSION_CRICTL=$(get-module-version sigs.k8s.io/cri-tools)
if [ -z "$VERSION_CRICTL" ]; then
    VERSION_CRICTL="v0.0.0"
fi

VERSION_K8S_K8E=$(get-module-version k8s.io/kubernetes)
VERSION_K8S=${VERSION_K8S_K8E%-k3s*}
if [ -z "$VERSION_K8S" ]; then
    VERSION_K8S="v0.0.0"
fi

VERSION_RUNC=$(get-module-version github.com/opencontainers/runc)
if [ -z "$VERSION_RUNC" ]; then
    VERSION_RUNC="v0.0.0"
fi

VERSION_HCSSHIM=$(get-module-version github.com/Microsoft/hcsshim)
if [ -z "$VERSION_HCSSHIM" ]; then
    VERSION_HCSSHIM="v0.0.0"
fi

VERSION_CRI_DOCKERD=$(get-module-version github.com/Mirantis/cri-dockerd)
if [ -z "$VERSION_CRI_DOCKERD" ]; then
  VERSION_CRI_DOCKERD="v0.0.0"
fi

VERSION_CNIPLUGINS="v1.9.1-k3s1"

VERSION_CILIUM_CHART="1.20.0"

# rqlite powers the Tandem etcd-compatibility backend (KIP-29). One pinned
# version, shared by every consumer: hack/download stages it into bin/ for the
# release, hack/e2e/Dockerfile vendors it for the E2E image, and the M0/M1
# evidence harnesses download and verify it. Changing this version means
# re-running the M0/M1 differential suite — rqlite releases occasionally change
# Raft semantics or the HTTP surface the compat layer speaks to.
VERSION_RQLITE="v10.3.6"

DEPENDENCIES_URL="https://raw.githubusercontent.com/kubernetes/kubernetes/${VERSION_K8S}/build/dependencies.yaml"
if command -v yq >/dev/null 2>&1 && yq --version >/dev/null 2>&1; then
    VERSION_GOLANG="go"$(curl  --proto '=https' -sL --connect-timeout 5 --max-time 10 "${DEPENDENCIES_URL}" 2>/dev/null | yq e '.dependencies[] | select(.name == "golang: upstream version").version' - 2>/dev/null)
else
    VERSION_GOLANG="go"$(curl  --proto '=https' -sL --connect-timeout 5 --max-time 10 "${DEPENDENCIES_URL}" 2>/dev/null | grep -A 2 "golang: upstream version" | grep "version:" | cut -d: -f2 | xargs 2>/dev/null)
fi
# Fallback: if upstream Go version could not be fetched (e.g. network unavailable),
# use the locally installed Go version.
if [ "$VERSION_GOLANG" = "go" ]; then
    VERSION_GOLANG=$(${GO} version | awk '{print $3}')
fi


if [[ -n "$GIT_TAG" ]]; then
    if [[ ! "$GIT_TAG" =~ ^"$VERSION_K8S"[+-] ]]; then
        echo "Tagged version '$GIT_TAG' does not match expected version '$VERSION_K8S[+-]*'" >&2
        exit 1
    fi
    VERSION=$GIT_TAG
else
    VERSION="$VERSION_K8S+k8e-${COMMIT:0:8}$DIRTY"
    # The commit is only known from git history (REVISION) or an override, so a
    # build outside a checkout leaves it empty and produces "+k8e-". Warn
    # instead of shipping it silently: the version is what operators read from
    # `k8e --version` and what image tags are derived from, and a truncated one
    # is impossible to trace back to a source commit.
    if [[ -z "${COMMIT}" ]]; then
        echo "warning: no git tag and no commit available; version '${VERSION}' cannot be traced to a source revision." >&2
        echo "warning: set RELEASE_TAG (e.g. RELEASE_TAG='$VERSION') when building a copied source tree." >&2
    fi
fi
VERSION_TAG="$(sed -e 's/+/-/g' <<< "$VERSION")"

BINARY_POSTFIX=
if [ ${OS} = windows ]; then
    BINARY_POSTFIX=.exe
fi
export VERSION VERSION_TAG VERSION_GOLANG VERSION_K8S VERSION_CRICTL VERSION_CONTAINERD VERSION_CNIPLUGINS VERSION_CILIUM_CHART VERSION_RUNC VERSION_HCSSHIM VERSION_CRI_DOCKERD VERSION_RQLITE PKG_CONTAINERD_K8E COMMIT TREE_STATE
