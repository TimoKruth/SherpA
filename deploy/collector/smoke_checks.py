#!/usr/bin/env python3
"""Isolated, explicit collector image and Compose smoke gates."""

import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tarfile
import tempfile


class CheckFailure(Exception):
    pass


def require(condition, message):
    if not condition:
        raise CheckFailure(message)


def load_json(path):
    try:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, ValueError, TypeError) as error:
        raise CheckFailure(f"cannot load required JSON: {Path(path).name}") from error


def check_compose(path, root, revision):
    config = load_json(path)
    try:
        service = config["services"]["collector"]
    except (KeyError, TypeError) as error:
        raise CheckFailure("rendered Compose lacks collector service") from error

    expected = [
        (os.path.realpath(service.get("build", {}).get("context", "")) == os.path.realpath(os.path.join(root, "source")), "Compose build context is not exact"),
        (service.get("build", {}).get("dockerfile") == "deploy/collector/Dockerfile", "Compose Dockerfile is not exact"),
        (service.get("build", {}).get("args") == {"SOURCE_REVISION": revision}, "Compose build revision is not exact"),
        (service.get("restart") == "unless-stopped", "Compose restart policy is not exact"),
        (service.get("user") == "10001:10001", "Compose runtime identity is not exact"),
        (service.get("read_only") is True, "Compose root filesystem is not read-only"),
        (service.get("cap_drop") == ["ALL"], "Compose does not drop all capabilities"),
        (service.get("security_opt") == ["no-new-privileges:true"], "Compose no-new-privileges setting is not exact"),
        (service.get("pids_limit") == 128, "Compose PID limit is not exact"),
        (float(service.get("cpus", 0)) == 1.0, "Compose CPU limit is not exact"),
        (int(service.get("mem_limit", 0)) == 1024 * 1024 * 1024, "Compose memory limit is not exact"),
        (service.get("expose") in (["8080"], ["8080/tcp"]), "Compose internal port is not exact"),
        ("ports" not in service, "Compose publishes a host port"),
        ("cap_add" not in service, "Compose adds capabilities"),
        ("privileged" not in service, "Compose enables privileged mode"),
        ("devices" not in service, "Compose adds a device"),
        (service.get("networks") in (None, {"default": None}, ["default"]), "Compose adds a custom network"),
        (service.get("tmpfs") == ["/tmp:rw,nosuid,nodev,noexec,size=64m"], "Compose tmpfs is not exact"),
    ]
    for condition, message in expected:
        require(condition, message)

    healthcheck = service.get("healthcheck") or {}
    require(healthcheck.get("test") == ["CMD", "/usr/local/bin/collector", "healthcheck", "http://127.0.0.1:8080/healthz"], "Compose healthcheck command is not exact")
    require(healthcheck.get("interval") == "30s", "Compose healthcheck interval is not exact")
    require(healthcheck.get("timeout") == "5s", "Compose healthcheck timeout is not exact")
    require(healthcheck.get("retries") == 3, "Compose healthcheck retries are not exact")
    require(healthcheck.get("start_period") == "20s", "Compose healthcheck start period is not exact")
    require(service.get("logging") == {"driver": "json-file", "options": {"max-file": "5", "max-size": "10m"}}, "Compose logging is not exact")

    expected_labels = {
        "traefik.enable": "true",
        "traefik.http.routers.sherpa-collector.rule": "Host(`sherpa-collector.kruth-support.de`)",
        "traefik.http.routers.sherpa-collector.entrypoints": "websecure",
        "traefik.http.routers.sherpa-collector.tls": "true",
        "traefik.http.routers.sherpa-collector.tls.certresolver": "letsencrypt",
        "traefik.http.services.sherpa-collector.loadbalancer.server.port": "8080",
    }
    require(service.get("labels") == expected_labels, "Compose Traefik labels are not exact")

    try:
        mounts = {
            (os.path.realpath(item["source"]), item["target"], item.get("read_only", False))
            for item in service["volumes"]
        }
    except (KeyError, TypeError) as error:
        raise CheckFailure("Compose mounts are malformed") from error
    expected_mounts = {
        (os.path.realpath(os.path.join(root, "data")), "/data", False),
        (os.path.realpath(os.path.join(root, "config/age-recipient")), "/run/config/age-recipient", True),
        (os.path.realpath(os.path.join(root, "config/known_hosts")), "/run/config/known_hosts", True),
        (os.path.realpath(os.path.join(root, "secrets/upload-token")), "/run/secrets/upload-token", True),
        (os.path.realpath(os.path.join(root, "secrets/storage-ssh-key")), "/run/secrets/storage-ssh-key", True),
        (os.path.realpath(os.path.join(root, "secrets/borg-repository")), "/run/secrets/borg-repository", True),
    }
    require(mounts == expected_mounts, "Compose mounts are not exact")


