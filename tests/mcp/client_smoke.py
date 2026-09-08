"""Independent MCP 2026-07-28 wire probe; credentials are read from environment."""
import argparse
import json
import os
from pathlib import Path
import ssl
import time
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--ca")
    parser.add_argument("--state", required=True)
    parser.add_argument("--phase", choices=["before", "after", "cleanup"], required=True)
    args = parser.parse_args()
    if not args.endpoint.startswith("https://"):
        parser.error("HTTPS is required")
    context = ssl.create_default_context(cafile=args.ca)
    token = os.environ["MCP_TEST_API_KEY"]

    def rpc(method, name=None, arguments=None):
        params = {"_meta": {
            "io.modelcontextprotocol/protocolVersion": "2026-07-28",
            "io.modelcontextprotocol/clientCapabilities": {},
        }}
        headers = {"Authorization": "Bearer " + token,
                   "Content-Type": "application/json",
                   "Accept": "application/json, text/event-stream",
                   "MCP-Protocol-Version": "2026-07-28", "Mcp-Method": method}
        if name:
            params.update(name=name, arguments=arguments)
            headers["Mcp-Name"] = name
        request_id = str(uuid.uuid4())
        request = urllib.request.Request(args.endpoint, headers=headers, data=json.dumps(
            {"jsonrpc": "2.0", "id": request_id, "method": method, "params": params}).encode())
        with urllib.request.urlopen(request, context=context, timeout=45) as response:
            if response.headers.get("Mcp-Session-Id"):
                raise RuntimeError("unexpected protocol session")
            message = json.load(response)
        if message.get("id") != request_id or "error" in message:
            raise RuntimeError("invalid RPC response: " + json.dumps(message))
        result = message["result"]
        if result.get("resultType") != "complete" or result.get("isError"):
            raise RuntimeError("tool/protocol failure: " + json.dumps(result))
        return result

    def call(name, arguments):
        return rpc("tools/call", name, arguments)["structuredContent"]

    discovery = rpc("server/discover")
    if "2026-07-28" not in discovery["supportedVersions"]:
        raise RuntimeError("version not advertised")
    names = {tool["name"] for tool in rpc("tools/list")["tools"]}
    if not {"sandbox_create", "sandbox_exec", "sandbox_poll", "sandbox_operation"} <= names:
        raise RuntimeError("missing required tools")
    state_path = Path(args.state)
    if args.phase == "before":
        if state_path.exists():
            raise RuntimeError("state file exists; use after or cleanup")
        prefix = "wire-" + uuid.uuid4().hex
        state = {"create": {"operation_id": prefix + "-create"}}
        state["created"] = call("sandbox_create", state["create"])
        state["exec"] = {"session_id": state["created"]["session_id"],
                         "operation_id": prefix + "-exec", "command": "printf mcp-smoke-ok"}
        state_path.write_text(json.dumps(state))
        state["submitted"] = call("sandbox_exec", state["exec"])
        state_path.write_text(json.dumps(state))
    else:
        state = json.loads(state_path.read_text())
    if args.phase == "cleanup":
        call("sandbox_destroy", {"session_id": state["created"]["session_id"],
                                 "operation_id": state["create"]["operation_id"] + "-destroy"})
        print("cleanup passed")
        return
    if call("sandbox_create", state["create"]) != state["created"]:
        raise RuntimeError("create replay changed result")
    if call("sandbox_exec", state["exec"]) != state["submitted"]:
        raise RuntimeError("exec replay changed result")
    recovered = call("sandbox_operation", {"operation_kind": "sandbox_exec",
                                           "operation_id": state["exec"]["operation_id"]})
    if recovered != state["submitted"]:
        raise RuntimeError("operation recovery changed result")
    deadline = time.monotonic() + 60
    while True:
        polled = call("sandbox_poll", {"run_id": recovered["run_id"]})
        if polled["status"] == "completed":
            if int(polled["exit_code"]) != 0:
                raise RuntimeError("command failed")
            break
        if polled["status"] in ("failed", "cancelled") or time.monotonic() >= deadline:
            raise RuntimeError("run did not complete: " + json.dumps(polled))
        time.sleep(1)
    print(args.phase + " passed: discovery, tools, deduplication, recovery, polling")


if __name__ == "__main__":
    main()
