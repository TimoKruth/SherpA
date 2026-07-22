#!/usr/bin/env python3
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
CHECKS = HERE / "smoke_checks.py"
SMOKE = HERE / "smoke_build.sh"
MAKEFILE = ROOT / "Makefile"
REVISION = "a" * 40
SOURCE_URL = "https://github.com/TimoKruth/SherpA"
APPROVED_ENV = [
    "PATH=/opt/borg/bin:/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "GPG_KEY=7169605F62C751356D054A26A821E680E5FA6305",
    "PYTHON_VERSION=3.13.14",
    "PYTHON_SHA256=639e43243c620a308f968213df9e00f2f8f62332f7adbaa7a7eeb9783057c690",
    "BORG_CACHE_DIR=/data/borg/cache",
    "BORG_CONFIG_DIR=/data/borg/config",
    "BORG_SECURITY_DIR=/data/borg/security",
]


def run_checks(*args, optimize=False):
    command = [sys.executable, "-I"]
    if optimize:
        command.append("-O")
    command.extend([str(CHECKS), *map(str, args)])
    return subprocess.run(command, capture_output=True, text=True, check=False)


class SmokeArchitectureTests(unittest.TestCase):
    def test_real_bind_mount_and_compose_startup_are_exercised(self):
        source = SMOKE.read_text(encoding="utf-8")
        self.assertNotIn("data_volume=", source)
        self.assertNotIn("type=volume,src=$data_volume,dst=/data", source)
        self.assertIn('type=bind,src=$work_dir/data,dst=/data', source)
        self.assertIn('smoke_checks.py layout /data 10001 10001', source)
        self.assertIn('smoke_checks.py runtime-files /fixture 10001 10001', source)
        self.assertRegex(source, r"docker compose .* up .*--no-build")

    def test_builds_use_an_exact_committed_archive(self):
        source = SMOKE.read_text(encoding="utf-8")
        makefile = MAKEFILE.read_text(encoding="utf-8")
        self.assertIn('"$checks_script" archive "$root_dir" "$source_dir"', source)
        self.assertIn('"$source_dir" 2>&1 | tee "$build_log"', source)
        self.assertNotIn('"$root_dir" 2>&1 | tee "$build_log"', source)
        self.assertIn("bash deploy/collector/smoke_build.sh --build-only sherpa-collector:local", makefile)
        self.assertNotIn("docker build --file deploy/collector/Dockerfile", makefile)

    def test_all_host_python_gates_are_isolated(self):
        source = SMOKE.read_text(encoding="utf-8")
        self.assertNotRegex(source, r"(?m)^\s*python3\s+-\s")
        self.assertNotRegex(source, r"(?m)^\s*python3\s+-O\s")
        self.assertIn('python3 -I "$checks_script"', source)


class ProvenanceTests(unittest.TestCase):
    def test_archive_ignores_dirty_tracked_staged_and_relevant_untracked_sources(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "repo"
            destination = Path(temporary) / "context"
            root.mkdir()
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "smoke@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Smoke Test"], check=True)
            (root / "tracked.go").write_text("package fixture\nconst Value = \"committed\"\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "tracked.go"], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)
            expected_revision = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()

            (root / "tracked.go").write_text("package fixture\nconst Value = \"dirty\"\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "tracked.go"], check=True)
            (root / "untracked.go").write_text("package fixture\nconst Untracked = true\n", encoding="utf-8")

            result = run_checks("archive", root, destination)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), expected_revision)
            self.assertEqual((destination / "tracked.go").read_text(encoding="utf-8"), "package fixture\nconst Value = \"committed\"\n")
            self.assertFalse((destination / "untracked.go").exists())
            self.assertFalse((destination / ".git").exists())