def create_archive(repo, destination):
    repo = os.path.realpath(repo)
    destination = os.path.realpath(destination)
    revision = subprocess.check_output(["git", "-C", repo, "rev-parse", "HEAD"], text=True).strip()
    require(re.fullmatch(r"[0-9a-f]{40}", revision) is not None, "source revision is not a full commit")
    if os.path.exists(destination):
        require(os.path.isdir(destination) and not os.listdir(destination), "archive destination is not empty")
    else:
        os.makedirs(destination, mode=0o700)
    with tempfile.NamedTemporaryFile(prefix="sherpa-source-", suffix=".tar", delete=False) as handle:
        archive_path = handle.name
    try:
        subprocess.run(["git", "-C", repo, "archive", "--format=tar", f"--output={archive_path}", revision], check=True)
        subprocess.run(["tar", "-xf", archive_path, "-C", destination], check=True)
    except subprocess.CalledProcessError as error:
        raise CheckFailure("cannot construct exact committed source archive") from error
    finally:
        try:
            os.remove(archive_path)
        except FileNotFoundError:
            pass
    require(not os.path.lexists(os.path.join(destination, ".git")), "committed source archive contains Git metadata")
    return revision


SENSITIVE_KEY = re.compile(
    r"(?:^|[_.-])(?:token|secret|password|passwd|passphrase|credential|private(?:[_.-]?key)?|"
    r"ssh(?:[_.-]?key)?|storage[_.-]?key|api[_.-]?key|access[_.-]?key|age[_.-]?(?:identity|recipient)|"
    r"known[_.-]?hosts|repository|repo(?:sitory)?[_.-]?(?:url|path)?|borg[_.-]?repo|"
    r"database(?:[_.-]?url)?|db[_.-]?url|oauth|client[_.-]?(?:id|secret)|auth)(?:$|[_.-])",
    re.IGNORECASE,
)
GENERIC_KEY = re.compile(r"(?:^|[_.-])key(?:$|[_.-])", re.IGNORECASE)
ASSIGNMENT = re.compile(r"(?<![A-Za-z0-9_.-])([A-Za-z_][A-Za-z0-9_.-]{1,127})=([^\s\x00]{1,4096})")
ALLOWED_ENVIRONMENT = {
    "PATH": "/opt/borg/bin:/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "GPG_KEY": "7169605F62C751356D054A26A821E680E5FA6305",
    "PYTHON_VERSION": "3.13.14",
    "PYTHON_SHA256": "639e43243c620a308f968213df9e00f2f8f62332f7adbaa7a7eeb9783057c690",
    "BORG_CACHE_DIR": "/data/borg/cache",
    "BORG_CONFIG_DIR": "/data/borg/config",
    "BORG_SECURITY_DIR": "/data/borg/security",
}
ALLOWED_SENSITIVE_ASSIGNMENTS = {"GPG_KEY"}
SAFE_SENTINELS = (
    "safe-smoke-upload-token",
    "safe-smoke-storage-key",
    "safe-bind-red-token",
    "safe-bind-red-key",
    "safe-review-metadata-secret",
    "safe-review-file-sentinel",
    "example.invalid ssh-ed25519",
    "/data/repository",
)


def sensitive_assignment_key(key):
    return SENSITIVE_KEY.search(key) is not None or GENERIC_KEY.search(key) is not None


def check_assignments(text, context, allowed=()):
    allowed = set(allowed)
    for match in ASSIGNMENT.finditer(text):
        key = match.group(1)
        if sensitive_assignment_key(key) and key.upper() not in allowed:
            raise CheckFailure(f"secret-like assignment key found in {context}")


