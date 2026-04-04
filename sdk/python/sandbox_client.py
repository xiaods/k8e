"""
K8E Sandbox Python SDK
Direct gRPC client — no MCP stdio overhead (~1-5ms vs ~500ms).

Install:
    pip install grpcio grpcio-tools protobuf

Generate stubs (once):
    python -m grpc_tools.protoc -I proto \
        --python_out=. --grpc_python_out=. \
        proto/sandbox/v1/sandbox.proto
"""

from __future__ import annotations

import os
import threading
from contextlib import contextmanager
from typing import Generator, Iterator, List, Optional

import grpc
from sandbox.v1 import sandbox_pb2 as pb
from sandbox.v1 import sandbox_pb2_grpc as pb_grpc


_DEFAULT_ENDPOINT = "127.0.0.1:50051"


def _build_channel(endpoint: str) -> grpc.Channel:
    """Build a gRPC channel with TLS auto-discovery (mirrors Go client logic)."""
    cert_path = os.environ.get("K8E_SANDBOX_CERT")
    if cert_path:
        with open(cert_path, "rb") as f:
            creds = grpc.ssl_channel_credentials(root_certificates=f.read())
    else:
        # probe well-known paths
        for path in (
            "/var/lib/k8e/server/tls/serving-kube-apiserver.crt",
            "/etc/k8e/tls/serving-kube-apiserver.crt",
        ):
            if os.path.exists(path):
                with open(path, "rb") as f:
                    creds = grpc.ssl_channel_credentials(root_certificates=f.read())
                break
        else:
            creds = grpc.ssl_channel_credentials()  # system CA pool

    return grpc.secure_channel(endpoint, creds)


class ExecResult:
    __slots__ = ("stdout", "stderr", "exit_code")

    def __init__(self, stdout: str, stderr: str, exit_code: int) -> None:
        self.stdout = stdout
        self.stderr = stderr
        self.exit_code = exit_code

    def __repr__(self) -> str:
        return f"ExecResult(exit_code={self.exit_code}, stdout={self.stdout!r})"


class SandboxClient:
    """
    Long-lived K8E sandbox client. Create once, reuse across calls.

    Usage::

        client = SandboxClient()
        result = client.run("print('hello')", language="python")
        print(result.stdout)
        client.close()

    Or as a context manager::

        with SandboxClient() as client:
            result = client.run("echo hi", language="bash")
    """

    def __init__(
        self,
        endpoint: Optional[str] = None,
        tenant_id: str = "",
    ) -> None:
        ep = endpoint or os.environ.get("K8E_SANDBOX_ENDPOINT", _DEFAULT_ENDPOINT)
        self._channel = _build_channel(ep)
        self._stub = pb_grpc.SandboxServiceStub(self._channel)
        self._tenant_id = tenant_id
        self._session_id: Optional[str] = None
        self._lock = threading.Lock()

    # ── lifecycle ──────────────────────────────────────────────────────────

    def close(self) -> None:
        """Close the gRPC channel and destroy the default session (if any)."""
        with self._lock:
            sid = self._session_id
            tenant = self._tenant_id
            self._session_id = None
        if sid and not tenant:
            try:
                self._stub.DestroySession(pb.DestroySessionRequest(session_id=sid))
            except grpc.RpcError:
                pass
        self._channel.close()

    def __enter__(self) -> "SandboxClient":
        return self

    def __exit__(self, *_) -> None:
        self.close()

    # ── high-level API ─────────────────────────────────────────────────────

    def run(self, code: str, language: str = "bash", timeout: int = 30) -> ExecResult:
        """Run code in the default session (lazily created, reused across calls)."""
        sid = self._default_session()
        return self.exec(sid, _build_command(code, language), timeout)

    def exec(self, session_id: str, command: str, timeout: int = 30) -> ExecResult:
        """Run a command in an explicit session."""
        resp = self._stub.Exec(pb.ExecRequest(
            session_id=session_id,
            command=command,
            timeout=timeout,
            workdir="/workspace",
        ))
        return ExecResult(resp.stdout, resp.stderr, resp.exit_code)

    def exec_stream(self, session_id: str, command: str, timeout: int = 300) -> Iterator[str]:
        """Run a command and yield output chunks as they arrive."""
        for chunk in self._stub.ExecStream(pb.ExecRequest(
            session_id=session_id,
            command=command,
            timeout=timeout,
            workdir="/workspace",
        )):
            yield chunk.chunk

    # ── session management ─────────────────────────────────────────────────

    def create_session(
        self,
        runtime_class: str = "gvisor",
        allowed_hosts: Optional[List[str]] = None,
        tenant_id: str = "",
    ) -> str:
        """Create a new isolated sandbox pod and return its session ID."""
        resp = self._stub.CreateSession(pb.CreateSessionRequest(
            runtime_class=runtime_class,
            allowed_hosts=allowed_hosts or [],
            tenant_id=tenant_id,
        ))
        return resp.session_id

    def destroy_session(self, session_id: str) -> None:
        self._stub.DestroySession(pb.DestroySessionRequest(session_id=session_id))

    # ── file operations ────────────────────────────────────────────────────

    def write_file(self, session_id: str, path: str, content: str) -> None:
        self._stub.WriteFile(pb.WriteFileRequest(
            session_id=session_id, path=path, content=content,
        ))

    def read_file(self, session_id: str, path: str) -> str:
        return self._stub.ReadFile(pb.ReadFileRequest(
            session_id=session_id, path=path,
        )).content

    def list_files(self, session_id: str, since: int = 0):
        return self._stub.ListFiles(pb.ListFilesRequest(
            session_id=session_id, since=since,
        )).files

    # ── extras ────────────────────────────────────────────────────────────

    def pip_install(self, session_id: str, packages: List[str]) -> ExecResult:
        resp = self._stub.PipInstall(pb.PipInstallRequest(
            session_id=session_id, packages=packages,
        ))
        return ExecResult(resp.output, "", resp.exit_code)

    # ── internal ──────────────────────────────────────────────────────────

    def _default_session(self) -> str:
        with self._lock:
            if self._session_id:
                return self._session_id
            sid = self._stub.CreateSession(pb.CreateSessionRequest(
                runtime_class="gvisor",
                tenant_id=self._tenant_id,
            )).session_id
            self._session_id = sid
            return sid


def _build_command(code: str, language: str) -> str:
    lang = language.lower()
    if lang in ("python", "python3"):
        return f"python3 -c {code!r}"
    if lang in ("node", "nodejs"):
        return f"node -e {code!r}"
    return code


@contextmanager
def sandbox_session(
    runtime_class: str = "gvisor",
    allowed_hosts: Optional[List[str]] = None,
    endpoint: Optional[str] = None,
) -> Generator[tuple["SandboxClient", str], None, None]:
    """
    Context manager that creates a dedicated session and cleans up on exit.

    Usage::

        with sandbox_session() as (client, sid):
            client.write_file(sid, "/workspace/main.py", code)
            result = client.exec(sid, "python3 /workspace/main.py")
    """
    client = SandboxClient(endpoint=endpoint)
    sid = client.create_session(runtime_class=runtime_class, allowed_hosts=allowed_hosts)
    try:
        yield client, sid
    finally:
        client.destroy_session(sid)
        client.close()
