#!/bin/sh

echo "start agent"

K8E_NODE_NAME=k8e-57 K8E_TOKEN=ilovek8e /opt/k8e/k8e agent --server https://172.25.1.55:6443 >> k8e.log 2>&1 &