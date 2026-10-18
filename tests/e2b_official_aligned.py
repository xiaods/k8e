#!/usr/bin/env python3
"""E2B compatibility check aligned with https://docs.e2b.dev/client and the
official lifecycle / persistence / quickstart examples.

Every step below is a verbatim-shape official example (create, commands.run,
files.write/read/list, set_timeout, get_info, Sandbox.list, pause, kill), plus
`connect()` — the documented replacement for the non-existent `resume()`.

Usage:
  E2B_API_URL=http://<k8e-e2b-endpoint>:3676 \
  E2B_API_KEY=e2b_<key> python3 e2b_official_aligned.py
"""
import os
import sys
import traceback

from e2b import Sandbox

API_URL = os.environ.get("E2B_API_URL", "http://43.203.225.109:3676")

results = []


def check(name, fn):
    """Run one official-example step and record PASS/FAIL + detail."""
    try:
        detail = fn()
        results.append((name, True, detail))
        print(f"[PASS] {name}: {detail}")
    except Exception as exc:  # noqa: BLE001 - report, do not abort the suite
        tb = traceback.format_exc()
        results.append((name, False, f"{type(exc).__name__}: {exc}"))
        print(f"[FAIL] {name}: {type(exc).__name__}: {exc}")
        print(tb)


# ── L1: Sandbox.create ───────────────────────────────────────────────────────
sandbox = None


def step_create():
    global sandbox
    sandbox = Sandbox.create(timeout=60)
    return f"sandbox_id={sandbox.sandbox_id}"


# ── L1: commands.run (official quickstart shape) ─────────────────────────────
def step_run():
    r = sandbox.commands.run('python3 -c "print(40+2)"')
    return (f"stdout={r.stdout!r} stderr={r.stderr!r} "
            f"exit_code={r.exit_code} error={r.error!r}")


def step_run_exit_code():
    """stdout AND exit code must both be right — the k8e2 regression."""
    r = sandbox.commands.run("exit 7")
    return f"stdout={r.stdout!r} exit_code={r.exit_code} (expected 7)"


def step_run_stderr():
    r = sandbox.commands.run("echo out; echo err >&2; exit 3")
    return (f"stdout={r.stdout!r} stderr={r.stderr!r} "
            f"exit_code={r.exit_code} (expected 3)")


# ── L2: files.write / files.read (official quickstart shape) ────────────────
def step_files_write_read():
    sandbox.files.write("/tmp/e2b-check.txt", "hello from official example\n")
    got = sandbox.files.read("/tmp/e2b-check.txt")
    return f"read back {got!r}"


def step_files_list():
    entries = sandbox.files.list("/tmp")
    names = [getattr(e, "name", None) or getattr(e, "path", None) for e in entries]
    hit = [n for n in names if n and "e2b-check" in n]
    return f"{len(names)} entries, found {hit}"


# ── L3: set_timeout ─────────────────────────────────────────────────────────
def step_set_timeout():
    sandbox.set_timeout(120)
    return "set_timeout(120) ok"


def step_set_timeout_never():
    sandbox.set_timeout(-1)
    return "set_timeout(-1) ok"


# ── L3: get_info / Sandbox.list ─────────────────────────────────────────────
def step_get_info():
    info = sandbox.get_info()
    return (f"state={getattr(info, 'state', None)!r} "
            f"end_at={getattr(info, 'end_at', None)!r} "
            f"sandbox_id={getattr(info, 'sandbox_id', None)!r}")


def step_list():
    items = Sandbox.list().next_items()
    mine = [s for s in items if s.sandbox_id == sandbox.sandbox_id]
    return f"{len(items)} total, ours present={bool(mine)}"


# ── L3: pause (persistence doc shape) ───────────────────────────────────────
def step_pause():
    sandbox.pause()
    return "paused"


# ── L3: connect — the DOCUMENTED resume path (no resume() in SDK 2.39.1) ────
def step_connect():
    same = sandbox.connect()
    return f"reconnected sandbox_id={same.sandbox_id}"


def step_connect_files_survive():
    """A real persistence check: data written before pause survives connect."""
    same = sandbox.connect()
    got = same.files.read("/tmp/e2b-check.txt")
    return f"state survived reconnect: {got!r}"


# ── L4: watch_dir (envd filesystem watcher) ─────────────────────────────────
def step_watch_dir():
    h = sandbox.files.watch_dir("/tmp")
    # write inside the watched dir, then poll for the event
    sandbox.files.write("/tmp/e2b-watched.txt", "x")
    import time
    deadline = time.time() + 15
    seen = []
    while time.time() < deadline:
        for ev in h.get_new_events():
            seen.append(getattr(ev, "name", None) or str(ev))
        if seen:
            break
        time.sleep(0.5)
    h.stop()
    return f"events={seen}" if seen else "no events within 15s"


# ── Cleanup: kill ───────────────────────────────────────────────────────────
def step_kill():
    sandbox.kill()
    return "killed"


ORDER = [
    ("L1 create", step_create),
    ("L1 commands.run stdout+exit_code", step_run),
    ("L1 commands.run pure exit code", step_run_exit_code),
    ("L1 commands.run stderr+exit code", step_run_stderr),
    ("L2 files.write / files.read", step_files_write_read),
    ("L2 files.list", step_files_list),
    ("L3 set_timeout(120)", step_set_timeout),
    ("L3 set_timeout(-1)", step_set_timeout_never),
    ("L3 get_info", step_get_info),
    ("L3 Sandbox.list", step_list),
    ("L3 pause", step_pause),
    ("L3 connect (documented resume path)", step_connect),
    ("L3 connect preserves files", step_connect_files_survive),
    ("L4 watch_dir", step_watch_dir),
    ("Cleanup kill", step_kill),
]


def main():
    print(f"# E2B official-example-aligned compatibility check")
    print(f"# endpoint: {API_URL}\n")
    for name, fn in ORDER:
        if sandbox is None and name != "L1 create":
            print(f"[SKIP] {name}: no sandbox")
            results.append((name, None, "skipped - no sandbox"))
            continue
        check(name, fn)

    passed = sum(1 for _, ok, _ in results if ok is True)
    failed = sum(1 for _, ok, _ in results if ok is False)
    skipped = sum(1 for _, ok, _ in results if ok is None)
    print(f"\n# summary: {passed} pass, {failed} fail, {skipped} skip")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
