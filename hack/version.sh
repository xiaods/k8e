#!/bin/bash

GO=${GO-go}
ARCH=${ARCH:-$("${GO}" env GOARCH)}
SUFFIX="-${ARCH}"
TREE_STATE=clean
COMMIT=$(git rev-parse HEAD)

if [ -d .git ]; then
    if [ -z "$GIT_TAG" ]; then
        GIT_TAG=$(git tag -l --contains HEAD | head -n 1)
    fi
    if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
        DIRTY="-dirty"
        TREE_STATE=dirty
    fi

    COMMIT=$(git log -n3 --pretty=format:"%H %ae"| cut -f1 -d\  | head -1)
    if [ -z "${COMMIT}" ]; then
    COMMIT=$(git rev-parse HEAD || true)
    fi
fi

VERSION_CRICTL=$(grep github.com/kubernetes-sigs/cri-tools go.mod | head -n1 | awk '{print $4}')
if [ -z "$VERSION_CRICTL" ]; then
    VERSION_CRICTL="v0.0.0"
fi

VERSION_K8S=$(grep 'k8s.io/kubernetes v' go.mod | head -n1 | awk '{print $2}')
if [ -z "$VERSION_K8S" ]; then
    VERSION_K8S="v0.0.0"
fi

VERSION_CNIPLUGINS="v0.8.6-k3s1"

VERSION="$VERSION_K8S+k8e-${COMMIT:0:8}$DIRTY"

VERSION_TAG="$(sed -e 's/+/-/g' <<< "$VERSION")"