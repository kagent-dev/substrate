# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class InstallPostgres(unittest.TestCase):
    def run_installer(self, args=(), fail=""):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            log = directory / "calls.jsonl"
            kubectl = directory / "kubectl"
            kubectl.write_text(f"#!{sys.executable}\n" + """
import json, os, sys
args = sys.argv[1:]
stdin = sys.stdin.read()
with open(os.environ['CALL_LOG'], 'a') as log:
    log.write(json.dumps({'args': args, 'stdin': stdin}) + '\\n')
if os.environ['FAIL_COMMAND'] and os.environ['FAIL_COMMAND'] in args:
    sys.exit(1)
if 'kustomize' in args or '--dry-run=client' in args:
    print('prepared resource')
""")
            kubectl.chmod(0o755)
            result = subprocess.run(
                ["bash", str(ROOT / "hack/install-postgres.sh"), *args],
                input="", text=True, capture_output=True,
                env={**os.environ, "PATH": f"{directory}:{os.environ['PATH']}",
                     "CALL_LOG": str(log), "FAIL_COMMAND": fail,
                     "KUBECTL_CONTEXT": "test-cluster"},
            )
            calls = [json.loads(line) for line in log.read_text().splitlines()]
            return result, calls

    def test_provisions_database_before_publishing_connections(self):
        for args in ((), ("--kind",)):
            with self.subTest(args=args):
                result, calls = self.run_installer(args)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(all(call["args"][:2] == ["--context", "test-cluster"] for call in calls))
                self.assertEqual(any("kustomize" in call["args"] for call in calls), bool(args))
                if not args:
                    self.assertTrue(any(str(ROOT / "manifests/ate-install/postgres/postgres.yaml") in call["args"] for call in calls))
                rollout = next(i for i, call in enumerate(calls) if "rollout" in call["args"])
                bootstrap = next(i for i, call in enumerate(calls) if "exec" in call["args"])
                self.assertLess(rollout, bootstrap)
                self.assertIn("-i", calls[bootstrap]["args"])
                self.assertIn("--single-transaction", calls[bootstrap]["args"])
                self.assertEqual(calls[bootstrap]["stdin"], (ROOT / "pkg/postgressetup/setup.sql").read_text())
                for role in ("owner", "readwrite"):
                    self.assertIn(f"--set=substrate_{role}_password=", calls[bootstrap]["args"])
                secrets = [(i, call) for i, call in enumerate(calls) if "secret" in call["args"]]
                self.assertEqual(len(secrets), 2)
                for i, call in secrets:
                    self.assertLess(bootstrap, i)
                    self.assertIn("--dry-run=client", call["args"])
                    self.assertIn("ate-system", call["args"])
                for (_, call), role, key in zip(secrets, ("readwrite", "owner"), ("readWriteConnectionString", "ownerConnectionString")):
                    dsn = next(arg for arg in call["args"] if arg.startswith(f"--from-literal={key}="))
                    self.assertIn(f"postgresql://substrate_{role}_user@", dsn)
                    bundle = f"/run/postgres.podcert.ate.dev/substrate_{role}_user.pem"
                    self.assertIn(f"sslcert={bundle}", dsn)
                    self.assertIn(f"sslkey={bundle}", dsn)
                    self.assertIn("sslmode=verify-full", dsn)
                self.assertIn("substrate-postgres-readwrite", secrets[0][1]["args"])
                self.assertIn("substrate-postgres-owner", secrets[1][1]["args"])

    def test_does_not_publish_connections_after_failed_preparation(self):
        for fail in ("kustomize", "rollout", "exec"):
            with self.subTest(fail=fail):
                result, calls = self.run_installer(("--kind",), fail=fail)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any("secret" in call["args"] for call in calls))


if __name__ == "__main__":
    unittest.main()