def check_metadata(inspect_path, history_path, source_url, revision):
    image_data = load_json(inspect_path)
    require(isinstance(image_data, list) and len(image_data) == 1, "image inspection shape is not exact")
    try:
        config = image_data[0]["Config"]
    except (KeyError, TypeError) as error:
        raise CheckFailure("image inspection lacks config") from error
    try:
        history = Path(history_path).read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise CheckFailure("cannot read image history") from error
    environment = config.get("Env") or []
    labels = config.get("Labels") or {}
    require(isinstance(environment, list), "image environment is malformed")
    require(isinstance(labels, dict), "image labels are malformed")

    parsed_environment = {}
    for item in environment:
        require(isinstance(item, str) and "=" in item, "image environment entry is malformed")
        key, value = item.split("=", 1)
        require(key not in parsed_environment, "image environment contains duplicate key")
        parsed_environment[key] = value
        if sensitive_assignment_key(key):
            require(key in ALLOWED_SENSITIVE_ASSIGNMENTS, "secret-like image environment key is prohibited")
    require(parsed_environment == ALLOWED_ENVIRONMENT, "image environment is not the approved public set")

    expected_labels = {
        "org.opencontainers.image.source": source_url,
        "org.opencontainers.image.revision": revision,
    }
    require(labels == expected_labels, "image labels are not the approved public set")
    check_assignments(history, "image history", ALLOWED_SENSITIVE_ASSIGNMENTS)
    for key, value in labels.items():
        check_assignments(key, "image label key", ALLOWED_SENSITIVE_ASSIGNMENTS)
        check_assignments(value, "image label value", ALLOWED_SENSITIVE_ASSIGNMENTS)
    for text in [history, *environment, *labels.keys(), *labels.values()]:
        for sentinel in SAFE_SENTINELS:
            require(sentinel not in text, "safe secret sentinel found in image metadata")


PRIVATE_HEADERS = (
    b"-----BEGIN PRIVATE KEY-----",
    b"-----BEGIN ENCRYPTED PRIVATE KEY-----",
    b"-----BEGIN RSA PRIVATE KEY-----",
    b"-----BEGIN DSA PRIVATE KEY-----",
    b"-----BEGIN EC PRIVATE KEY-----",
    b"-----BEGIN OPENSSH PRIVATE KEY-----",
)
AGE_PRIVATE_IDENTITY = re.compile(rb"(?:^|[\r\n])AGE-SECRET-KEY-1[0-9A-Z]+(?:$|[\r\n])")
PROHIBITED_PATH = re.compile(
    r"(^|/)(?:\.env(?:\.[^/]*)?|runtime\.env|compose(?:\.[^/]*)?\.override\.ya?ml|"
    r"age-identity(?:\.txt)?|storage-ssh-key|upload-token|borg-repository|known_hosts|"
    r"id_(?:rsa|dsa|ecdsa|ed25519))(?:$|/)",
    re.IGNORECASE,
)
COMPILER_PATH = re.compile(
    r"(^|/)(?:([^/]+-)?(?:gcc|g\+\+|cc|c\+\+)(?:-[0-9.]+)?|clang(?:-[0-9.]+)?|"
    r"go|make|cmake|pkg-config|curl|fusermount3?)$"
)
ASSIGNMENT_BYTES = re.compile(rb"(?<![A-Za-z0-9_.-])([A-Za-z_][A-Za-z0-9_.-]{1,127})=([^\s\x00]{1,4096})")
MAX_FILE_SCAN_BYTES = 64 * 1024 * 1024
MAX_TOTAL_SCAN_BYTES = 1024 * 1024 * 1024


def skip_runtime_assignment_scan(path):
    lowered = path.lower()
    return (
        lowered == "etc/security/namespace.init"
        or lowered.startswith(("bin/", "sbin/", "lib/", "usr/bin/", "usr/sbin/", "usr/lib/", "usr/share/"))
        or re.search(r"^usr/local/(?:lib|include)/python[0-9.]+/", lowered) is not None
        or re.search(r"^opt/borg/lib/python[0-9.]+/site-packages/", lowered) is not None
    )


def check_file_contents(data, path):
    for sentinel in SAFE_SENTINELS:
        require(sentinel.encode() not in data, f"safe secret sentinel found in exported regular file: {path}")
    if b"\x00" in data:
        return
    for header in PRIVATE_HEADERS:
        require(re.search(re.escape(header) + rb"\r?\n", data) is None, f"private-key material found in exported regular file: {path}")
    require(AGE_PRIVATE_IDENTITY.search(data) is None, f"age private identity found in exported regular file: {path}")
    if skip_runtime_assignment_scan(path):
        return
    for match in ASSIGNMENT_BYTES.finditer(data):
        key = match.group(1).decode("ascii", errors="ignore")
        if sensitive_assignment_key(key) and key.upper() not in ALLOWED_SENSITIVE_ASSIGNMENTS:
            raise CheckFailure(f"secret-like assignment found in exported regular file: {path}")