class RuntimeMountTests(unittest.TestCase):
    def test_runtime_data_mount_is_an_exact_writable_bind(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data = root / "data"
            data.mkdir()
            inspect_path = root / "runtime.json"
            inspect_path.write_text(json.dumps([{"Mounts": [{
                "Type": "bind",
                "Source": str(data),
                "Destination": "/data",
                "RW": True,
            }]}]), encoding="utf-8")
            result = run_checks("bind", inspect_path, data)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_runtime_data_mount_accepts_docker_desktop_host_mnt_translation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data = root / "data"
            data.mkdir()
            inspect_path = root / "runtime.json"
            inspect_path.write_text(json.dumps([{"Mounts": [{
                "Type": "bind",
                "Source": "/host_mnt" + str(data),
                "Destination": "/data",
                "RW": True,
            }]}]), encoding="utf-8")
            result = run_checks("bind", inspect_path, data)
            self.assertEqual(result.returncode, 0, result.stderr)


class RuntimeFileTests(unittest.TestCase):
    def test_runtime_fixture_permissions_and_ownership_are_exact(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config"
            secrets = root / "secrets"
            config.mkdir(mode=0o755)
            secrets.mkdir(mode=0o700)
            for path in [config / "age-recipient", config / "known_hosts"]:
                path.write_text("safe public fixture\n", encoding="utf-8")
                path.chmod(0o644)
            for path in [secrets / "upload-token", secrets / "storage-ssh-key", secrets / "borg-repository"]:
                path.write_text("safe private fixture\n", encoding="utf-8")
                path.chmod(0o600)
            result = run_checks("runtime-files", root, os.getuid(), os.getgid())
            self.assertEqual(result.returncode, 0, result.stderr)


class ComposeGateTests(unittest.TestCase):
    def test_python_optimization_cannot_disable_a_failing_compose_gate(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "source").mkdir()
            service = {
                "build": {"context": str(root / "source"), "dockerfile": "deploy/collector/Dockerfile", "args": {"SOURCE_REVISION": REVISION}},
                "restart": "unless-stopped",
                "user": "10001:10001",
                "read_only": False,
                "cap_drop": ["ALL"],
                "security_opt": ["no-new-privileges:true"],
                "pids_limit": 128,
                "cpus": 1.0,
                "mem_limit": 1024 * 1024 * 1024,
                "expose": ["8080"],
                "tmpfs": ["/tmp:rw,nosuid,nodev,noexec,size=64m"],
                "healthcheck": {"test": ["CMD", "/usr/local/bin/collector", "healthcheck", "http://127.0.0.1:8080/healthz"], "interval": "30s", "timeout": "5s", "retries": 3, "start_period": "20s"},
                "logging": {"driver": "json-file", "options": {"max-file": "5", "max-size": "10m"}},
                "labels": {
                    "traefik.enable": "true",
                    "traefik.http.routers.sherpa-collector.rule": "Host(`sherpa-collector.kruth-support.de`)",
                    "traefik.http.routers.sherpa-collector.entrypoints": "websecure",
                    "traefik.http.routers.sherpa-collector.tls": "true",
                    "traefik.http.routers.sherpa-collector.tls.certresolver": "letsencrypt",
                    "traefik.http.services.sherpa-collector.loadbalancer.server.port": "8080",
                },
                "volumes": [],
            }
            path = root / "compose.json"
            path.write_text(json.dumps({"services": {"collector": service}}), encoding="utf-8")
            result = run_checks("compose", path, root, REVISION, optimize=True)
            self.assertNotEqual(result.returncode, 0, "invalid read_only=false passed under Python optimization")


class LeakageGateTests(unittest.TestCase):
    def write_inspect(self, root, environment=None, labels=None):
        path = root / "inspect.json"
        path.write_text(json.dumps([{"Config": {"Env": environment or [], "Labels": labels or {
            "org.opencontainers.image.source": SOURCE_URL,
            "org.opencontainers.image.revision": REVISION,
        }}}]), encoding="utf-8")
        return path

    def test_secret_like_history_assignment_is_rejected_without_printing_value(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            sentinel = "safe-review-metadata-secret"
            inspect_path = self.write_inspect(root, environment=APPROVED_ENV)
            history_path = root / "history"
            history_path.write_text(f"RUN OAUTH_CLIENT_SECRET={sentinel} do-something\n", encoding="utf-8")
            result = run_checks("metadata", inspect_path, history_path, SOURCE_URL, REVISION)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("image history", result.stderr)
            self.assertNotIn(sentinel, result.stdout + result.stderr)

    def test_json_style_history_assignment_is_rejected_without_printing_value(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            sentinel = "safe-review-json-secret"
            inspect_path = self.write_inspect(root, environment=APPROVED_ENV)
            history_path = root / "history"
            history_path.write_text(f'RUN metadata={{"api_token": "{sentinel}"}}\n', encoding="utf-8")
            result = run_checks("metadata", inspect_path, history_path, SOURCE_URL, REVISION)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("image history", result.stderr)
            self.assertNotIn(sentinel, result.stdout + result.stderr)

    def test_dotfile_deployment_artifact_path_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive_path = Path(temporary) / "image.tar"
            body = b"SAFE_PUBLIC_SETTING=true\n"
            with tarfile.open(archive_path, "w") as archive:
                info = tarfile.TarInfo("./.env")
                info.size = len(body)
                info.mode = 0o600
                archive.addfile(info, io.BytesIO(body))
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("deployment artifact", result.stderr)

    def test_deleted_layer_secret_content_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            layer_path = root / "layer.tar"
            sentinel = "safe-review-layer-sentinel"
            body = f"-----BEGIN PRIVATE KEY-----\n{sentinel}\n".encode()
            with tarfile.open(layer_path, "w") as layer:
                info = tarfile.TarInfo("tmp/deleted-secret")
                info.size = len(body)
                info.mode = 0o600
                layer.addfile(info, io.BytesIO(body))
            manifest = json.dumps([{"Config": "config.json", "RepoTags": ["fixture:latest"], "Layers": ["layer.tar"]}]).encode()
            config = b"{}"
            save_path = root / "image-save.tar"
            with tarfile.open(save_path, "w") as image_save:
                for name, payload in [("manifest.json", manifest), ("config.json", config), ("layer.tar", layer_path.read_bytes())]:
                    info = tarfile.TarInfo(name)
                    info.size = len(payload)
                    image_save.addfile(info, io.BytesIO(payload))
            result = run_checks("layers", save_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("private-key material", result.stderr)
            self.assertNotIn(sentinel, result.stdout + result.stderr)

    def test_regular_file_private_key_header_and_sentinel_are_rejected_without_printing_content(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive_path = root / "image.tar"
            sentinel = "safe-review-file-sentinel"
            body = f"-----BEGIN OPENSSH PRIVATE KEY-----\n{sentinel}\n".encode()
            with tarfile.open(archive_path, "w") as archive:
                info = tarfile.TarInfo("opt/application/config.txt")
                info.size = len(body)
                info.mode = 0o600
                archive.addfile(info, io.BytesIO(body))
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn(sentinel, result.stdout + result.stderr)

    def test_smoke_scans_saved_image_layers(self):
        source = SMOKE.read_text(encoding="utf-8")
        self.assertIn('docker image save "$image" >"$work_dir/image.save.tar"', source)
        self.assertIn('smoke_checks.py" layers "$work_dir/image.save.tar"', source)

    def test_public_runtime_material_does_not_false_positive(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive_path = root / "image.tar"
            bodies = {
                "etc/ssh/ssh_known_hosts.example": b"public.example ssh-ed25519 AAAAC3NzaSafePublicFixture\n",
                "etc/ssl/certs/public.pem": b"-----BEGIN CERTIFICATE-----\nU2FmZVB1YmxpY0NlcnRpZmljYXRl\n-----END CERTIFICATE-----\n",
                "opt/borg/lib/python3.13/site-packages/borg/testsuite/runtime.py": b"required upstream Borg runtime module\n",
            }
            with tarfile.open(archive_path, "w") as archive:
                for name, body in bodies.items():
                    info = tarfile.TarInfo(name)
                    info.size = len(body)
                    info.mode = 0o644
                    archive.addfile(info, io.BytesIO(body))
            result = run_checks("filesystem", archive_path)
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
