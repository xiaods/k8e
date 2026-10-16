#!/usr/bin/env python3
"""Contract tests for .github/workflows/release.yml artifact handling.

The Release workflow must not download "all artifacts": a bare
actions/download-artifact step pulls in every artifact of the run, including
the ``*.dockerbuild`` build records that docker/build-push-action uploads by
default (artifact name = dockerbuild basename, uploaded with skipArchive so it
is a raw, non-zip file). These tests pin the following contracts:

  C1 every actions/download-artifact step is pinned to v8
  C2 every actions/download-artifact step filters by name / artifact-ids /
     pattern (no bare "download everything" step)
  C3 every docker/build-push-action (and docker/bake-action, if ever added)
     job disables the build-record upload (DOCKER_BUILD_RECORD_UPLOAD)
  C4 the publish job, simulated with download-artifact's real destination
     semantics, never extracts a *.dockerbuild file, never has two artifacts
     extract onto the same path, and staging leaves exactly the assets the
     release upload step references
  C5 per-arch checksum files match contrib/install.sh's contract:
     sha256sum-amd64.txt carries the k8e (amd64) digest, sha256sum-arm64.txt
     carries the k8e-arm64 (arm64) digest -- install.sh aborts on mismatch
  C6 every artifact uploaded by a build job is selected by a publish download
     step and vice versa (no orphans, no "artifact not found" at runtime)
  C7 contrib/install.sh's asset/checksum naming still matches the release
     upload list

The simulation mirrors the action sources (actions/download-artifact v8
src/download-artifact.ts and @actions/artifact streamExtract):

  * with ``name``      -> the single artifact extracts flat into ``path``
  * with ``pattern``   -> matching artifacts; flat into ``path`` only when
    merge-multiple is true or exactly one artifact matched, otherwise each
    lands in ``path/<artifact-name>/``
  * without any filter -> every artifact in the run is downloaded
  * extraction is a plain unzip.Extract into the destination directory, so two
    artifacts holding a file with the same relative name collide

Run standalone (``python3 tests/release_workflow_test.py``) or via pytest.
"""

from __future__ import annotations

import fnmatch
import hashlib
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import yaml

REPO = Path(__file__).resolve().parents[1]
WF_PATH = REPO / ".github" / "workflows" / "release.yml"
INSTALL_SH = REPO / "contrib" / "install.sh"

DOWNLOAD_ACTION = "actions/download-artifact"
UPLOAD_ACTION = "actions/upload-artifact"
RELEASE_ACTION = "softprops/action-gh-release"
IMAGE_BUILD_ACTIONS = ("docker/build-push-action", "docker/bake-action")

# Values @docker/actions-toolkit Util.parseBool treats as false
# (docker/build-push-action buildRecordUploadEnabled()).
PARSE_BOOL_FALSE = {"0", "f", "false"}

# Simulated artifact set of one Release workflow run. build-push-action names
# the build-record artifact after the exported dockerbuild basename, so a
# "download all" step picks it up whenever the env is not disabled.
RUN_ARTIFACTS: dict[str, list[str]] = {
    "k8e": ["k8e", "k8e-airgap-images.tar.gz"],
    "k8e-arm64": ["k8e"],
    "sandbox-cli-linux-amd64": ["k8e-sandbox-cli-linux-amd64"],
    "sandbox-cli-linux-arm64": ["k8e-sandbox-cli-linux-arm64"],
    "sandbox-cli-darwin-amd64": ["k8e-sandbox-cli-darwin-amd64"],
    "sandbox-cli-darwin-arm64": ["k8e-sandbox-cli-darwin-arm64"],
    "sandbox-cli-windows-amd64": ["k8e-sandbox-cli-windows-amd64.exe"],
    "q5v2l5tnxfq1d1zlg7d0e3vku.dockerbuild": ["q5v2l5tnxfq1d1zlg7d0e3vku.dockerbuild"],
    "lw8j3k1yfzrr0a3pq7h5d2vb.dockerbuild": ["lw8j3k1yfzrr0a3pq7h5d2vb.dockerbuild"],
}


class WorkflowError(RuntimeError):
    """A release.yml contract violation."""


class VirtualFS(dict):
    """path -> content id (``artifact:member``)."""

    def digest(self, path: str) -> str:
        return hashlib.sha256(self[path].encode()).hexdigest()


def minimatch(pattern: str, name: str) -> bool:
    """Minimatch stand-in: artifact names never contain '/', so fnmatch's '*'
    matches the same names as minimatch's for the patterns used here."""
    return fnmatch.fnmatchcase(name, pattern)


