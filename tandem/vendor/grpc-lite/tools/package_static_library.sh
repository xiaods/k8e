#!/usr/bin/env bash
set -euo pipefail

input=$1
output=$2
zig=$3
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/grpc-lite-static.XXXXXX")
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT

object_index=0
payload_index=0
add_object() {
    local source=$1
    cp "$source" "$work_dir/object-$(printf '%08d.o' "$object_index")"
    object_index=$((object_index + 1))
}

flatten_member() {
    local archive=$1
    local member=$2
    local source=$member
    local archive_dir
    archive_dir=$(dirname "$archive")

    if [[ ! -f "$source" && -f "$archive_dir/$member" ]]; then
        source="$archive_dir/$member"
    elif [[ ! -f "$source" ]]; then
        source="$work_dir/payload-$(printf '%08d' "$payload_index")"
        payload_index=$((payload_index + 1))
        "$zig" ar p "$archive" "$member" >"$source"
    fi

    if archive_members=$("$zig" ar t "$source" 2>/dev/null); then
        while IFS= read -r object; do
            flatten_member "$source" "$object"
        done <<<"$archive_members"
    else
        add_object "$source"
    fi
}

while IFS= read -r member; do
    flatten_member "$input" "$member"
done < <("$zig" ar t "$input")

"$zig" ar rcs "$output" "$work_dir"/object-*.o
