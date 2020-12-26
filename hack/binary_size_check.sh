#!/bin/sh

set -e

if [ "${DEBUG}" = 1 ]; then
    set -x
fi

MAX_BINARY_SIZE=61000000
SIZE=$(ls -l dist/artifacts/k8e | awk -F ' ' '{print $5}')

if [ ${SIZE} -gt ${MAX_BINARY_SIZE} ]; then
    echo "k8e binary exceeds acceptable size of "${MAX_BINARY_SIZE}
    exit 1
fi

exit 0