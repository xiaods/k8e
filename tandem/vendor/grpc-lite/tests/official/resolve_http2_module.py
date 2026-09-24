#!/usr/bin/env python3
"""Validate the pinned HTTP/2 interop module metadata and print its source directory."""

import json
import sys
from pathlib import Path

MODULE = "github.com/grpc/grpc/tools/http2_interop"
VERSION = "v0.0.0-20260718051024-8542e01ff47e"
SUM = "h1:vMscIt60GwX5lCKg3tzIFy+chYjwxd5hX0G842KABlA="
REQUIRED_FILES = ("go.mod", "http2interop.go", "http2interop_test.go", "s6.5_test.go")


def resolve_module_dir(metadata: dict[str, object]) -> Path:
    expected = {"Path": MODULE, "Version": VERSION, "Sum": SUM}
    for field, value in expected.items():
        if metadata.get(field) != value:
            raise ValueError(
                f"unexpected HTTP/2 module {field}: {metadata.get(field)!r}; expected {value!r}"
            )

    directory = metadata.get("Dir")
    if not isinstance(directory, str) or not directory:
        raise ValueError("HTTP/2 module source directory is empty")

    source = Path(directory)
    if not source.is_absolute():
        raise ValueError(f"HTTP/2 module source directory is not absolute: {directory!r}")

    try:
        source = source.resolve(strict=True)
    except OSError as error:
        raise ValueError(
            f"HTTP/2 module source directory cannot be resolved: {directory!r}: {error}"
        ) from error

    if source == Path(source.anchor):
        raise ValueError(f"refusing root HTTP/2 module source directory: {source}")
    if not source.is_dir():
        raise ValueError(f"HTTP/2 module source path is not a directory: {source}")

    missing = [name for name in REQUIRED_FILES if not (source / name).is_file()]
    if missing:
        raise ValueError(
            f"HTTP/2 module source directory is missing required files: {', '.join(missing)}"
        )

    return source


def main() -> int:
    try:
        metadata = json.load(sys.stdin)
        if not isinstance(metadata, dict):
            raise ValueError("Go module metadata is not a JSON object")
        print(resolve_module_dir(metadata))
    except (json.JSONDecodeError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
