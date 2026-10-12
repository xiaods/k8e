#!/usr/bin/env python3

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import resolve_http2_module


class ResolveHttp2ModuleTest(unittest.TestCase):
    def metadata(self, directory: str) -> dict[str, object]:
        return {
            "Path": resolve_http2_module.MODULE,
            "Version": resolve_http2_module.VERSION,
            "Sum": resolve_http2_module.SUM,
            "Dir": directory,
        }

    def create_module(self, root: Path) -> None:
        for name in resolve_http2_module.REQUIRED_FILES:
            (root / name).touch()

    def test_accepts_pinned_module_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            self.create_module(source)
            self.assertEqual(
                resolve_http2_module.resolve_module_dir(self.metadata(directory)),
                source.resolve(),
            )

    def test_rejects_empty_source_directory(self) -> None:
        with self.assertRaisesRegex(ValueError, "source directory is empty"):
            resolve_http2_module.resolve_module_dir(self.metadata(""))

    def test_rejects_root_source_directory(self) -> None:
        with self.assertRaisesRegex(ValueError, "refusing root"):
            resolve_http2_module.resolve_module_dir(self.metadata("/."))

    def test_rejects_unpinned_checksum(self) -> None:
        metadata = self.metadata("/unused")
        metadata["Sum"] = "h1:unexpected"
        with self.assertRaisesRegex(ValueError, "unexpected HTTP/2 module Sum"):
            resolve_http2_module.resolve_module_dir(metadata)


if __name__ == "__main__":
    unittest.main()
