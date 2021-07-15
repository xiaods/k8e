#!/bin/sh

echo "starting k8e server..."

# Notice: --cluster-init will init etcd cluster.
K8E_TOKEN=ilovek8e k8e server --cluster-init >> k8e.log 2>&1 &
