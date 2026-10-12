#!/usr/bin/env bash
set -euo pipefail

executable=$1
output_dir=$2
enable_tcmalloc=$3
cpu_profile="$output_dir/cpu.prof"
heap_prefix="$output_dir/heap"

rm -f "$cpu_profile" "$heap_prefix".*.heap
if [[ "$enable_tcmalloc" == true ]]; then
    CPUPROFILE="$cpu_profile" HEAPPROFILE="$heap_prefix" "$executable"
else
    CPUPROFILE="$cpu_profile" "$executable"
fi
test -s "$cpu_profile"
if [[ "$enable_tcmalloc" == true ]]; then
    compgen -G "$heap_prefix.*.heap" >/dev/null
fi