@dataclass
class DownloadStep:
    job: str
    step_name: str
    ref: str
    name: str | None = None
    artifact_ids: str | None = None
    pattern: str | None = None
    merge_multiple: bool = False
    path: str = "."

    @property
    def filtered(self) -> bool:
        return bool(self.name or self.artifact_ids or self.pattern)

    def select(self, artifacts: dict[str, list[str]]) -> list[str]:
        if self.name:
            if self.name not in artifacts:
                raise WorkflowError(
                    f"{self.job}/{self.step_name}: artifact '{self.name}' does not "
                    "exist in the run (the download step would fail)"
                )
            return [self.name]
        if self.artifact_ids:
            return [a for a in artifacts if a in self.artifact_ids]
        if self.pattern:
            matched = [a for a in artifacts if minimatch(self.pattern, a)]
            if not matched:
                raise WorkflowError(
                    f"{self.job}/{self.step_name}: pattern '{self.pattern}' matches "
                    "no artifact in the run"
                )
            return matched
        return list(artifacts)  # the footgun: downloads every artifact

    def extract(self, artifacts: dict[str, list[str]], fs: VirtualFS) -> None:
        selected = self.select(artifacts)
        flat = bool(self.name) or self.merge_multiple or len(selected) == 1
        for artifact in selected:
            root = self.path if flat else f"{self.path}/{artifact}"
            for member in artifacts[artifact]:
                dest = f"{root}/{member}"
                if dest in fs:
                    raise WorkflowError(
                        f"{self.job}/{self.step_name}: artifacts '{fs[dest]}' and "
                        f"'{artifact}:{member}' both extract to {dest} "
                        "(two artifacts hold the same relative file name)"
                    )
                fs[dest] = f"{artifact}:{member}"


class Shell:
    """Tiny executor for the mutating shell lines the publish job uses.

    Each ``execute()`` call is one workflow ``run:`` block, so the working
    directory resets to the repository root just like a real Actions step.
    """

    READ_ONLY = re.compile(
        r"^\s*(echo|cat|ls|cd|info|export|set|if|fi|for|done|chmod|chown|sudo|https_fetch)( |$)"
    )
    MV = re.compile(r"^\s*mv\s+(?:-\S+\s+)?(\S+)\s+(\S+)\s*$")
    CP = re.compile(r"^\s*cp\s+(?:-\S+\s+)*(\S+)\s+(\S+)\s*$")
    RM = re.compile(r"^\s*rm\s+(?:-\S+\s+)+(\S+(?:\s+\S+)*)\s*$")
    SHA = re.compile(
        r"^\s*sha256sum\s+(\S+)"
        r"(?:\s+2>/dev/null)?"
        r"(?:\s*\|\s*(?:sort|awk\s+'[^']*'))*"
        r"\s*>\s*(\S+)\s*$"
    )

    def __init__(self, fs: VirtualFS) -> None:
        self.fs = fs
        self.cwd = ""
        self.checksum_records: dict[str, list[tuple[str, str]]] = {}

    def _res(self, path: str) -> str:
        return path if path.startswith("/") else f"{self.cwd}/{path}".lstrip("/")

    def execute(self, script: str) -> None:
        self.cwd = ""
        for raw in script.splitlines():
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if self.READ_ONLY.match(line):
                if line.startswith("cd "):
                    target = line.split(None, 1)[1].strip()
                    self.cwd = "" if target == "." else self._res(target)
                continue
            mv = self.MV.match(line)
            cp = self.CP.match(line)
            if mv or cp:
                src = self._res((mv or cp).group(1))
                dst = self._res((mv or cp).group(2))
                if src not in self.fs:
                    raise WorkflowError(
                        f"`{line}` failed: {src} does not exist "
                        "(download layout does not match the shell step)"
                    )
                content = self.fs.pop(src) if mv else self.fs[src]
                self.fs[dst] = content
                continue
            rm = self.RM.match(line)
            if rm:
                for target in rm.group(1).split():
                    p = self._res(target)
                    for victim in [q for q in self.fs if q == p or q.startswith(p + "/")]:
                        del self.fs[victim]
                continue
            sha = self.SHA.match(line)
            if sha:
                src, dst = self._res(sha.group(1)), self._res(sha.group(2))
                if "*" in src:
                    entries = [
                        (self.fs.digest(q), q.rsplit("/", 1)[-1])
                        for q in sorted(p for p in self.fs if fnmatch.fnmatchcase(p, src))
                    ]
                else:
                    if src not in self.fs:
                        raise WorkflowError(f"`{line}` failed: {src} does not exist")
                    entries = [(self.fs.digest(src), src.rsplit("/", 1)[-1])]
                self.fs[dst] = f"generated:{dst}"
                self.checksum_records[dst] = sorted(entries)
                continue
            if ">" in line:
                continue  # informational redirect (echo/cat ... > file)
            raise WorkflowError(
                f"unsupported mutating shell line (extend the test executor): `{line}`"
            )


