#!/bin/bash
set -e -x

cd $(dirname $0)/..

. ./hack/version.sh

airgap_image_file='hack/airgap/image-list.txt'
images=$(cat "${airgap_image_file}")
xargs -n1 docker pull <<< "${images}"
docker save ${images} -o dist/artifacts/k8e-airgap-images-${ARCH}.tar
gzip -v -c dist/artifacts/k8e-airgap-images-${ARCH}.tar > dist/artifacts/k8e-airgap-images-${ARCH}.tar.gz
if [ ${ARCH} = amd64 ]; then
  cp "${airgap_image_file}" dist/artifacts/k8e-images.txt
elif [ ${ARCH} = aarch64 ] || [ ${ARCH} = arm64 ]; then
    cp "${airgap_image_file}" dist/artifacts/k8e-images-arm64.txt
fi