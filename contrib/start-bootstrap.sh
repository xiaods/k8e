#!/bin/sh

echo "starting k8e server..."

K8E_NODE_NAME=k8e-55  K8E_TOKEN=ilovek8e /opt/k8e/k8e server --cluster-init >> k8e.log 2>&1 &
