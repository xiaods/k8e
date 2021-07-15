#!/bin/sh

echo "start server with agent"

# Notice: --server should bind to bootstrap node ip.
K8E_TOKEN=ilovek8e k8e server --server https://172.25.1.55:6443 >> k8e.log 2>&1 &
