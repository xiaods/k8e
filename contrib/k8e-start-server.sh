#!/bin/sh

echo "start server with agent"

# Notice: --server should bind to bootstrap node ip.
# The value to use for K8E_TOKEN is stored at /var/lib/k8e/k8e/server/node-token on your server node.
K8E_TOKEN=ilovek8e k8e server --server https://172.25.1.55:6443 >> k8e.log 2>&1 &
