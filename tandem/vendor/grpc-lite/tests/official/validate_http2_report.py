#!/usr/bin/env python3

import json
import sys
from pathlib import Path


REQUIRED_PASSES = {
    "TestSoonAllSettingsFramesAcked",
    "TestSoonClientPrefaceWithStreamId",
    "TestSoonClientShortSettings",
    "TestSoonShortPreface",
    "TestSoonSmallMaxFrameSize",
    "TestSoonTLSApplicationProtocol",
    "TestSoonTLSBadCipherSuites",
    "TestSoonTLSMaxVersion",
    "TestSoonUnknownFrameType",
}


def load_reports(path: Path) -> list[dict]:
    reports = [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.startswith('{"cases":')
    ]
    if not reports:
        raise ValueError("HTTP/2 report JSON was not found")
    return reports


def validate(reports: list[dict]) -> list[str]:
    entries_by_name: dict[str, list[dict]] = {}
    errors: list[str] = []
    for report in reports:
        entries = report.get("cases")
        if not isinstance(entries, list):
            errors.append("HTTP/2 report does not contain a cases list")
            continue
        for entry in entries:
            entries_by_name.setdefault(entry.get("name"), []).append(entry)

    errors.extend(
        f"missing expected case: {name}"
        for name in sorted(REQUIRED_PASSES - entries_by_name.keys())
    )

    for name in sorted(REQUIRED_PASSES & entries_by_name.keys()):
        active = [entry for entry in entries_by_name[name] if not entry.get("skipped")]
        if len(active) != 1:
            errors.append(f"required framing case did not run exactly once: {name}")
        elif not active[0].get("passed"):
            errors.append(f"required framing case did not pass: {name}")

    for name in sorted(entries_by_name.keys() - REQUIRED_PASSES):
        active = [entry for entry in entries_by_name[name] if not entry.get("skipped")]
        if len(active) != 1:
            errors.append(f"new framing case did not run exactly once: {name}")
        elif not active[0].get("passed"):
            errors.append(f"new framing case is not passing: {name}")

    return errors


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {Path(sys.argv[0]).name} REPORT", file=sys.stderr)
        return 2

    try:
        reports = load_reports(Path(sys.argv[1]))
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(error, file=sys.stderr)
        return 1

    errors = validate(reports)
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1

    print("HTTP/2 framing and TLS reports validated; all required cases passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
