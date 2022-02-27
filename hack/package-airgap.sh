#!/bin/bash
set -e -x

cd $(dirname $0)/..

. ./hack/version.sh

BIN_SUFFIX="-${ARCH}"
if [ ${ARCH} = amd64 ]; then
    BIN_SUFFIX=""
elif [ ${ARCH} = aarch64 ] || [ ${ARCH} = arm64 ]; then
    BIN_SUFFIX="-arm64"
fi

airgap_image_file='hack/airgap/image-list.txt'
images=$(cat "${airgap_image_file}")
xargs -n1 docker pull <<< "${images}"
docker save ${images} -o dist/artifacts/k8e-airgap-images${BIN_SUFFIX}.tar
gzip -v -c dist/artifacts/k8e-airgap-images${BIN_SUFFIX}.tar > dist/artifacts/k8e-airgap-images${BIN_SUFFIX}.tar.gz
cp "${airgap_image_file}" dist/artifacts/k8e-images${BIN_SUFFIX}.txt