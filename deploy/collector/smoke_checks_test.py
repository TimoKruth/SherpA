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

    def add_tar_member(self, archive, path, body=b"", member_type=tarfile.REGTYPE, linkname=""):
        info = tarfile.TarInfo(path)
        info.type = member_type
        info.linkname = linkname
        info.mode = 0o600
        if member_type == tarfile.REGTYPE:
            info.size = len(body)
            archive.addfile(info, io.BytesIO(body))
        else:
            archive.addfile(info)

    def write_filesystem(self, root, path, body=b"", member_type=tarfile.REGTYPE, linkname=""):
        archive_path = root / "image.tar"
        with tarfile.open(archive_path, "w") as archive:
            self.add_tar_member(archive, path, body, member_type, linkname)
        return archive_path

    def write_image_save(self, root, path, body=b"", member_type=tarfile.REGTYPE, linkname=""):
        return self.write_image_save_layers(root, [[(path, body, member_type, linkname)]])

    def write_image_save_layers(self, root, layers):
        layer_payloads = []
        for index, members in enumerate(layers):
            layer_name = f"layer-{index}.tar"
            layer_path = root / layer_name
            with tarfile.open(layer_path, "w") as layer:
                for path, body, member_type, linkname in members:
                    self.add_tar_member(layer, path, body, member_type, linkname)
            layer_payloads.append((layer_name, layer_path.read_bytes()))
        manifest = json.dumps([{
            "Config": "config.json",
            "RepoTags": ["fixture:latest"],
            "Layers": [name for name, _ in layer_payloads],
        }]).encode()
        save_path = root / "image-save.tar"
        with tarfile.open(save_path, "w") as image_save:
            for name, payload in [("manifest.json", manifest), ("config.json", b"{}"), *layer_payloads]:
                info = tarfile.TarInfo(name)
                info.size = len(payload)
                image_save.addfile(info, io.BytesIO(payload))
        return save_path

    def write_scanner_archive(self, scanner, root, path, body=b"", member_type=tarfile.REGTYPE, linkname=""):
        if scanner == "filesystem":
            return self.write_filesystem(root, path, body, member_type, linkname)
        return self.write_image_save(root, path, body, member_type, linkname)

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

    def test_nul_delimited_sensitive_assignments_are_rejected_directly(self):
        keys = (
            "UPLOAD_TOKEN",
            "DATABASE_PASSWORD",
            "SECRET_KEY",
            "OAUTH_CLIENT_SECRET",
            "THIRD_PARTY_API_KEY",
            "NEW_VENDOR_CREDENTIAL",
        )
        for key in keys:
            with self.subTest(key=key):
                value = f"safe-review-direct-{key.lower()}"
                body = b"binary-prefix\x00" + f"{key}={value}".encode() + b"\x00binary-suffix"
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_file_contents(body, "opt/application/cache.bin")
                self.assertIn("secret-like assignment", str(failure.exception))
                self.assertNotIn(value, str(failure.exception))

    def test_nul_delimited_sensitive_assignments_are_rejected_from_exported_filesystem(self):
        keys = (
            "UPLOAD_TOKEN",
            "DATABASE_PASSWORD",
            "SECRET_KEY",
            "OAUTH_CLIENT_SECRET",
            "THIRD_PARTY_API_KEY",
            "NEW_VENDOR_CREDENTIAL",
        )
        for key in keys:
            with self.subTest(key=key), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                value = f"safe-review-filesystem-{key.lower()}"
                body = b"binary-prefix\x00" + f"{key}={value}".encode() + b"\x00binary-suffix"
                archive_path = self.write_filesystem(root, "opt/application/cache.bin", body)
                result = run_checks("filesystem", archive_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("secret-like assignment", result.stderr)
                self.assertNotIn(value, result.stdout + result.stderr)

    def test_nul_delimited_sensitive_assignments_are_rejected_from_saved_layer(self):
        keys = (
            "UPLOAD_TOKEN",
            "DATABASE_PASSWORD",
            "SECRET_KEY",
            "OAUTH_CLIENT_SECRET",
            "THIRD_PARTY_API_KEY",
            "NEW_VENDOR_CREDENTIAL",
        )
        for key in keys:
            with self.subTest(key=key), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                value = f"safe-review-layer-{key.lower()}"
                body = b"binary-prefix\x00" + f"{key}={value}".encode() + b"\x00binary-suffix"
                save_path = self.write_image_save(root, "tmp/deleted-cache.bin", body)
                result = run_checks("layers", save_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("secret-like assignment", result.stderr)
                self.assertNotIn(value, result.stdout + result.stderr)

    def test_long_sensitive_assignment_keys_are_rejected_directly(self):
        fixtures = (
            ("uPlOaD-tOkEn-" + "a" * (129 - len("uPlOaD-tOkEn-")), "="),
            ("b" * 600 + ".Client-Secret." + "c" * 600, ":"),
            ("d" * 32768 + "_UPLOAD_TOKEN", "="),
        )
        for key, separator in fixtures:
            with self.subTest(length=len(key), separator=separator):
                value = f"safe-review-long-direct-{len(key)}"
                body = f"{key}{separator}{value}".encode()
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_file_contents(body, "opt/application/cache.bin")
                self.assertIn("secret-like assignment", str(failure.exception))
                self.assertNotIn(value, str(failure.exception))
                self.assertNotIn(key, str(failure.exception))

    def test_long_sensitive_assignment_keys_are_rejected_by_direct_link_helper(self):
        keys = (
            "uPlOaD-tOkEn-" + "a" * (129 - len("uPlOaD-tOkEn-")),
            "b" * 600 + ".Client-Secret." + "c" * 600,
            "d" * 32768 + "_UPLOAD_TOKEN",
        )
        for key in keys:
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(length=len(key), link_type=link_type):
                    value = f"safe-review-long-link-{len(key)}-{link_type.decode()}"
                    member = tarfile.TarInfo("usr/local/bin/runtime-link")
                    member.type = link_type
                    member.linkname = f"../runtime/{key}={value}"
                    with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                        CHECKS_MODULE.check_link_target(member, member.name, "fixture link target")
                    self.assertIn("secret-like assignment", str(failure.exception))
                    self.assertNotIn(value, str(failure.exception))
                    self.assertNotIn(key, str(failure.exception))

    def test_long_sensitive_assignment_keys_are_rejected_from_exported_filesystem(self):
        fixtures = (
            "uPlOaD-tOkEn-" + "a" * (129 - len("uPlOaD-tOkEn-")),
            "b" * 600 + ".Client-Secret." + "c" * 600,
            "d" * 32768 + "_UPLOAD_TOKEN",
        )
        for key in fixtures:
            for member_type in (tarfile.REGTYPE, tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(length=len(key), member_type=member_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    value = f"safe-review-long-filesystem-{len(key)}-{member_type.decode()}"
                    assignment = f"{key}={value}"
                    archive_path = self.write_filesystem(
                        root,
                        "opt/application/cache.bin" if member_type == tarfile.REGTYPE else "usr/local/bin/runtime-link",
                        body=assignment.encode() if member_type == tarfile.REGTYPE else b"",
                        member_type=member_type,
                        linkname=f"../runtime/{assignment}" if member_type != tarfile.REGTYPE else "",
                    )
                    result = run_checks("filesystem", archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("secret-like assignment", result.stderr)
                    self.assertNotIn(value, result.stdout + result.stderr)
                    self.assertNotIn(key, result.stdout + result.stderr)

    def test_long_sensitive_assignment_keys_are_rejected_from_every_saved_layer(self):
        keys = (
            "uPlOaD-tOkEn-" + "a" * (129 - len("uPlOaD-tOkEn-")),
            "b" * 600 + ".Client-Secret." + "c" * 600,
            "d" * 32768 + "_UPLOAD_TOKEN",
        )
        benign_layer = [("opt/application/public.txt", b"public fixture\n", tarfile.REGTYPE, "")]
        for layer_index, key in enumerate(keys):
            for member_type in (tarfile.REGTYPE, tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(layer=layer_index, length=len(key), member_type=member_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    value = f"safe-review-long-layer-{layer_index}-{member_type.decode()}"
                    assignment = f"{key}={value}"
                    malicious = [(
                        "tmp/deleted-cache.bin" if member_type == tarfile.REGTYPE else "usr/local/bin/runtime-link",
                        assignment.encode() if member_type == tarfile.REGTYPE else b"",
                        member_type,
                        f"../runtime/{assignment}" if member_type != tarfile.REGTYPE else "",
                    )]
                    layers = [benign_layer, benign_layer, benign_layer]
                    layers[layer_index] = malicious
                    save_path = self.write_image_save_layers(root, layers)
                    result = run_checks("layers", save_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("secret-like assignment", result.stderr)
                    self.assertNotIn(value, result.stdout + result.stderr)
                    self.assertNotIn(key, result.stdout + result.stderr)

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

    def test_allowlisted_member_paths_do_not_allow_sensitive_link_assignments(self):
        for scanner in ("filesystem", "layers"):
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(scanner=scanner, link_type=link_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    value = f"safe-review-allowlisted-{scanner}-{link_type.decode()}"
                    target = f"../secret={value}"
                    archive_path = self.write_scanner_archive(
                        scanner,
                        root,
                        "etc/ssl/openssl.cnf",
                        member_type=link_type,
                        linkname=target,
                    )
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("secret-like assignment", result.stderr)
                    self.assertIn("link target", result.stderr)
                    self.assertNotIn(target, result.stdout + result.stderr)
                    self.assertNotIn(value, result.stdout + result.stderr)

    def test_path_prefixed_private_material_in_link_targets_is_rejected(self):
        materials = [
            (header.decode(), "private-key material")
            for header in CHECKS_MODULE.PRIVATE_HEADERS
        ]
        materials.append(("AGE-SECRET-KEY-1TESTFIXTURE", "age private identity"))
        for scanner in ("filesystem", "layers"):
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                for prefix in ("../", "./", "/"):
                    for material, message in materials:
                        with self.subTest(
                            scanner=scanner,
                            link_type=link_type,
                            prefix=prefix,
                            material=material,
                        ), tempfile.TemporaryDirectory() as temporary:
                            root = Path(temporary)
                            target = f"{prefix}{material}\nQUJDREVGR0g="
                            archive_path = self.write_scanner_archive(
                                scanner,
                                root,
                                "usr/local/bin/runtime-link",
                                member_type=link_type,
                                linkname=target,
                            )
                            result = run_checks(scanner, archive_path)
                            self.assertNotEqual(result.returncode, 0)
                            self.assertIn(message, result.stderr)
                            self.assertIn("link target", result.stderr)
                            self.assertNotIn(target, result.stdout + result.stderr)
                            self.assertNotIn(material, result.stdout + result.stderr)

    def test_standalone_bearer_credentials_are_rejected_directly(self):
        fixtures = (
            b"Bearer safe-review-standalone-direct",
            b"binary-prefix\x00bearer safe-review-standalone-nul\x00suffix",
            b"first line\nBEARER safe-review-standalone-newline\n",
            b"../Bearer safe-review-standalone-path",
        )
        for body in fixtures:
            value = body.split(b"Bearer ")[-1].split(b"bearer ")[-1].split(b"BEARER ")[-1].split(b"\x00", 1)[0].split(b"\n", 1)[0]
            with self.subTest(body=body[:24]):
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_file_contents(body, "opt/application/cache.bin")
                self.assertIn("bearer credential", str(failure.exception))
                self.assertNotIn(value.decode(), str(failure.exception))
        for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
            for target in (
                "../Bearer safe-review-standalone-link-path",
                "./prefix\nbearer safe-review-standalone-link-newline",
                "/BEARER safe-review-standalone-link-absolute",
            ):
                with self.subTest(link_type=link_type, target=target[:24]):
                    member = tarfile.TarInfo("usr/local/bin/runtime-link")
                    member.type = link_type
                    member.linkname = target
                    with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                        CHECKS_MODULE.check_link_target(member, member.name, "fixture link target")
                    self.assertIn("bearer credential", str(failure.exception))
                    self.assertNotIn(target, str(failure.exception))

    def test_quoted_and_vocabulary_bearer_credentials_are_rejected_directly(self):
        fixtures = (
            'Bearer "safe-review-value"',
            "Bearer 'safe-review-value'",
            "Bearer token",
            "Bearer TOKEN,",
        )
        for text in fixtures:
            with self.subTest(text=text):
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_assignments(text, "image history")
                self.assertIn("bearer credential", str(failure.exception))
                self.assertNotIn(text.split(" ", 1)[1], str(failure.exception))
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_file_contents(text.encode(), "opt/application/cache.bin")
                self.assertIn("bearer credential", str(failure.exception))
                self.assertNotIn(text.split(" ", 1)[1], str(failure.exception))

        for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
            for target in fixtures:
                with self.subTest(link_type=link_type, target=target):
                    member = tarfile.TarInfo("usr/local/bin/runtime-link")
                    member.type = link_type
                    member.linkname = f"../{target}"
                    with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                        CHECKS_MODULE.check_link_target(member, member.name, "fixture link target")
                    self.assertIn("bearer credential", str(failure.exception))
                    self.assertNotIn(member.linkname, str(failure.exception))
                    self.assertNotIn("safe-review-value", str(failure.exception))

    def test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_metadata(self):
        fixtures = (
            'Bearer "safe-review-value"',
            "Bearer 'safe-review-value'",
            "Bearer token",
            "Bearer TOKEN,",
        )
        for text in fixtures:
            with self.subTest(text=text), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                inspect_path = self.write_inspect(root, environment=APPROVED_ENV)
                history_path = root / "history"
                history_path.write_text(f"RUN review {text}\n", encoding="utf-8")
                result = run_checks("metadata", inspect_path, history_path, SOURCE_URL, REVISION)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("bearer credential", result.stderr)
                self.assertNotIn(text.split(" ", 1)[1], result.stdout + result.stderr)

    def test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_exported_filesystem(self):
        fixtures = (
            (tarfile.REGTYPE, b'prefix\x00Bearer "safe-review-value"\n', ""),
            (tarfile.SYMTYPE, b"", "../Bearer token"),
            (tarfile.LNKTYPE, b"", "/prefix\nBearer TOKEN,"),
        )
        for member_type, body, target in fixtures:
            with self.subTest(member_type=member_type), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive_path = self.write_filesystem(
                    root,
                    "opt/application/cache.bin" if member_type == tarfile.REGTYPE else "usr/local/bin/runtime-link",
                    body=body,
                    member_type=member_type,
                    linkname=target,
                )
                result = run_checks("filesystem", archive_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("bearer credential", result.stderr)
                material = body.decode(errors="ignore") if body else target
                credential = material.rsplit("Bearer ", 1)[1].splitlines()[0]
                self.assertNotIn(credential, result.stdout + result.stderr)
                if target:
                    self.assertNotIn(target, result.stdout + result.stderr)

    def test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_every_saved_layer(self):
        malicious_members = (
            ("tmp/deleted-cache.bin", b"Bearer 'safe-review-value'", tarfile.REGTYPE, ""),
            ("usr/local/bin/runtime-symlink", b"", tarfile.SYMTYPE, "../Bearer token"),
            ("usr/local/bin/runtime-hardlink", b"", tarfile.LNKTYPE, "/prefix\nBearer TOKEN,"),
        )
        benign_layer = [("opt/application/public.txt", b"public fixture\n", tarfile.REGTYPE, "")]
        for layer_index, malicious in enumerate(malicious_members):
            with self.subTest(layer=layer_index, member_type=malicious[2]), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                layers = [benign_layer, benign_layer, benign_layer]
                layers[layer_index] = [malicious]
                save_path = self.write_image_save_layers(root, layers)
                result = run_checks("layers", save_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("bearer credential", result.stderr)
                material = malicious[1].decode(errors="ignore") if malicious[1] else malicious[3]
                credential = material.rsplit("Bearer ", 1)[1].splitlines()[0]
                self.assertNotIn(credential, result.stdout + result.stderr)
                if malicious[3]:
                    self.assertNotIn(malicious[3], result.stdout + result.stderr)

    def test_standalone_bearer_credentials_are_rejected_from_exported_filesystem(self):
        fixtures = (
            (tarfile.REGTYPE, b"prefix\x00Bearer safe-review-standalone-filesystem-regular\n", ""),
            (tarfile.SYMTYPE, b"", "../bearer safe-review-standalone-filesystem-symlink"),
            (tarfile.LNKTYPE, b"", "/prefix\nBEARER safe-review-standalone-filesystem-hardlink"),
        )
        for member_type, body, target in fixtures:
            with self.subTest(member_type=member_type), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive_path = self.write_filesystem(
                    root,
                    "opt/application/cache.bin" if member_type == tarfile.REGTYPE else "usr/local/bin/runtime-link",
                    body=body,
                    member_type=member_type,
                    linkname=target,
                )
                result = run_checks("filesystem", archive_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("bearer credential", result.stderr)
                material = body.decode(errors="ignore") if body else target
                self.assertNotIn(material, result.stdout + result.stderr)
                self.assertNotIn("safe-review-standalone", result.stdout + result.stderr)

    def test_standalone_bearer_credentials_are_rejected_from_every_saved_layer(self):
        malicious_members = (
            ("tmp/deleted-cache.bin", b"Bearer safe-review-standalone-layer-regular", tarfile.REGTYPE, ""),
            ("usr/local/bin/runtime-symlink", b"", tarfile.SYMTYPE, "../bearer safe-review-standalone-layer-symlink"),
            ("usr/local/bin/runtime-hardlink", b"", tarfile.LNKTYPE, "/prefix\nBEARER safe-review-standalone-layer-hardlink"),
        )
        benign_layer = [("opt/application/public.txt", b"public fixture\n", tarfile.REGTYPE, "")]
        for layer_index, malicious in enumerate(malicious_members):
            with self.subTest(layer=layer_index, member_type=malicious[2]), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                layers = [benign_layer, benign_layer, benign_layer]
                layers[layer_index] = [malicious]
                save_path = self.write_image_save_layers(root, layers)
                result = run_checks("layers", save_path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("bearer credential", result.stderr)
                self.assertNotIn("safe-review-standalone", result.stdout + result.stderr)
                if malicious[3]:
                    self.assertNotIn(malicious[3], result.stdout + result.stderr)

    def test_benign_bearer_prose_and_code_remain_accepted(self):
        body = (
            b"The bearer authentication scheme is supported.\n"
            b"A bearer token is carried in an authorization header.\n"
            b"The bearer of this certificate may present it.\n"
            b"print('bearer authentication handler')\n"
        )
        try:
            CHECKS_MODULE.check_file_contents(body, "opt/application/public.txt")
        except CHECKS_MODULE.CheckFailure as error:
            self.fail(str(error))
        for scanner in ("filesystem", "layers"):
            with self.subTest(scanner=scanner), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive_path = self.write_scanner_archive(scanner, root, "opt/application/public.txt", body)
                result = run_checks(scanner, archive_path)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_repeated_benign_bearer_records_do_not_rescan_prefixes(self):
        class PrefixCountingStr(str):
            def __new__(cls, value):
                instance = super().__new__(cls, value)
                instance.prefix_search_work = 0
                return instance

            def rfind(self, sub, start=0, end=None):
                if end is None:
                    end = len(self)
                self.prefix_search_work += end - start
                return super().rfind(sub, start, end)

        class PrefixCountingBytes(bytes):
            def __new__(cls, value):
                instance = super().__new__(cls, value)
                instance.prefix_search_work = 0
                return instance

            def rfind(self, sub, start=0, end=None):
                if end is None:
                    end = len(self)
                self.prefix_search_work += end - start
                return super().rfind(sub, start, end)

        benign = tuple(sorted(CHECKS_MODULE.BENIGN_BEARER_CONTEXT_LINES))
        records = [f" \t{benign[index % len(benign)]}\t " for index in range(256)]
        for text_type, separator in ((PrefixCountingStr, "\n\x00"), (PrefixCountingBytes, b"\n\x00")):
            encoded_records = records if text_type is PrefixCountingStr else [record.encode("ascii") for record in records]
            text = text_type(separator.join(encoded_records))
            with self.subTest(text_type=text_type.__name__):
                self.assertFalse(
                    CHECKS_MODULE.standalone_bearer_credential_found(text, CHECKS_MODULE.STANDALONE_BEARER if text_type is PrefixCountingStr else CHECKS_MODULE.STANDALONE_BEARER_BYTES)
                )
                self.assertLessEqual(
                    text.prefix_search_work,
                    len(text) * 6,
                    "standalone Bearer scan repeatedly rescanned already-checked prefixes",
                )

        for separator in ("\n", "\x00"):
            with self.subTest(separator=repr(separator)):
                exact_record = f" \t{benign[0]}\t "
                self.assertFalse(
                    CHECKS_MODULE.standalone_bearer_credential_found(exact_record, CHECKS_MODULE.STANDALONE_BEARER)
                )
                self.assertTrue(
                    CHECKS_MODULE.standalone_bearer_credential_found(
                        separator.join((exact_record, f"{benign[0]} appended-credential")),
                        CHECKS_MODULE.STANDALONE_BEARER,
                    )
                )

    def test_bearer_link_targets_are_rejected_without_printing_material(self):
        for scanner in ("filesystem", "layers"):
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(scanner=scanner, link_type=link_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    value = f"safe-review-bearer-{scanner}-{link_type.decode()}"
                    target = f"../Authorization: Bearer {value}"
                    archive_path = self.write_scanner_archive(
                        scanner,
                        root,
                        "usr/local/bin/runtime-link",
                        member_type=link_type,
                        linkname=target,
                    )
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("bearer credential", result.stderr)
                    self.assertIn("link target", result.stderr)
                    self.assertNotIn(target, result.stdout + result.stderr)
                    self.assertNotIn(value, result.stdout + result.stderr)

    def test_nul_delimited_link_assignments_are_rejected_without_printing_material(self):
        for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
            with self.subTest(link_type=link_type):
                value = f"safe-review-nul-link-{link_type.decode()}"
                member = tarfile.TarInfo("usr/local/bin/runtime-link")
                member.type = link_type
                member.linkname = f"../UPLOAD_TOKEN={value}\x00suffix"
                with self.assertRaises(CHECKS_MODULE.CheckFailure) as failure:
                    CHECKS_MODULE.check_link_target(member, member.name, "fixture link target")
                self.assertIn("secret-like assignment", str(failure.exception))
                self.assertNotIn(member.linkname, str(failure.exception))
                self.assertNotIn(value, str(failure.exception))

    def test_oversized_link_targets_are_rejected_without_printing_targets(self):
        target = "../" + "a" * CHECKS_MODULE.MAX_LINK_TARGET_SCAN_BYTES
        for scanner in ("filesystem", "layers"):
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(scanner=scanner, link_type=link_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    archive_path = self.write_scanner_archive(
                        scanner,
                        root,
                        "usr/local/bin/runtime-link",
                        member_type=link_type,
                        linkname=target,
                    )
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("bounded scan limit", result.stderr)
                    self.assertIn("link target", result.stderr)
                    self.assertNotIn(target, result.stdout + result.stderr)

    def test_regular_file_exact_assignment_allowlist_still_applies(self):
        body = b"secret=public-openssl-configuration-reference\n"
        for scanner in ("filesystem", "layers"):
            with self.subTest(scanner=scanner), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive_path = self.write_scanner_archive(
                    scanner,
                    root,
                    "etc/ssl/openssl.cnf",
                    body=body,
                )
                result = run_checks(scanner, archive_path)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_secret_bearing_link_targets_are_rejected_without_printing_values(self):
        scanners = ("filesystem", "layers")
        link_types = (tarfile.SYMTYPE, tarfile.LNKTYPE)
        for scanner in scanners:
            for link_type in link_types:
                with self.subTest(scanner=scanner, link_type=link_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    value = f"safe-review-{scanner}-{link_type.decode()}-link-secret"
                    target = f"../runtime/UPLOAD_TOKEN={value}"
                    if scanner == "filesystem":
                        archive_path = self.write_filesystem(
                            root, "usr/local/bin/runtime-link", member_type=link_type, linkname=target
                        )
                    else:
                        archive_path = self.write_image_save(
                            root, "usr/local/bin/runtime-link", member_type=link_type, linkname=target
                        )
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("link target", result.stderr)
                    self.assertNotIn(target, result.stdout + result.stderr)
                    self.assertNotIn(value, result.stdout + result.stderr)

    def test_secret_sentinel_link_targets_are_rejected_without_printing_values(self):
        target = "../runtime/safe-review-file-sentinel"
        for scanner in ("filesystem", "layers"):
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(scanner=scanner, link_type=link_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    if scanner == "filesystem":
                        archive_path = self.write_filesystem(
                            root, "usr/local/bin/runtime-link", member_type=link_type, linkname=target
                        )
                    else:
                        archive_path = self.write_image_save(
                            root, "usr/local/bin/runtime-link", member_type=link_type, linkname=target
                        )
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("link target", result.stderr)
                    self.assertNotIn(target, result.stdout + result.stderr)

    def test_prohibited_artifact_link_targets_are_rejected_without_printing_targets(self):
        target = "../run/secrets/upload-token"
        for scanner in ("filesystem", "layers"):
            for link_type in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                with self.subTest(scanner=scanner, link_type=link_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    if scanner == "filesystem":
                        archive_path = self.write_filesystem(
                            root, "usr/local/bin/runtime-link", member_type=link_type, linkname=target
                        )
                    else:
                        archive_path = self.write_image_save(
                            root, "usr/local/bin/runtime-link", member_type=link_type, linkname=target
                        )
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("prohibited deployment artifact", result.stderr)
                    self.assertIn("link target", result.stderr)
                    self.assertNotIn(target, result.stdout + result.stderr)

    def test_prohibited_paths_are_rejected_for_every_tar_member_type(self):
        member_types = (tarfile.REGTYPE, tarfile.DIRTYPE, tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE)
        for scanner in ("filesystem", "layers"):
            for member_type in member_types:
                with self.subTest(scanner=scanner, member_type=member_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    kwargs = {
                        "body": b"public fixture\n" if member_type == tarfile.REGTYPE else b"",
                        "member_type": member_type,
                        "linkname": "../usr/bin/python3" if member_type in (tarfile.SYMTYPE, tarfile.LNKTYPE) else "",
                    }
                    if scanner == "filesystem":
                        archive_path = self.write_filesystem(root, "run/config/upload-token", **kwargs)
                    else:
                        archive_path = self.write_image_save(root, "run/config/upload-token", **kwargs)
                    result = run_checks(scanner, archive_path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("prohibited deployment artifact", result.stderr)

    def test_benign_relative_runtime_links_are_accepted(self):
        fixtures = (
            (tarfile.SYMTYPE, "usr/bin/python3", "python3.13"),
            (tarfile.LNKTYPE, "usr/bin/python-copy", "usr/bin/python3.13"),
        )
        for scanner in ("filesystem", "layers"):
            for member_type, path, target in fixtures:
                with self.subTest(scanner=scanner, member_type=member_type), tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    if scanner == "filesystem":
                        archive_path = self.write_filesystem(
                            root, path, member_type=member_type, linkname=target
                        )
                    else:
                        archive_path = self.write_image_save(
                            root, path, member_type=member_type, linkname=target
                        )
                    result = run_checks(scanner, archive_path)
                    self.assertEqual(result.returncode, 0, result.stderr)

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
