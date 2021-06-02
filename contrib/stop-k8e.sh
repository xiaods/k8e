#!/bin/sh
set -e

echo "stop k8e process"

killtree() {
    kill -9 $@ 2>/dev/null
}

getshims() {
    ps axu|grep k8e|grep -v grep|grep -v containerd|awk '{print $2}'
}

killtree $({ set +x; } 2>/dev/null; getshims; set -x)