def check_filesystem(archive_path):
    scanned = 0
    try:
        archive = tarfile.open(archive_path)
    except (OSError, tarfile.TarError) as error:
        raise CheckFailure("cannot open exported image filesystem") from error
    with archive:
        for member in archive:
            path = member.name.lstrip("./")
            lowered = path.lower()
            require(not (member.isfile() and member.mode & 0o6000), f"setuid/setgid file found: {path}")
            require("security.capability" not in " ".join(member.pax_headers).lower(), f"file capability found: {path}")
            require(not path.startswith("usr/local/go/"), f"Go toolchain found: {path}")
            require(not path.startswith("src/"), f"source directory found: {path}")
            require(not path.endswith(".go"), f"Go source found: {path}")
            require("/.git/" not in f"/{path}/" and not path.endswith("/.git"), f"Git metadata found: {path}")
            require(re.search(r"(^|/)(testdata|fixtures)(/|$)", lowered) is None, f"SherpA test fixture found: {path}")
            require("recoveryarchive/testfixture" not in lowered, f"recovery fixture found: {path}")
            require(PROHIBITED_PATH.search(path) is None, f"prohibited deployment artifact found: {path}")
            if not member.isfile():
                continue
            require(COMPILER_PATH.search(path) is None, f"compiler or build tool found: {path}")
            require(not path.startswith("var/lib/apt/lists/"), f"apt list cache found: {path}")
            require(not path.startswith("var/cache/apt/"), f"apt cache found: {path}")
            require(re.search(r"(^|/)\.cache/pip/", lowered) is None, f"pip cache found: {path}")
            require(member.size <= MAX_FILE_SCAN_BYTES, f"exported regular file exceeds bounded scan limit: {path}")
            scanned += member.size
            require(scanned <= MAX_TOTAL_SCAN_BYTES, "exported filesystem exceeds bounded content scan limit")
            extracted = archive.extractfile(member)
            require(extracted is not None, f"cannot read exported regular file: {path}")
            data = extracted.read(MAX_FILE_SCAN_BYTES + 1)
            require(len(data) == member.size, f"cannot completely scan exported regular file: {path}")
            check_file_contents(data, path)


DATA_DIRECTORIES = (
    "spool",
    "state",
    "borg",
    "borg/cache",
    "borg/config",
    "borg/security",
)


def check_layout(root, expected_uid, expected_gid):
    root = os.path.realpath(root)
    for relative in DATA_DIRECTORIES:
        path = os.path.join(root, relative)
        try:
            info = os.stat(path, follow_symlinks=False)
        except OSError as error:
            raise CheckFailure(f"required bind directory is unavailable: {relative}") from error
        require(stat.S_ISDIR(info.st_mode), f"required bind path is not a directory: {relative}")
        require(stat.S_IMODE(info.st_mode) == 0o700, f"bind directory mode is not 0700: {relative}")
        require(info.st_uid == expected_uid and info.st_gid == expected_gid, f"bind directory ownership is not exact: {relative}")


def check_bind(inspect_path, expected_source):
    data = load_json(inspect_path)
    require(isinstance(data, list) and len(data) == 1, "runtime inspection shape is not exact")
    mounts = data[0].get("Mounts") or []
    matching = [mount for mount in mounts if mount.get("Destination") == "/data"]
    require(len(matching) == 1, "runtime data mount count is not exact")
    mount = matching[0]
    require(mount.get("Type") == "bind", "runtime data mount is not a bind mount")
    require(os.path.realpath(mount.get("Source", "")) == os.path.realpath(expected_source), "runtime data bind source is not exact")
    require(mount.get("RW") is True, "runtime data bind is not writable")


def main(argv):
    require(len(argv) >= 2, "missing smoke check command")
    command = argv[1]
    if command == "compose":
        require(len(argv) == 5, "compose check arguments are invalid")
        check_compose(argv[2], argv[3], argv[4])
    elif command == "archive":
        require(len(argv) == 4, "archive check arguments are invalid")
        print(create_archive(argv[2], argv[3]))
    elif command == "metadata":
        require(len(argv) == 6, "metadata check arguments are invalid")
        check_metadata(argv[2], argv[3], argv[4], argv[5])
    elif command == "filesystem":
        require(len(argv) == 3, "filesystem check arguments are invalid")
        check_filesystem(argv[2])
    elif command == "layout":
        require(len(argv) == 5, "layout check arguments are invalid")
        check_layout(argv[2], int(argv[3]), int(argv[4]))
    elif command == "bind":
        require(len(argv) == 4, "bind check arguments are invalid")
        check_bind(argv[2], argv[3])
    else:
        raise CheckFailure("unknown smoke check command")


if __name__ == "__main__":
    try:
        main(sys.argv)
    except (CheckFailure, OSError, subprocess.SubprocessError, ValueError) as error:
        print(f"collector smoke check failed: {error}", file=sys.stderr)
        sys.exit(1)
