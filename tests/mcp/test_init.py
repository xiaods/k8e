import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class InitTests(unittest.TestCase):
    def test_initialization_uses_existing_keys(self):
        root = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as directory:
            temporary = Path(directory)
            log = temporary / "calls"
            kubectl = temporary / "kubectl"
            kubectl.write_text(
                '#!/bin/bash\n'
                'printf "%s\\n" "$*" >> "$MCP_INIT_TEST_LOG"\n'
                'case "$*" in\n'
                '  *" -f -"*) cat >/dev/null ;;\n'
                '  *"--dry-run=client"*) printf "{}\\n" ;;\n'
                'esac\n'
                'exit 0\n'
            )
            kubectl.chmod(0o700)
            credential = temporary / "input"
            credential.write_text("test-only-private-material")
            arguments = ["bash", str(root / "hack/init-sandbox-mcp.sh")]
            for option in ["kubeconfig", "public-cert", "public-key", "gateway-ca", "gateway-cert", "gateway-key"]:
                arguments += ["--" + option, str(credential)]
            environment = dict(os.environ, PATH=str(temporary) + os.pathsep + os.environ["PATH"], MCP_INIT_TEST_LOG=str(log))
            preview = subprocess.run(arguments, env=environment, cwd=temporary, capture_output=True, text=True)
            self.assertEqual(preview.returncode, 0, preview.stderr)
            self.assertFalse(log.exists(), "preview accessed Kubernetes")
            applied = subprocess.run(arguments + ["--apply"], env=environment, cwd=temporary, capture_output=True, text=True)
            self.assertEqual(applied.returncode, 0, applied.stderr)
            calls = log.read_text()
            self.assertIn("-n sandbox-matrix get secret sandbox-apikeys", calls)
            self.assertNotIn("create secret generic sandbox-apikeys", calls)
            self.assertNotIn("oauth", calls)
            self.assertIn(str(root / "manifests/sandbox-mcp/deployment.yaml"), calls)
            self.assertIn("rollout status deployment/k8e-mcp", calls)
            self.assertNotIn(credential.read_text(), calls + applied.stdout + applied.stderr)

    def test_missing_option_value_fails_before_mutation(self):
        root = Path(__file__).resolve().parents[2]
        result = subprocess.run(["bash", str(root / "hack/init-sandbox-mcp.sh"), "--kubeconfig"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing option value", result.stderr)


if __name__ == "__main__":
    unittest.main()
