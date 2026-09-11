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
                '  *"get secret sandbox-e2b -o jsonpath="*) printf "Y2VydA==" ;;\n'
                '  *"get gateway e2b -o jsonpath="*) printf "203.0.113.10" ;;\n'
                '  *" -f -"*) cat >/dev/null ;;\n'
                '  *"--dry-run=client"*) printf "{}\\n" ;;\n'
                'esac\n'
                'exit 0\n'
            )
            kubectl.chmod(0o700)
            curl = temporary / "curl"
            curl.write_text('#!/bin/bash\nprintf "%s\\n" "$*" >> "$MCP_INIT_TEST_LOG"\nprintf "405"\n')
            curl.chmod(0o700)
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
            self.assertIn("-n sandbox-matrix get gateway e2b -o name", calls)
            self.assertNotIn("create secret generic sandbox-apikeys", calls)
            self.assertNotIn("oauth", calls)
            self.assertIn("-n sandbox-matrix create secret tls sandbox-e2b", calls)
            self.assertNotIn("create secret tls mcp-public-tls", calls)
            self.assertIn(str(root / "manifests/sandbox-mcp/deployment.yaml"), calls)
            self.assertIn("rollout status deployment/k8e-mcp", calls)
            self.assertIn("condition=Programmed gateway/e2b", calls)
            self.assertIn('status.listeners[?(@.name=="https")]', calls)
            self.assertIn('conditions[?(@.type=="ResolvedRefs")]', calls)
            self.assertIn("httproute/k8e-mcp", calls)
            self.assertIn("--write-out %{http_code}", calls)
            self.assertIn("https://203.0.113.10/mcp", calls)
            self.assertIn("MCP endpoint: https://203.0.113.10/mcp", applied.stdout)
            self.assertNotIn(credential.read_text(), calls + applied.stdout + applied.stderr)

            manifest = (root / "manifests/sandbox-mcp/deployment.yaml").read_text()
            self.assertIn("kind: HTTPRoute", manifest)
            self.assertIn("kind: ReferenceGrant", manifest)
            self.assertIn("sectionName: https", manifest)
            self.assertIn("value: /mcp", manifest)
            self.assertIn("--listen=:8080", manifest)
            self.assertNotIn("mcp-public-tls", manifest)

            log.write_text("")
            reuse_arguments = ["bash", str(root / "hack/init-sandbox-mcp.sh")]
            for option in ["kubeconfig", "gateway-ca", "gateway-cert", "gateway-key"]:
                reuse_arguments += ["--" + option, str(credential)]
            reused = subprocess.run(reuse_arguments + ["--apply"], env=environment, cwd=temporary, capture_output=True, text=True)
            self.assertEqual(reused.returncode, 0, reused.stderr)
            reuse_calls = log.read_text()
            self.assertIn("-n sandbox-matrix get secret sandbox-e2b -o name", reuse_calls)
            self.assertNotIn("create secret tls sandbox-e2b", reuse_calls)

            invalid_host = subprocess.run(
                reuse_arguments + ["--hostname", "999.1.1.1"],
                env=environment,
                cwd=temporary,
                capture_output=True,
                text=True,
            )
            self.assertEqual(invalid_host.returncode, 2)
            self.assertIn("DNS name or IPv4 address", invalid_host.stderr)

    def test_missing_option_value_fails_before_mutation(self):
        root = Path(__file__).resolve().parents[2]
        result = subprocess.run(["bash", str(root / "hack/init-sandbox-mcp.sh"), "--kubeconfig"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing option value", result.stderr)


if __name__ == "__main__":
    unittest.main()
