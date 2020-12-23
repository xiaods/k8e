#!/bin/bash
set -e

mkdir -p bin dist
if [ -e ./hack/$1 ]; then
    ./hack/"$@"
else
    exec "$@"
fi

chown -R $DAPPER_UID:$DAPPER_GID .