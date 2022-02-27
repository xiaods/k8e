#!/bin/bash
set -e

if [ "${DEBUG}" = 1 ]; then
    set -x
fi

cd $(dirname $0)/..

. ./hack/version.sh

MAX_BINARY_SIZE=81000000
BIN_SUFFIX="-${ARCH}"
if [ ${ARCH} = amd64 ]; then
    BIN_SUFFIX=""
elif [ ${ARCH} = aarch64 ] || [ ${ARCH} = arm64 ]; then
    BIN_SUFFIX="-arm64"
fi

CMD_NAME="dist/artifacts/k8e${BIN_SUFFIX}"
SIZE=$(stat -c '%s' ${CMD_NAME})

if [ ${SIZE} -gt ${MAX_BINARY_SIZE} ]; then
    echo "k8e binary ${CMD_NAME} size ${SIZE} exceeds max acceptable size of ${MAX_BINARY_SIZE} bytes"
    exit 1
fi

echo "k8e binary ${CMD_NAME} size ${SIZE} is less than max acceptable size of ${MAX_BINARY_SIZE} bytes"
exit 0