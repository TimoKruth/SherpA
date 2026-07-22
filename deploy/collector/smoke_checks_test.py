#!/usr/bin/env python3
import importlib.util
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
CHECKS_SPEC = importlib.util.spec_from_file_location("collector_smoke_checks", CHECKS)
CHECKS_MODULE = importlib.util.module_from_spec(CHECKS_SPEC)
CHECKS_SPEC.loader.exec_module(CHECKS_MODULE)


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

    def test_compose_cleanup_is_armed_before_startup_and_removes_image_alias(self):
        source = SMOKE.read_text(encoding="utf-8")
        self.assertLess(source.index("compose_started=true"), source.index("up --detach --no-build collector"))
        self.assertIn('image_alias="${project}-collector"', source)
        self.assertIn('docker image rm --force "$image_alias"', source)


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

    def write_filesystem(self, root, path, body):
        archive_path = root / "image.tar"
        with tarfile.open(archive_path, "w") as archive:
            info = tarfile.TarInfo(path)
            info.size = len(body)
            info.mode = 0o600
            archive.addfile(info, io.BytesIO(body))
        return archive_path

    def write_image_save(self, root, path, body):
        layer_path = root / "layer.tar"
        with tarfile.open(layer_path, "w") as layer:
            info = tarfile.TarInfo(path)
            info.size = len(body)
            info.mode = 0o600
            layer.addfile(info, io.BytesIO(body))
        manifest = json.dumps([{"Config": "config.json", "RepoTags": ["fixture:latest"], "Layers": ["layer.tar"]}]).encode()
        save_path = root / "image-save.tar"
        with tarfile.open(save_path, "w") as image_save:
            for name, payload in [("manifest.json", manifest), ("config.json", b"{}"), ("layer.tar", layer_path.read_bytes())]:
                info = tarfile.TarInfo(name)
                info.size = len(payload)
                image_save.addfile(info, io.BytesIO(payload))
        return save_path

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

    def test_arbitrary_gpg_key_history_value_is_rejected_without_printing_value(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            value = "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"
            inspect_path = self.write_inspect(root, environment=APPROVED_ENV)
            history_path = root / "history"
            history_path.write_text(f"ENV GPG_KEY={value}\n", encoding="utf-8")
            result = run_checks("metadata", inspect_path, history_path, SOURCE_URL, REVISION)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("image history", result.stderr)
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_approved_gpg_value_is_only_allowed_in_exact_env_history_form(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            value = APPROVED_ENV[1].split("=", 1)[1]
            inspect_path = self.write_inspect(root, environment=APPROVED_ENV)
            history_path = root / "history"
            history_path.write_text(f"RUN GPG_KEY={value} do-something\n", encoding="utf-8")
            result = run_checks("metadata", inspect_path, history_path, SOURCE_URL, REVISION)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("image history", result.stderr)
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_authorization_bearer_history_is_rejected_without_printing_value(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            value = "safe-review-bearer-history"
            inspect_path = self.write_inspect(root, environment=APPROVED_ENV)
            history_path = root / "history"
            history_path.write_text(f"RUN curl -H 'Authorization: Bearer {value}' example.invalid\n", encoding="utf-8")
            result = run_checks("metadata", inspect_path, history_path, SOURCE_URL, REVISION)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("image history", result.stderr)
            self.assertNotIn(value, result.stdout + result.stderr)

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

    def test_nul_delimited_private_material_is_rejected_directly(self):
        fixtures = {
            header.decode(): (b"prefix\x00" + header + b"\nQUJDREVGR0g=\n", "private-key material")
            for header in CHECKS_MODULE.PRIVATE_HEADERS
        }
        fixtures["age"] = (b"prefix\x00AGE-SECRET-KEY-1TESTFIXTURE\n", "age private identity")
        for name, (body, message) in fixtures.items():
            with self.subTest(name=name):
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_file_contents(body, "opt/application/cache.bin")
                self.assertIn(message, str(failure.exception))
                self.assertNotIn(body.decode(errors="ignore"), str(failure.exception))

    def test_all_nul_delimited_private_headers_are_rejected_from_exported_filesystem(self):
        for header in CHECKS_MODULE.PRIVATE_HEADERS:
            with self.subTest(header=header.decode()), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                marker = "safe-review-all-headers-filesystem"
                body = b"prefix\x00" + header + b"\nQUJDREVGR0g=\n" + marker.encode() + b"\n"
                archive_path = self.write_filesystem(root, "opt/application/cache.bin", body)
                result = run_checks("filesystem", archive_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("private-key material", result.stderr)
                self.assertNotIn(marker, result.stdout + result.stderr)

    def test_all_nul_delimited_private_headers_are_rejected_from_saved_layer(self):
        for header in CHECKS_MODULE.PRIVATE_HEADERS:
            with self.subTest(header=header.decode()), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                marker = "safe-review-all-headers-layer"
                body = b"prefix\x00" + header + b"\nQUJDREVGR0g=\n" + marker.encode() + b"\n"
                save_path = self.write_image_save(root, "tmp/deleted-cache.bin", body)
                result = run_checks("layers", save_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("private-key material", result.stderr)
                self.assertNotIn(marker, result.stdout + result.stderr)

    def test_binary_openssh_private_key_is_rejected_from_exported_filesystem(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = "safe-review-binary-openssh-filesystem"
            body = b"prefix\x00-----BEGIN OPENSSH PRIVATE KEY-----\npayload\n" + marker.encode() + b"\n"
            archive_path = self.write_filesystem(root, "opt/application/cache.bin", body)
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("private-key material", result.stderr)
            self.assertNotIn(marker, result.stdout + result.stderr)

    def test_binary_age_identity_is_rejected_from_exported_filesystem(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = "safe-review-binary-age-filesystem"
            body = b"prefix\x00AGE-SECRET-KEY-1TESTFIXTURE\n" + marker.encode() + b"\n"
            archive_path = self.write_filesystem(root, "opt/application/cache.bin", body)
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("age private identity", result.stderr)
            self.assertNotIn(marker, result.stdout + result.stderr)

    def test_binary_openssh_private_key_is_rejected_from_saved_layer(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = "safe-review-binary-openssh-layer"
            body = b"prefix\x00-----BEGIN OPENSSH PRIVATE KEY-----\npayload\n" + marker.encode() + b"\n"
            save_path = self.write_image_save(root, "tmp/deleted-cache.bin", body)
            result = run_checks("layers", save_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("private-key material", result.stderr)
            self.assertNotIn(marker, result.stdout + result.stderr)

    def test_binary_age_identity_is_rejected_from_saved_layer(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = "safe-review-binary-age-layer"
            body = b"prefix\x00AGE-SECRET-KEY-1TESTFIXTURE\n" + marker.encode() + b"\n"
            save_path = self.write_image_save(root, "tmp/deleted-cache.bin", body)
            result = run_checks("layers", save_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("age private identity", result.stderr)
            self.assertNotIn(marker, result.stdout + result.stderr)

    def test_etc_configuration_assignment_is_rejected_without_printing_value(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            value = "safe-review-etc-api-value"
            archive_path = self.write_filesystem(root, "etc/sherpa/config", f"API_TOKEN={value}\n".encode())
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("etc/sherpa/config", result.stderr)
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_authorization_bearer_file_content_is_rejected_without_printing_value(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            value = "safe-review-bearer-file"
            archive_path = self.write_filesystem(root, "opt/application/client.conf", f"Authorization: Bearer {value}\n".encode())
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("opt/application/client.conf", result.stderr)
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_vendor_configuration_assignments_are_rejected_without_printing_values(self):
        fixtures = {
            "opt/borg/etc/client.conf": b"OAUTH_CLIENT_SECRET=safe-review-borg-config-value\n",
            "usr/local/lib/sherpa/client.conf": b"DATABASE_URL=safe-review-usr-config-value\n",
        }
        for path, body in fixtures.items():
            with self.subTest(path=path), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive_path = self.write_filesystem(root, path, body)
                result = run_checks("filesystem", archive_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(path, result.stderr)
                self.assertNotIn(body.decode().strip().split("=", 1)[1], result.stdout + result.stderr)

    def test_vendor_source_assignments_are_rejected_without_printing_values(self):
        fixtures = {
            "usr/local/lib/python3.13/site-packages/example/config.py": b"UPLOAD_TOKEN=embedded-secret\n",
            "usr/local/lib/python3.13/site-packages/example/database.py": b"DATABASE_PASSWORD=embedded-secret\n",
            "opt/borg/lib/python3.13/site-packages/borg/review_config.py": b"SECRET_KEY=embedded-secret\n",
            "opt/borg/lib/python3.13/site-packages/borg/review_oauth.py": b"OAUTH_CLIENT_SECRET=embedded-secret\n",
            "opt/borg/lib/python3.13/site-packages/borg/review_token.py": b"API_TOKEN=embedded-token\n",
        }
        for path, body in fixtures.items():
            with self.subTest(path=path), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive_path = self.write_filesystem(root, path, body)
                result = run_checks("filesystem", archive_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(path, result.stderr)
                self.assertNotIn(body.decode().strip().split("=", 1)[1], result.stdout + result.stderr)

    def test_known_reviewed_source_rejects_unapproved_sensitive_key(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            path = "usr/local/lib/python3.13/http/server.py"
            value = "safe-review-known-source-token"
            archive_path = self.write_filesystem(root, path, f"UPLOAD_TOKEN={value}\n".encode())
            result = run_checks("filesystem", archive_path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(path, result.stderr)
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_deleted_layer_secret_content_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            sentinel = "safe-review-layer-sentinel"
            body = f"-----BEGIN PRIVATE KEY-----\n{sentinel}\n".encode()
            save_path = self.write_image_save(root, "tmp/deleted-secret", body)
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
                "opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/util/request.py": b"authorization = header\nproxy-authorization: header\n",
                "usr/local/lib/python3.13/http/server.py": b"authorization = header\n",
                "usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/util/request.py": b"authorization = header\nproxy-authorization: header\n",
                "usr/local/lib/python3.13/venv/scripts/common/Activate.ps1": b"Key = Get-Item\n",
                "var/lib/dpkg/info/libc6:arm64.templates": b"ssh: public-package-template\n",
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
