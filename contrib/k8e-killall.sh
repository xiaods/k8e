#!/bin/sh

set -x
[ `id -u` = 0 ] || exec sudo $0 $@
for bin in /var/lib/k8e/k8e/data/**/bin/; do
    [ -d $bin ] && export PATH=$bin:$PATH
done
pstree() {
    for pid in $@; do
        echo $pid
        pstree $(ps -o ppid= -o pid= | awk "\$1==$pid {print \$2}")
    done
}
killtree() {
    [ $# -ne 0 ] && kill -9 $(set +x; pstree $@; set -x)
}
killtree $(lsof | sed -e 's/^[^0-9]*//g; s/  */\t/g' | grep -w 'k8e/data/[^/]*/bin/containerd-shim' | cut -f1 | sort -n -u)
do_unmount() {
    MOUNTS=`cat /proc/self/mounts | awk '{print $2}' | grep "^$1" | sort -r`
    if [ -n "${MOUNTS}" ]; then
        umount ${MOUNTS}
    fi
}
do_unmount '/run/k8e'
do_unmount '/var/lib/k8e/k8e'