# --- workflow parsing --------------------------------------------------------


def load_workflow() -> dict:
    with WF_PATH.open(encoding="utf-8") as fh:
        return yaml.safe_load(fh)


def iter_steps(workflow: dict):
    for job_id, job in (workflow.get("jobs") or {}).items():
        for step in job.get("steps") or []:
            yield job_id, job, step


def step_label(job_id: str, step: dict) -> str:
    return str(step.get("name") or step.get("uses") or step.get("run", "")[:24])


def download_steps(workflow: dict) -> list[DownloadStep]:
    steps: list[DownloadStep] = []
    for job_id, _job, step in iter_steps(workflow):
        uses = step.get("uses") or ""
        if not uses.startswith(DOWNLOAD_ACTION):
            continue
        with_ = step.get("with") or {}
        steps.append(
            DownloadStep(
                job=job_id,
                step_name=step_label(job_id, step),
                ref=uses.split("@", 1)[1],
                name=with_.get("name"),
                artifact_ids=with_.get("artifact-ids"),
                pattern=with_.get("pattern"),
                merge_multiple=bool(with_.get("merge-multiple", False)),
                path=with_.get("path") or ".",
            )
        )
    return steps


def uploaded_artifacts(workflow: dict) -> dict[str, list[str]]:
    out: dict[str, list[str]] = {}
    for job_id, _job, step in iter_steps(workflow):
        uses = step.get("uses") or ""
        if not uses.startswith(UPLOAD_ACTION):
            continue
        with_ = step.get("with") or {}
        name = with_["name"]
        path = with_["path"]
        if name in RUN_ARTIFACTS:
            out[name] = RUN_ARTIFACTS[name]
        else:
            out[name] = [path.rsplit("/", 1)[-1]]
    return out


def release_upload_files(workflow: dict) -> list[str]:
    publish = workflow["jobs"]["publish"]
    release = next(s for s in publish["steps"] if (s.get("uses") or "").startswith(RELEASE_ACTION))
    return [ln.strip() for ln in (release["with"]["files"] or "").splitlines() if ln.strip()]


def simulate_publish_downloads(workflow: dict) -> tuple[VirtualFS, list[DownloadStep]]:
    fs = VirtualFS()
    steps = [s for s in download_steps(workflow) if s.job == "publish"]
    assert steps, "publish job has no actions/download-artifact step"
    for step in steps:
        step.extract(RUN_ARTIFACTS, fs)
    return fs, steps


def run_publish_shell(workflow: dict, fs: VirtualFS) -> Shell:
    shell = Shell(fs)
    publish = workflow["jobs"]["publish"]
    for step in publish.get("steps") or []:
        run = step.get("run")
        if run:
            shell.execute(run)
    return shell


# --- contract checks ---------------------------------------------------------


def check_c1_download_version(workflow: dict) -> None:
    steps = download_steps(workflow)
    assert steps, "no actions/download-artifact step found in release.yml"
    for step in steps:
        assert re.fullmatch(r"v8(\.\d+\.\d+)?", step.ref), (
            f"{step.job}/{step.step_name}: actions/download-artifact is pinned to "
            f"@{step.ref}, expected the current v8 major"
        )


def check_c2_download_filtered(workflow: dict) -> None:
    for step in download_steps(workflow):
        assert step.filtered, (
            f"{step.job}/{step.step_name}: no name/artifact-ids/pattern given; the "
            "step downloads every artifact in the run, including *.dockerbuild "
            "build records uploaded by the image jobs"
        )


def check_c3_no_build_record_upload(workflow: dict) -> None:
    found = False
    for job_id, job, step in iter_steps(workflow):
        uses = step.get("uses") or ""
        if not any(uses.startswith(a) for a in IMAGE_BUILD_ACTIONS):
            continue
        found = True
        env = {**(job.get("env") or {}), **(step.get("env") or {})}
        value = env.get("DOCKER_BUILD_RECORD_UPLOAD")
        assert value is not None, (
            f"{job_id}: uses {uses} without DOCKER_BUILD_RECORD_UPLOAD, so the "
            "action uploads a *.dockerbuild build-record artifact into the run"
        )
        assert str(value).lower() in PARSE_BOOL_FALSE, (
            f"{job_id}: DOCKER_BUILD_RECORD_UPLOAD={value!r} is not a value "
            "@docker/actions-toolkit parseBool treats as false"
        )
    assert found, "expected at least one docker build action in release.yml"


