#!/bin/sh

echo "starting k8e server..."

# Notice: --cluster-init will init etcd cluster.
# The value to use for K8E_TOKEN is stored at /var/lib/k8e/k8e/server/node-token on your server node.
K8E_TOKEN=ilovek8e k8e server --cluster-init >> k8e.log 2>&1 &
