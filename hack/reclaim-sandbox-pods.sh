#!/usr/bin/env bash
# Reclaim sandbox-matrix pods that the K8E controller cannot reclaim.
#
# Why this is needed (2026-10-21):
#   A sandboxd panic (files.zig:491 @intCast overflow) kills the Pod.
#   DestroySession then relabels it state=resetting, but detectResetting()
#   only promotes *Running* pods back to warm -- Failed/resetting pods are
#   skipped forever. CheckCapacity counts them against the node's memory
#   budget, so each crash permanently burns one of the ~13 slots on a 4C8G node.
#
# Usage:  ./reclaim.sh            # dry-run (prints what it would delete)
#         ./reclaim.sh --apply    # actually delete
set -uo pipefail

NS="${NS:-sandbox-matrix}"
APPLY=0
[[ "${1:-}" == "--apply" ]] && APPLY=1

# Pods that are dead, or that have been stuck Pending for too long.
mapfile -t VICTIMS < <(
  kubectl get po -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys, datetime
d = json.load(sys.stdin)
now = datetime.datetime.now(datetime.timezone.utc)
pending_age = 600  # seconds a Pending pod may sit before we call it an orphan
for p in d.get("items", []):
    meta, st = p["metadata"], p.get("status", {})
    phase = st.get("phase")
    name = meta["name"]
    state = meta.get("labels", {}).get("sandbox.k8e.io/state", "")
    if phase == "Failed":
        print(f"{name}\tFailed/{state}")
    elif phase == "Pending":
        try:
            created = datetime.datetime.fromisoformat(meta["creationTimestamp"].replace("Z", "+00:00"))
        except Exception:
            continue
        if (now - created).total_seconds() > pending_age:
            print(f"{name}\tPending/{state}/age>{pending_age}s")
'
)

if [[ ${#VICTIMS[@]} -eq 0 ]]; then
  echo "OK: no reclaimable pods in namespace $NS"
  exit 0
fi

for v in "${VICTIMS[@]}"; do
  name="${v%%$'\t'}"
  reason="${v#*$'\t'}"
  if [[ $APPLY -eq 1 ]]; then
    kubectl delete po -n "$NS" "$name" --wait=false --grace-period=0 --force >/dev/null 2>&1 \
      && echo "deleted $name ($reason)" \
      || echo "FAILED  $name ($reason)"
  else
    echo "would delete $name ($reason)"
  fi
done

echo
echo "current pods in $NS:"
kubectl get po -n "$NS" 2>/dev/null | tail -n +2