def check_c4_publish_layout(workflow: dict) -> None:
    fs, _steps = simulate_publish_downloads(workflow)
    run_publish_shell(workflow, fs)

    for path in fs:
        assert not path.endswith(".dockerbuild"), (
            f"publish job downloaded a *.dockerbuild build record: {path}"
        )

    files = release_upload_files(workflow)
    assert files, "release upload step lists no files"
    missing = [f for f in files if f not in fs]
    assert not missing, (
        f"release upload references files the publish job never staged: {missing}"
    )


def check_c5_per_arch_checksums(workflow: dict) -> None:
    fs, _steps = simulate_publish_downloads(workflow)
    shell = run_publish_shell(workflow, fs)

    assert fs.get("release-artifacts/k8e") and fs.get("release-artifacts/k8e-arm64"), (
        "publish job did not stage both the k8e and k8e-arm64 binaries"
    )
    amd64_digest = fs.digest("release-artifacts/k8e")
    arm64_digest = fs.digest("release-artifacts/k8e-arm64")
    assert amd64_digest != arm64_digest, (
        "fixture collapse: the amd64 and arm64 binaries must hash differently"
    )
    amd64_rec = shell.checksum_records.get("release-artifacts/sha256sum-amd64.txt")
    arm64_rec = shell.checksum_records.get("release-artifacts/sha256sum-arm64.txt")
    assert amd64_rec, "publish job never generated sha256sum-amd64.txt"
    assert arm64_rec, "publish job never generated sha256sum-arm64.txt"
    assert [d for d, _ in amd64_rec] == [amd64_digest], (
        f"sha256sum-amd64.txt {amd64_rec} is not the digest of the amd64 k8e binary"
    )
    assert [d for d, _ in arm64_rec] == [arm64_digest], (
        f"sha256sum-arm64.txt {arm64_rec} is not the digest of the arm64 k8e binary; "
        "install.sh greps k8e-arm64 there and aborts the install on mismatch"
    )


def check_c6_artifact_coverage(workflow: dict) -> None:
    uploaded = set(uploaded_artifacts(workflow))
    selected = set()
    for step in download_steps(workflow):
        if step.job == "publish":
            selected.update(step.select(RUN_ARTIFACTS))
    assert selected <= uploaded, (
        "publish downloads artifacts no job uploads (would fail at runtime): "
        f"{sorted(selected - uploaded)}"
    )
    assert uploaded <= selected, (
        f"uploaded artifacts the publish job never selects (orphaned): "
        f"{sorted(uploaded - selected)}"
    )


def check_c7_install_sh_contract(workflow: dict) -> None:
    text = INSTALL_SH.read_text(encoding="utf-8")
    assert "sha256sum-${ARCH}.txt" in text, "install.sh checksum URL contract changed"
    assert 'grep "${bin_name}"' in text, "install.sh grep contract changed"
    assert 'SUFFIX="-${ARCH}"' in text, "install.sh arch suffix mapping changed"
    files = release_upload_files(workflow)
    assert "release-artifacts/k8e" in files, "release assets lost the amd64 k8e binary"
    assert "release-artifacts/k8e-arm64" in files, (
        "release assets lost the k8e-arm64 binary install.sh fetches on aarch64"
    )


CHECKS: list[tuple[str, Callable[[dict], None]]] = [
    ("C1 download-artifact pinned to v8", check_c1_download_version),
    ("C2 download steps filter by name/pattern", check_c2_download_filtered),
    ("C3 docker build jobs disable build-record upload", check_c3_no_build_record_upload),
    ("C4 publish layout: no dockerbuild, no collisions, all assets staged", check_c4_publish_layout),
    ("C5 per-arch checksums carry the right digest", check_c5_per_arch_checksums),
    ("C6 uploaded artifacts == selected artifacts", check_c6_artifact_coverage),
    ("C7 install.sh <-> release asset contract", check_c7_install_sh_contract),
]


def run_all() -> int:
    workflow = load_workflow()
    failures = []
    print(f"checking {WF_PATH}")
    for name, check in CHECKS:
        try:
            check(workflow)
            print(f"  PASS  {name}")
        except (AssertionError, WorkflowError) as exc:
            failures.append((name, exc))
            print(f"  FAIL  {name}\n        {exc}")
    print()
    if failures:
        print(f"{len(failures)} contract(s) violated in {WF_PATH}")
        return 1
    print(f"all {len(CHECKS)} release.yml artifact contracts hold")
    return 0


def test_all_contracts() -> None:
    workflow = load_workflow()
    for _name, check in CHECKS:
        check(workflow)


if __name__ == "__main__":
    sys.exit(run_all())
