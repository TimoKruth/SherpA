#!/usr/bin/env python3
"""Isolated, explicit collector image and Compose smoke gates."""

import hashlib
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


ALLOWED_IMAGE_PLATFORMS = frozenset({"linux/arm64", "linux/amd64"})


def image_platform(inspect_path):
    image_data = load_json(inspect_path)
    require(isinstance(image_data, list) and len(image_data) == 1, "image platform inspection shape is not exact")
    try:
        platform = f"{image_data[0]['Os']}/{image_data[0]['Architecture']}"
    except (KeyError, TypeError) as error:
        raise CheckFailure("image platform metadata is missing or malformed") from error
    require(platform in ALLOWED_IMAGE_PLATFORMS, "image platform is not approved")
    return platform


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
    r"(?:^|[_.-])(?:token|bearer|secret|password|passwd|passphrase|credential|private(?:[_.-]?key)?|"
    r"ssh(?:[_.-]?key)?|storage[_.-]?key|api[_.-]?key|access[_.-]?key|age[_.-]?(?:identity|recipient)|"
    r"known[_.-]?hosts|repository|repo(?:sitory)?[_.-]?(?:url|path)?|borg[_.-]?repo|"
    r"database(?:[_.-]?url)?|db[_.-]?url|oauth|client[_.-]?(?:id|secret)|auth|authorization)(?:$|[_.-])",
    re.IGNORECASE,
)
GENERIC_KEY = re.compile(r"(?:^|[_.-])key(?:$|[_.-])", re.IGNORECASE)
ASSIGNMENT = re.compile(
    r"(?=(?<![A-Za-z0-9_.-])[\"']?([A-Za-z_][A-Za-z0-9_.-]*)[\"']?\s*(?:=|:)\s*([^\s\x00]{1,4096}))"
)
AUTHORIZATION_CREDENTIAL = re.compile(
    r"(?<![A-Za-z0-9_.-])(?:proxy[_.-]?)?authorization\s*(?:=|:)\s*(?:bearer|token)\s+[^\s\"']+",
    re.IGNORECASE,
)
STANDALONE_BEARER = re.compile(
    r"(?<![A-Za-z0-9_.-])bearer\s+[^\s\x00]+",
    re.IGNORECASE,
)
BENIGN_BEARER_CONTEXT_LINES = frozenset({
    "The bearer authentication scheme is supported.",
    "A bearer token is carried in an authorization header.",
    "The bearer of this certificate may present it.",
    "print('bearer authentication handler')",
})
BENIGN_BEARER_CONTEXT_LINES_BYTES = frozenset(
    line.encode("ascii") for line in BENIGN_BEARER_CONTEXT_LINES
)
BEARER_RECORD_SEPARATOR = re.compile(r"[\x00\r\n]")
BEARER_RECORD_SEPARATOR_BYTES = re.compile(rb"[\x00\r\n]")
ALLOWED_ENVIRONMENT = {
    "PATH": "/opt/borg/bin:/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "GPG_KEY": "7169605F62C751356D054A26A821E680E5FA6305",
    "PYTHON_VERSION": "3.13.14",
    "PYTHON_SHA256": "639e43243c620a308f968213df9e00f2f8f62332f7adbaa7a7eeb9783057c690",
    "BORG_CACHE_DIR": "/data/borg/cache",
    "BORG_CONFIG_DIR": "/data/borg/config",
    "BORG_SECURITY_DIR": "/data/borg/security",
}
ALLOWED_GPG_ASSIGNMENT = ("GPG_KEY", ALLOWED_ENVIRONMENT["GPG_KEY"])
ALLOWED_GPG_HISTORY_LINE = f"ENV GPG_KEY={ALLOWED_ENVIRONMENT['GPG_KEY']}"
ALLOWED_SENSITIVE_ENVIRONMENT = {ALLOWED_GPG_ASSIGNMENT}
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


def bearer_record_bounds(text):
    separator_pattern = BEARER_RECORD_SEPARATOR_BYTES if isinstance(text, bytes) else BEARER_RECORD_SEPARATOR
    start = 0
    for separator in separator_pattern.finditer(text):
        yield start, separator.start()
        start = separator.end()
    yield start, len(text)


def standalone_bearer_credential_found(text, pattern):
    benign_lines = BENIGN_BEARER_CONTEXT_LINES_BYTES if isinstance(text, bytes) else BENIGN_BEARER_CONTEXT_LINES
    records = iter(bearer_record_bounds(text))
    record_start, record_end = next(records)
    for match in pattern.finditer(text):
        while record_end < match.start():
            record_start, record_end = next(records)
        if text[record_start:record_end].strip() not in benign_lines:
            return True
    return False


def check_assignments(text, context):
    if AUTHORIZATION_CREDENTIAL.search(text) is not None or standalone_bearer_credential_found(text, STANDALONE_BEARER):
        raise CheckFailure(f"bearer credential found in {context}")
    for match in ASSIGNMENT.finditer(text):
        key = match.group(1)
        if sensitive_assignment_key(key):
            raise CheckFailure(f"secret-like assignment key found in {context}")


def check_history_assignments(history):
    unapproved = "\n".join(line for line in history.splitlines() if line != ALLOWED_GPG_HISTORY_LINE)
    check_assignments(unapproved, "image history")


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
            require((key.upper(), value) in ALLOWED_SENSITIVE_ENVIRONMENT, "secret-like image environment assignment is prohibited")
    require(parsed_environment == ALLOWED_ENVIRONMENT, "image environment is not the approved public set")

    expected_labels = {
        "org.opencontainers.image.source": source_url,
        "org.opencontainers.image.revision": revision,
    }
    require(labels == expected_labels, "image labels are not the approved public set")
    check_history_assignments(history)
    for key, value in labels.items():
        check_assignments(key, "image label key")
        check_assignments(value, "image label value")
    for text in [history, *environment, *labels.keys(), *labels.values()]:
        for sentinel in SAFE_SENTINELS:
            require(sentinel not in text, "safe secret sentinel found in image metadata")


OPENSSH_PRIVATE_HEADER = b"-----BEGIN OPENSSH PRIVATE KEY-----"
PRIVATE_HEADERS = (
    b"-----BEGIN PRIVATE KEY-----",
    b"-----BEGIN ENCRYPTED PRIVATE KEY-----",
    b"-----BEGIN RSA PRIVATE KEY-----",
    b"-----BEGIN DSA PRIVATE KEY-----",
    b"-----BEGIN EC PRIVATE KEY-----",
    OPENSSH_PRIVATE_HEADER,
)
# Pinned libgnutls contains reviewed NUL-delimited test vectors. The exception is
# exact to the normalized path and complete NUL-delimited record bytes.
ALLOWED_NUL_PRIVATE_RECORD_SHA256 = {
    "usr/lib/<arch>/libgnutls.so.30.34.3": frozenset({
        "8ca29b4039d33d0e308d90902ab8157aa4a0bb42d8e6a3557bf5c78d3b8d7010",
        "c22a09c6bbe3d182b0cb500d75bd9e97ecf07e8914b29f09a959e789419c6ef2",
        "3542b089a11061bd543f7db92a4b28c67e5cacc4293e61812bdfea8ddfbd19a4",
        "e7512a909f8818815fe5d547de5a90a124055623188ef159cc19c4fd9bf11ecd",
        "6cf9a0b2af6e63e436973615b1e39dddac553cd1e11b74c211514fa8bd881600",
        "fccc1f9804eb2ef9bbb97f3d74a3780389da2bf378f88ae270fe317c0f6140c9",
    }),
}
# msgpack 1.2.1 wheels for the two deployment architectures contain these
# reviewed complete NUL-delimited records. The exception remains exact to the
# normalized compiled-extension path, assignment key, and record digest.
ALLOWED_NUL_ASSIGNMENT_RECORD_SHA256 = {
    "opt/borg/lib/python3.13/site-packages/msgpack/_cmsgpack.cpython-313-<arch>-linux-gnu.so": {
        "strict_map_key": frozenset({
            "d63020dcc481de704039ce44faf1ca726f0d3e70765b2900c0886eb5d6f21525",
            "0cf91e3e6f10e36b6bb7698c7a1561a80ace0d9a28b672d3e8560d79487d5b03",
            "c4ac930f2245301678e8afab2a124d54ecdc1c4ee7393ca3cd71a677781ca24b",
        }),
    },
}
NUL_ASSIGNMENT_PATH_NORMALIZATION = {
    "opt/borg/lib/python3.13/site-packages/msgpack/_cmsgpack.cpython-313-aarch64-linux-gnu.so":
        "opt/borg/lib/python3.13/site-packages/msgpack/_cmsgpack.cpython-313-<arch>-linux-gnu.so",
    "opt/borg/lib/python3.13/site-packages/msgpack/_cmsgpack.cpython-313-x86_64-linux-gnu.so":
        "opt/borg/lib/python3.13/site-packages/msgpack/_cmsgpack.cpython-313-<arch>-linux-gnu.so",
}
# These exact whole-file artifacts and keys were reviewed from the pinned arm64
# and amd64 candidate images. They must not be regenerated or self-approved from
# the image under test; apt, pip, compiler, or source changes fail closed by hash.
_REVIEWED_PLATFORM_BINARY_ASSIGNMENTS = r"""
linux/arm64|usr/bin/findmnt|ac1f580590b440a028e684b633589fd87e782f0c1a4b1937449edb33a41c7668|key
linux/arm64|usr/bin/getent|fa93701168c7dbf756abb4144cd5bda19cb7a5efb6f3b961408a8d2f666004db|database
linux/arm64|usr/bin/gpgv|26999f2766739b3df31c6c355856440bbd7377a5a19ebcf3f6cfe4cfb3c42f69|cache_public_key,encode_session_key,key,ret_found_key,secret
linux/arm64|usr/bin/localedef|428a1a57253e62aa2939bd47ef1a021d6e6406ee0ff6b643dda04842f137b1f4|last_token
linux/arm64|usr/bin/lsblk|8a797bb55c2d950f07eaf5955378fff8c219abf898eac53a88e816e84dbf55c2|key
linux/arm64|usr/bin/lsfd|5c04a9b1ed3d62ef76b9a869d68395633c2839ca7ce949559b485f97ef01320b|token
linux/arm64|usr/bin/lsirq|c348ce945c0844b38e3a94c2fb3fd95c6ad03760e9630a3f81f0a303281f69dc|key
linux/arm64|usr/bin/lsmem|335e347d7eb9cc8ed8651a405f024dec0bf72a4589deed254fb85f2f0baf1653|key
linux/arm64|usr/bin/openssl|bc840e25ecb71ae7e84aed1e61980481b246fddb718159484f5a68ce1e199d09|key
linux/arm64|usr/bin/partx|b818aec5520fb5762e89e5942d9c55a1ad9dfe2c4eef37f661e9feb007dcf435|key
linux/arm64|usr/bin/passwd|bc0b502346f44eff9bf08cfd121b280684652a7e8eb4e78cba6404d32892ebf0|passwd
linux/arm64|usr/bin/perl|a87e4138d1e33d240bd31be3dd5ec59017f729b7aa9bd6b3dc012dfcab85d69b|MG_PRIVATE,PRIVATE
linux/arm64|usr/bin/ssh|78e7a4963fe15f37f40b5e700bbfcda1831e1978ac32bf3a2ee4d200cce255fe|input_userauth_passwd_changereq,key,ssh,ssh_selinux_getctxbyname,token
linux/arm64|usr/bin/ssh-add|265f00d9c611a124200efe86ea8d37ec5559dd05692829ca676dedeb386bb7a0|key,ssh_selinux_getctxbyname,sshkey_write
linux/arm64|usr/bin/ssh-agent|22d8527e1ecb96dd7d2db4cb4058bfe85bda2f7b3f5869828afdfa91271db092|AUTH_SOCKET,key
linux/arm64|usr/bin/ssh-keygen|6fc50fe603c9e2ba36fbbecb81601d938160d16b96e146901f75b1b18578b0c7|key,ssh,ssh_selinux_getctxbyname
linux/arm64|usr/bin/ssh-keyscan|94d75d14207aefee285e300f59553d7a75a4bd7bf8f10efbd5e867f77c3c0590|ssh_selinux_getctxbyname
linux/arm64|usr/lib/aarch64-linux-gnu/libc.so.6|e4ac8ae1d81e4865e3aadedb962879cf9415903b3f2ba81ec75e9962b86ab8b0|auth_unix.c
linux/arm64|usr/lib/aarch64-linux-gnu/libcrypto.so.3|6ca49d148cc9fff2ee82e46019f508d736cef6b3f15f2f5cbbc86457df9b05ce|Private-Key,Proxy-Authorization,Public-Key,recommended-private-length
linux/arm64|usr/lib/aarch64-linux-gnu/libdb-5.3.so|d2350d17e88d6693eff10fd95ad01581728609ac3017aead4d8c3952bf9f3232|database,key
linux/arm64|usr/lib/aarch64-linux-gnu/libext2fs.so.2.4|9102d31c6278d7804dcf4d55e528711e4205ed6887b323251fb69e3067647a6b|key.dptr,key_len
linux/arm64|usr/lib/aarch64-linux-gnu/libfido2.so.1.12.0|5923ae7a5dab03af586433f325eac850f448a9a786ce57f9c2d50c5c283ca255|key,key_len
linux/arm64|usr/lib/aarch64-linux-gnu/libgcrypt.so.20.4.1|01c8f6929c4c5853f8204a727e2be5fc3f7cef3f850e2818558800aa5f78c80e|check_secret_key
linux/arm64|usr/lib/aarch64-linux-gnu/libgnutls.so.30.34.3|a28310df0a36465608473face05bf89b87759804bd0df83b161d564b9804b3fc|get_challenge_password,get_key_usage,get_private_key_usage_period,get_subject_key_id,gnutls_x509_ext_import_authority_key_id,gnutls_x509_ext_import_key_purposes,gnutls_x509_key_purpose_get,gnutls_x509_key_purpose_init,key,password
linux/arm64|usr/lib/aarch64-linux-gnu/libkrb5.so.3.3|9521937afd55582d7714f7e20b5bf1ba53016221cb069edec49a3c2d58cdbe60|key
linux/arm64|usr/lib/aarch64-linux-gnu/libnsl.so.1|358d5eaf9adc3443cc28a0f5ee2e17a70c8bf99e471267c765c4c0fc1e523536|auth_name,auth_type
linux/arm64|usr/lib/aarch64-linux-gnu/libsqlite3.so.0.8.6|1da497e08d6343387879262c2be006e4801cb32d94f2868966747c1c5a6d1ede|database,key,token
linux/arm64|usr/lib/aarch64-linux-gnu/libssl.so.3|9e171264a6651d714938e83d81277d16524c150a36eaa59ce56e79b9cdbea7ce|secret
linux/arm64|usr/lib/aarch64-linux-gnu/libsystemd.so.0.35.0|e38e0c11131d330c9a49a4a7f9437f8cf9d6a198355a47ecff02bc4aabe52171|key,p.b.key
linux/arm64|usr/lib/aarch64-linux-gnu/libudev.so.1.7.5|215857660d2d9ff00d8d653188fc9bd77b25cb2492d275be4b25fe500981da95|key,p.b.key
linux/arm64|usr/lib/aarch64-linux-gnu/security/pam_unix.so|bf595555d3f0c80572ff96f3c1c65b17cdd2c2da18b1fb76e85b63709504e21a|pam_unix_auth
linux/arm64|usr/lib/apt/methods/http|1b2ae08e1853d682e7f472c4d96773bb4b237dd4a64189c21c729e79a7c70cfc|Authorization,Proxy-Authorization
linux/arm64|usr/local/bin/collector|86942e23b62714a352c1151b4ae25356c366a3ee43ab5dc4912d96bfbd58d277|key,key_sharebufio.Scanner,readage-renameBORG_REPO,stringBORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK
linux/arm64|usr/local/lib/libpython3.13.so.1.0|6883e73be69e1d09eff85be6670ba90c55b4e738735c5be2b172e6fbec4083a1|key,pwd.struct_passwd
linux/arm64|usr/local/lib/python3.13/lib-dynload/_bisect.cpython-313-aarch64-linux-gnu.so|c1935e52815bbeb478d2f97037b9b4d615735688cb0f0d98d41489771831eb45|key
linux/arm64|usr/local/lib/python3.13/lib-dynload/_blake2.cpython-313-aarch64-linux-gnu.so|0d64ecbbb59f84cb89f03ecce00546dfe3af0b0acb1bb69a5b3c868ec9db2f5a|key
linux/arm64|usr/local/lib/python3.13/lib-dynload/_ssl.cpython-313-aarch64-linux-gnu.so|40958d71011666176cd49eec9541e8cf945a59fff803e32ed818201e6bdeccfc|password
linux/arm64|usr/local/lib/python3.13/lib-dynload/_testinternalcapi.cpython-313-aarch64-linux-gnu.so|e5eba77f5329236282b7b5702f34860c4912d48b32f8eca670891bceb517561e|key
linux/arm64|usr/local/lib/python3.13/lib-dynload/_testlimitedcapi.cpython-313-aarch64-linux-gnu.so|34972ab94fce264df466aec7186ad7d490e2361a64ec45c13c5b8a9bada6246d|key
linux/arm64|usr/local/lib/python3.13/lib-dynload/_zoneinfo.cpython-313-aarch64-linux-gnu.so|e47979dd85142bc935b7d22a407deb47158949cc7997f13997060dd53aeed413|key
linux/arm64|usr/sbin/groupadd|82c9c3b90b84eff2b8db5661b9e6b1f3477f6d072320dc19f7378c5657ce9285|KEY
linux/arm64|usr/sbin/update-passwd|058c54987dd2415faae2cc638bf9e57bd86d7011d55863727aaeaa71c4ba9ed8|passwd,passwd-entry
linux/arm64|usr/sbin/useradd|ffaf805bf21df9095643578dfaf06a7b0420502aad01eb5a2c61d14f8409952e|KEY
linux/amd64|usr/bin/findmnt|c20246863774b36e928b0447bd255fac51925d60c123ac187b7c1a5ccf8f3eb3|key
linux/amd64|usr/bin/getent|1a8ece7c471cf0782699fbcbeaa10a21b6b6c9b7f775724c843144117fa85f5f|database
linux/amd64|usr/bin/gpgv|e82416f3ae002b63c0731f311519c55fc9e7d3c8bdadbd64239833298f09a2cf|cache_public_key,encode_session_key,key,ret_found_key,secret
linux/amd64|usr/bin/localedef|23c436bbc2924bfc98e95f7f0084dbdd8020039dbb96fe67b6485f65150a1a84|last_token
linux/amd64|usr/bin/lsblk|37c411674a512a38473f625b93a43914a540834fa5671916b0baa0475d2df3d0|key
linux/amd64|usr/bin/lsfd|86d85b4cd89da4d1313cd3fb72e35e025614359fcee4336b8643d4743dd894fb|token
linux/amd64|usr/bin/lsirq|790e1459772d1747da63350d978c5475bdf5316cb18d22749a6b79d8a142687d|key
linux/amd64|usr/bin/lsmem|e4960401262f9ae0a596c54e5fb1a953fe2727ec368126c8d6f6beeb244919cc|key
linux/amd64|usr/bin/openssl|b2eca5aab93387bfd865ba65df16b904458229093a380bf03f391b1e10658304|key
linux/amd64|usr/bin/partx|c492c820371ab8bb9e1afc91e9df2866b78c0ee7ca86ec5801f212a632e144f2|key
linux/amd64|usr/bin/passwd|d30cd42625b51cb54c49edc5b0b3348d6a438539446af3752a6af10393c4f2d2|passwd
linux/amd64|usr/bin/perl|f01fa7776dc21c9e4b5f60b2d231ca4d96dab958b8d06aff611cb1c16f871574|MG_PRIVATE,PRIVATE
linux/amd64|usr/bin/ssh|04f2ff5f506a3f332e7adeb1478a4c551ae74acdd328e6fb5c2495664d4064e6|input_userauth_passwd_changereq,key,ssh,ssh_selinux_getctxbyname,token
linux/amd64|usr/bin/ssh-add|1e02d3fca3c8d72570c11ded9681dcacd4775a33560786a27c489b9e4388ec06|key,ssh_selinux_getctxbyname,sshkey_write
linux/amd64|usr/bin/ssh-agent|165e70f42cf6a147ae2101fae05a4503791d95ac9de7cca7974577316d963829|AUTH_SOCKET,key
linux/amd64|usr/bin/ssh-keygen|2843cd46c617cf771c32e6f8a2a11585d83510ec528e3aeff8f7a7f0445420ba|key,ssh,ssh_selinux_getctxbyname
linux/amd64|usr/bin/ssh-keyscan|9475d0851a26a4f494dc7d40240b68b04874543cbd30f54195ff9af055aaf848|ssh_selinux_getctxbyname
linux/amd64|usr/lib/apt/methods/http|84b045df697f0b111ed712f64f30009b5c02218e96d3a65f8e76c7bbb6481f96|Proxy-Authorization
linux/amd64|usr/lib/x86_64-linux-gnu/libc.so.6|6b4a45352fd0c540a9c7c718f35ce8c8e46a4e482f9d3885a910c32d1a0e1421|auth_unix.c
linux/amd64|usr/lib/x86_64-linux-gnu/libcrypto.so.3|72db1b3de8b7dfbaba4c056135f408da555f9d5e137c82129478e07e769f8070|Private-Key,Proxy-Authorization,Public-Key,recommended-private-length
linux/amd64|usr/lib/x86_64-linux-gnu/libdb-5.3.so|3601dc1fc553a861cee3f969d9a384e0b157dcddc75e59c4efcd08ee836c601f|database,key
linux/amd64|usr/lib/x86_64-linux-gnu/libext2fs.so.2.4|dc840deeb5e4348fc46b8390527359309b230b53329cae237d2cc95f77699380|key.dptr,key_len
linux/amd64|usr/lib/x86_64-linux-gnu/libfido2.so.1.12.0|07d78f307e9509e9c46ad8a35d80360db2c8d20d54f776f33617431ba87e5587|key,key_len
linux/amd64|usr/lib/x86_64-linux-gnu/libgcrypt.so.20.4.1|14d0ad938ee07d31ad774567059ac3bb1139e692c6ad21a1450785e880eeb1e8|check_secret_key
linux/amd64|usr/lib/x86_64-linux-gnu/libgnutls.so.30.34.3|779b25d20249988bea2c1aa6bbeb218f5ae7ea8a9d30ce4f54ea37372965cc4b|get_challenge_password,get_key_usage,get_private_key_usage_period,get_subject_key_id,gnutls_x509_ext_import_authority_key_id,gnutls_x509_ext_import_key_purposes,gnutls_x509_key_purpose_get,gnutls_x509_key_purpose_init,key,password
linux/amd64|usr/lib/x86_64-linux-gnu/libkrb5.so.3.3|47b51d738881cbab3825dfcf8eb69fc64922a60d67609bb55b3f46e69276d572|key
linux/amd64|usr/lib/x86_64-linux-gnu/libnsl.so.1|60d61ad427637c39f7ad032b08eb97039eef67728f05cad4bc9a458904dbe68c|auth_name,auth_type
linux/amd64|usr/lib/x86_64-linux-gnu/libsqlite3.so.0.8.6|2e6eef9a727f081f0d453b4e5e6cbd8b9ef8b6f86cbf7681cbad444d3b0b55c8|database,key,token
linux/amd64|usr/lib/x86_64-linux-gnu/libssl.so.3|9aec161fdbc82d3e4280f5084843118939f1f4acc53c98ec963de03cfe812fad|secret
linux/amd64|usr/lib/x86_64-linux-gnu/libsystemd.so.0.35.0|3880319ae776b622ad3c89201984065905d10271f78ef3a4db2ee0604a61256b|key,p.b.key
linux/amd64|usr/lib/x86_64-linux-gnu/libudev.so.1.7.5|99a5e38f8b45ec2729e5bc24d2d8e2f04d260a2455431a1f9d61f500b31060dd|key,p.b.key
linux/amd64|usr/lib/x86_64-linux-gnu/security/pam_unix.so|f4ef9b05d76c72ff807e82929e745ae34e769f0caab1e357163ebc63eb1621b1|pam_unix_auth
linux/amd64|usr/local/bin/collector|ca754aefde6d34b3e03835b4c283ad710e4ad9d064b1b937207973acc98c2014|key,key_sharebufio.Scanner,readage-renameBORG_REPO,stringBORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK
linux/amd64|usr/local/lib/libpython3.13.so.1.0|1c99db6c082a0ff2acb4af8b87dc6b43349056ee22dfc573bd7353ffd05fc8ef|key,pwd.struct_passwd
linux/amd64|usr/local/lib/python3.13/lib-dynload/_bisect.cpython-313-x86_64-linux-gnu.so|ab1941e87f927eed2683dd5a84166d403b5678ae0131ac79869cf62f8ca7bf53|key
linux/amd64|usr/local/lib/python3.13/lib-dynload/_blake2.cpython-313-x86_64-linux-gnu.so|c6e5cffe5c51f94349f372afba720ea96a9712ac2034130eae1a820517c5b5e3|key
linux/amd64|usr/local/lib/python3.13/lib-dynload/_ssl.cpython-313-x86_64-linux-gnu.so|d298b3f52be4313e30205871ae60fbaad8ebdd9d82b70e2d0a5a946879552ef5|password
linux/amd64|usr/local/lib/python3.13/lib-dynload/_testinternalcapi.cpython-313-x86_64-linux-gnu.so|f96713e47308c325fc70c5b972d1415c1e75a88f05cc534b80a9f8827e87967a|key
linux/amd64|usr/local/lib/python3.13/lib-dynload/_testlimitedcapi.cpython-313-x86_64-linux-gnu.so|ea1ad228552abbb4119fcf20bea8ae281df058b4e608d62eca4dbf6c53b1a876|key
linux/amd64|usr/local/lib/python3.13/lib-dynload/_zoneinfo.cpython-313-x86_64-linux-gnu.so|386f7826aeb220d1505a7c7dea820fbc16752e88651d50465a083ae5d8aa533b|key
linux/amd64|usr/sbin/groupadd|8261929420f9eb93885df8a8dee887005b72ee0d373141e224722cd5618617c1|KEY
linux/amd64|usr/sbin/update-passwd|565e3e900a4b6d2a1359e803a5d644c5096692b6e31077c352da4f833f2f553f|passwd,passwd-entry
linux/amd64|usr/sbin/useradd|a4019d514585c2c4b9c6be4bc97e6983d910a41956da428cc744577e72cdc5ff|KEY
"""
REVIEWED_PLATFORM_BINARY_ASSIGNMENTS = {"linux/arm64": {}, "linux/amd64": {}}
for line in _REVIEWED_PLATFORM_BINARY_ASSIGNMENTS.splitlines():
    if not line:
        continue
    platform, path, digest, keys = line.split("|", 3)
    REVIEWED_PLATFORM_BINARY_ASSIGNMENTS[platform][path] = {
        digest: frozenset(keys.split(",")),
    }
del _REVIEWED_PLATFORM_BINARY_ASSIGNMENTS
AGE_PRIVATE_IDENTITY = re.compile(rb"(?:^|[\x00\r\n])AGE-SECRET-KEY-1[0-9A-Z]+(?:$|[\x00\r\n])")
LINK_AGE_PRIVATE_IDENTITY = re.compile(rb"AGE-SECRET-KEY-1[0-9A-Z]+")
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
ASSIGNMENT_BYTES = re.compile(
    rb"(?=(?<![A-Za-z0-9_.-])[\"']?([A-Za-z_][A-Za-z0-9_.-]*)[\"']?\s*(?:=|:)\s*([^\s\x00]{1,4096}))"
)
AUTHORIZATION_CREDENTIAL_BYTES = re.compile(
    rb"(?<![A-Za-z0-9_.-])(?:proxy[_.-]?)?authorization\s*(?:=|:)\s*(?:bearer|token)\s+[^\s\"']+",
    re.IGNORECASE,
)
STANDALONE_BEARER_BYTES = re.compile(
    rb"(?<![A-Za-z0-9_.-])bearer\s+[^\s\x00]+",
    re.IGNORECASE,
)
MAX_FILE_SCAN_BYTES = 64 * 1024 * 1024
MAX_TOTAL_SCAN_BYTES = 1024 * 1024 * 1024
MAX_LINK_TARGET_SCAN_BYTES = 64 * 1024

ALLOWED_RUNTIME_ASSIGNMENT_KEYS = {
    "etc/group": {"_ssh"},
    "etc/group-": {"_ssh"},
    "etc/gshadow": {"_ssh"},
    "etc/gshadow-": {"_ssh"},
    "etc/nsswitch.conf": {"passwd"},
    "etc/security/namespace.init": {"passwd"},
    "etc/ssl/openssl.cnf": {"database", "input_password", "key", "output_password", "private_key", "secret", "signer_key"},
    "opt/borg/bin/Activate.ps1": {"Key", "key"},
    "usr/bin/apt-key": {"APT_KEY_NET_UPDATE_ENABLED", "FORCED_SECRET_KEYRING", "KEY", "Key", "all_add_key"},
    "usr/bin/ssh-argv0": {"ssh-argv0"},
    "usr/bin/ssh-copy-id": {"AUTH_KEY_DIR", "AUTH_KEY_FILE", "KEY_NO", "SSH", "SSH_OPTS"},
    "usr/lib/openssh/agent-launch": {"SSH_AGENT_LAUNCHER", "SSH_AUTH_SOCK"},
    "usr/local/lib/pkgconfig/python-3.13-embed.pc": {"Libs.private"},
    "usr/local/lib/pkgconfig/python-3.13.pc": {"Libs.private"},
    "usr/local/lib/python3.13/idlelib/News3.txt": {"config_key"},
    "usr/local/lib/python3.13/venv/scripts/common/Activate.ps1": {"Key", "key"},
    "usr/local/share/man/man1/python3.13.1": {"repository"},
    "usr/sbin/adduser": {"PASSWD", "ask_passwd", "disabled-password", "passwd"},
    "usr/sbin/deluser": {"passwd"},
    "usr/sbin/dpkg-fsys-usrunmess": {"database"},
    "usr/sbin/pam-auth-update": {"auth", "pam-auth-update", "password"},
    "usr/share/doc/libgcrypt20/copyright": {"Repository"},
    "usr/share/libc-bin/nsswitch.conf": {"passwd"},
    "usr/share/pam-configs/unix": {"Auth", "Auth-Initial", "Auth-Type", "Password", "Password-Initial", "Password-Type"},
    "usr/share/polkit-1/actions/org.dpkg.pkexec.update-alternatives.policy": {"key"},
    "var/cache/debconf/templates.dat": {"ssh"},
    "var/cache/debconf/templates.dat-old": {"ssh"},
}
# Exact path/key pairs reviewed from the pinned Python, Borg, pip, and Perl runtime.
# Unknown files and new sensitive keys fail closed, including beneath vendor prefixes.
_REVIEWED_SOURCE_ASSIGNMENTS = r"""
opt/borg/lib/python3.13/site-packages/borg/archive.py:Repository,Repository.ObjectNotFound,key,key_serialized,make_key,repository,self.key,self.repository
opt/borg/lib/python3.13/site-packages/borg/archiver.py:BORG_PASSPHRASE,REPO,Repository.ObjectNotFound,args.repo_only,borg_key_change-passphrase,borg_key_export,cached_repo,change_passphrase_epilog,debug_dump_repo_objs_epilog,debug_search_repo_objs_epilog,id_key,key,key.tam_required,key_export_epilog,key_files,key_import_epilog,key_name,key_new,key_new.chunk_seed,key_new.enc_hmac_key,key_new.enc_key,key_new.id_key,key_new.repository_id,key_new.target,key_old,key_parsers,manifest.key,passphrase,repo,repository,repository._active_txn,ssh
opt/borg/lib/python3.13/site-packages/borg/cache.py:decrypted_repository,key,repo_features,repo_location,repository,repository_location,self.key,self.key_type,self.key_type_file,self.repository
opt/borg/lib/python3.13/site-packages/borg/constants.py:REPOSITORY_README
opt/borg/lib/python3.13/site-packages/borg/crypto/key.py:AUTHENTICATED_NO_KEY,AVAILABLE_KEY_TYPES,REPO,enc_hmac_key,enc_key,id_key,key,key.ARG_NAME,key.TYPE,key._passphrase,key.chunk_seed,key.id_key,key.repository_id,key_b64,key_data,key_type,mac_key,passphrase,repo,repo_id,repository_id,self.enc_hmac_key,self.enc_key,self.id_key,self.repository,self.repository_id,tam_key
opt/borg/lib/python3.13/site-packages/borg/crypto/keymanager.py:KeyBlobStorage.REPO,Repository.ObjectNotFound,key,key_data,self.repository
opt/borg/lib/python3.13/site-packages/borg/crypto/nonces.py:repo_free_nonce,self.commit_repo_nonce_reservation,self.get_repo_free_nonce,self.repository
opt/borg/lib/python3.13/site-packages/borg/fuse.py:key,repo,self.decrypted_repository,self.key,self.repository_uncached
opt/borg/lib/python3.13/site-packages/borg/helpers/fs.py:key,repository_id
opt/borg/lib/python3.13/site-packages/borg/helpers/manifest.py:Repository.ObjectNotFound,SUPPORTED_REPO_FEATURES,key,self.key,self.key.tam_required,self.repository
opt/borg/lib/python3.13/site-packages/borg/helpers/misc.py:SSH_ORIGINAL_COMMAND,key
opt/borg/lib/python3.13/site-packages/borg/helpers/msgpack.py:strict_map_key
opt/borg/lib/python3.13/site-packages/borg/helpers/parseformat.py:KEY_DESCRIPTIONS,KEY_GROUPS,cls.KEY_DESCRIPTIONS,cls.KEY_GROUPS,key,repo,repo_raw,repository,self.key,self.repository,ssh,ssh_re
opt/borg/lib/python3.13/site-packages/borg/helpers/process.py:lp_key,ssh
opt/borg/lib/python3.13/site-packages/borg/paperkey.html:key
opt/borg/lib/python3.13/site-packages/borg/patterns.py:key
opt/borg/lib/python3.13/site-packages/borg/remote.py:key,key_,load_key,repository_iterator,restrict_to_repository_with_sep,save_key,self.repository,ssh
opt/borg/lib/python3.13/site-packages/borg/repository.py:Repository,key,key_not_in_shadow_index,repo,repo_version
opt/borg/lib/python3.13/site-packages/borg/testsuite/archive.py:key,repository,self.repository
opt/borg/lib/python3.13/site-packages/borg/testsuite/archiver.py:BORG_KEY_FILE,BORG_PASSPHRASE,BORG_RELOCATED_REPO_ACCESS_IS_OK,BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK,backup_key,base_key,exported_key_file,imported_key_contents,imported_key_file,info_repo,info_repo_json,key,key.tam_required,key_contents,key_contents2,key_file,list_repo,remote_repo,repo,repo_id,repo_info,repo_key,repo_key.enc_key,repo_key2,repo_key2.enc_key,repo_list,repository,repository._location,repository_id,repository_location,repository_path,secret,self.repository_location,self.repository_path,this-repository-does-not-exist
opt/borg/lib/python3.13/site-packages/borg/testsuite/benchmark.py:repo
opt/borg/lib/python3.13/site-packages/borg/testsuite/cache.py:key,key.compressor,repository,self.repository_location
opt/borg/lib/python3.13/site-packages/borg/testsuite/crypto.py:enc_key,mac_key
opt/borg/lib/python3.13/site-packages/borg/testsuite/hashindex.py:key_count
opt/borg/lib/python3.13/site-packages/borg/testsuite/helpers.py:escaped_key,key,repo,repository_id,ssh
opt/borg/lib/python3.13/site-packages/borg/testsuite/issue_8535.py:key,repo
opt/borg/lib/python3.13/site-packages/borg/testsuite/key.py:identified_key_class,key,key.chunk_seed,key.enc_hmac_key,key.enc_key,key.id_key,key.tam_required,keyfile2_key_file,keyfile_blake2_key_file,loaded_key,passphrase,repository,self.key_data
opt/borg/lib/python3.13/site-packages/borg/testsuite/nonces.py:self.repository,self.repository.next_free
opt/borg/lib/python3.13/site-packages/borg/testsuite/platform.py:acl_key
opt/borg/lib/python3.13/site-packages/borg/testsuite/remote.py:key,key.compressor,repository,self.repository_location
opt/borg/lib/python3.13/site-packages/borg/testsuite/repository.py:corrupted_key,key,key_size,repository,self.old_repo_handlers,self.old_repo_level,self.repository,self.repository._args,self.repository.additional_free_space,self.repository.append_only,self.repository.compact_segments,self.repository.io.delete_segment,self.repository.storage_quota,self.repository.storage_quota_use,self.repository.write_index,ssh
opt/borg/lib/python3.13/site-packages/borg/testsuite/upgrader.py:attic_key_file,attic_repo,repo_path,repository,repository.
opt/borg/lib/python3.13/site-packages/borg/upgrader.py:repository.id_str
opt/borg/lib/python3.13/site-packages/borgbackup-1.4.5.dist-info/METADATA:repo
opt/borg/lib/python3.13/site-packages/msgpack-1.2.1.dist-info/METADATA:strict_map_key
opt/borg/lib/python3.13/site-packages/msgpack/fallback.py:key,self._strict_map_key,strict_map_key
opt/borg/lib/python3.13/site-packages/packaging/_parser.py:extra_token,name_token,token
opt/borg/lib/python3.13/site-packages/packaging/_tokenizer.py:Token,close_token,open_token,self.next_token,token
opt/borg/lib/python3.13/site-packages/packaging/direct_url.py:key,strip_user_password
opt/borg/lib/python3.13/site-packages/packaging/licenses/__init__.py:final_token,token
opt/borg/lib/python3.13/site-packages/packaging/licenses/_spdx.py:ssh-keyscan,ssh-openssh,ssh-short
opt/borg/lib/python3.13/site-packages/packaging/markers.py:environment_key,key
opt/borg/lib/python3.13/site-packages/packaging/metadata.py:private
opt/borg/lib/python3.13/site-packages/packaging/pylock.py:key
opt/borg/lib/python3.13/site-packages/packaging/specifiers.py:key
opt/borg/lib/python3.13/site-packages/packaging/tags.py:key
opt/borg/lib/python3.13/site-packages/packaging/version.py:_key_cache,key,new_version._key_cache,other._key_cache,self._key,self._key_cache
opt/borg/lib/python3.13/site-packages/pip/_internal/cache.py:key_parts
opt/borg/lib/python3.13/site-packages/pip/_internal/cli/cmdoptions.py:KEY
opt/borg/lib/python3.13/site-packages/pip/_internal/cli/index_command.py:session.auth.keyring_provider,session.auth.prompting
opt/borg/lib/python3.13/site-packages/pip/_internal/cli/parser.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/commands/configuration.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/commands/install.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/commands/list.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/commands/search.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/commands/show.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/configuration.py:key,orig_key
opt/borg/lib/python3.13/site-packages/pip/_internal/exceptions.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/index/package_finder.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/locations/__init__.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/metadata/_json.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/models/target_python.py:key_values
opt/borg/lib/python3.13/site-packages/pip/_internal/network/auth.py:Password,auth,index_url_user_password,key,kr_auth,netrc_auth,password,url_user_password
opt/borg/lib/python3.13/site-packages/pip/_internal/network/cache.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/network/session.py:self.auth,self.auth.index_urls
opt/borg/lib/python3.13/site-packages/pip/_internal/operations/build/build_tracker.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/operations/check.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/operations/freeze.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/operations/install/wheel.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/req/req_set.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/req/req_uninstall.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/resolution/resolvelib/factory.py:cache_key,key
opt/borg/lib/python3.13/site-packages/pip/_internal/resolution/resolvelib/resolver.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/self_outdated_check.py:key,key_bytes
opt/borg/lib/python3.13/site-packages/pip/_internal/utils/misc.py:auth,password,secret,self.secret
opt/borg/lib/python3.13/site-packages/pip/_internal/utils/pylock.py:key
opt/borg/lib/python3.13/site-packages/pip/_internal/vcs/bazaar.py:repo,repo_name,ssh
opt/borg/lib/python3.13/site-packages/pip/_internal/vcs/git.py:repo_name,repo_root,repo_url,ssh
opt/borg/lib/python3.13/site-packages/pip/_internal/vcs/mercurial.py:repo_config,repo_name,repo_root
opt/borg/lib/python3.13/site-packages/pip/_internal/vcs/subversion.py:password,repo_name,ssh
opt/borg/lib/python3.13/site-packages/pip/_internal/vcs/versioncontrol.py:inner_most_repo_path,key,password,repo,repo_dir,repo_name,repo_path,repo_root,repo_url,secret_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/cachecontrol/cache.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/cachecontrol/caches/file_cache.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/cachecontrol/caches/redis_cache.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/cachecontrol/controller.py:private
opt/borg/lib/python3.13/site-packages/pip/_vendor/distlib/compat.py:call_key,key,result.key
opt/borg/lib/python3.13/site-packages/pip/_vendor/distlib/util.py:DEFAULT_REPOSITORY,password,repository
opt/borg/lib/python3.13/site-packages/pip/_vendor/distro/distro.py:Key,token
opt/borg/lib/python3.13/site-packages/pip/_vendor/msgpack/fallback.py:key,self._strict_map_key,strict_map_key
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/_parser.py:extra_token,name_token,token
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/_tokenizer.py:Token,close_token,open_token,self.next_token,token
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/direct_url.py:key,strip_user_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/licenses/__init__.py:final_token,token
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/licenses/_spdx.py:ssh-keyscan,ssh-openssh,ssh-short
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/markers.py:environment_key,key
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/metadata.py:private
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/pylock.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/specifiers.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/tags.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/packaging/version.py:_key_cache,key,new_version._key_cache,other._key_cache,self._key,self._key_cache
opt/borg/lib/python3.13/site-packages/pip/_vendor/pkg_resources/__init__.py:canonical_key,distribution_key,key,req.key,self._key,self.by_key,self.key
opt/borg/lib/python3.13/site-packages/pip/_vendor/platformdirs/unix.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/platformdirs/windows.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/pygments/lexer.py:cls.token_variants,key,token
opt/borg/lib/python3.13/site-packages/pip/_vendor/pygments/lexers/__init__.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/pygments/sphinxext.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/pygments/style.py:Token
opt/borg/lib/python3.13/site-packages/pip/_vendor/pygments/token.py:Token,Token.Number,Token.String,Token.Token
opt/borg/lib/python3.13/site-packages/pip/_vendor/pygments/util.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/requests/adapters.py:conn.key_file,password
opt/borg/lib/python3.13/site-packages/pip/_vendor/requests/api.py:Auth.,auth
opt/borg/lib/python3.13/site-packages/pip/_vendor/requests/auth.py:auth,password,s_auth,self.password
opt/borg/lib/python3.13/site-packages/pip/_vendor/requests/models.py:auth,key,self.auth,url_auth
opt/borg/lib/python3.13/site-packages/pip/_vendor/requests/sessions.py:Auth.,auth,new_auth,password,self.auth
opt/borg/lib/python3.13/site-packages/pip/_vendor/requests/utils.py:auth,key,key_without_value,password.
opt/borg/lib/python3.13/site-packages/pip/_vendor/resolvelib/resolvers/resolution.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/resolvelib/structs.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/__init__.py:private
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/_emoji_codes.py:closed_lock_with_key,japanese_secret_button,key,locked_with_key,old_key,secret
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/_inspect.py:key,key_text,private,self.private
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/console.py:key,password
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/default_styles.py:json.key,scope.key,scope.key.special
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/layout.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/markup.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/measure.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/palette.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/pretty.py:child_node.key_repr,child_node.key_separator,key_repr,key_separator,node.key_repr,self.key_repr
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/prompt.py:password,self.password
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/scope.py:key,key_text
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/syntax.py:Token,token,token_type
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/text.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/rich/traceback.py:key,scope.key,scope.key.special,token_style
opt/borg/lib/python3.13/site-packages/pip/_vendor/tomli/_parser.py:BARE_KEY_CHARS,KEY_INITIAL_CHARS,Key,MAX_KEY_PARTS,abs_key_parent,key,key_parent,key_part,key_stem,last_key
opt/borg/lib/python3.13/site-packages/pip/_vendor/tomli/_types.py:Key
opt/borg/lib/python3.13/site-packages/pip/_vendor/tomli_w/_writer.py:BARE_KEY_CHARS,key_part,only_bare_key_chars
opt/borg/lib/python3.13/site-packages/pip/_vendor/truststore/_api.py:password,self._ctx.post_handshake_auth
opt/borg/lib/python3.13/site-packages/pip/_vendor/truststore/_windows.py:OID_PKIX_KP_SERVER_AUTH
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/_base_connection.py:key_file,key_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/_collections.py:key,key_lower
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/connection.py:key_file,key_password,self.key_file,self.key_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/connectionpool.py:key_file,key_password,self.key_file,self.key_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/contrib/emscripten/connection.py:key_file,key_password,self.key_file,self.key_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/contrib/pyopenssl.py:password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/contrib/socks.py:password,proxy_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/http2/probe.py:key,key_lock
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/poolmanager.py:key.,key__proxy,key__proxy_config,key__proxy_headers,key__socks_options,key_assert_fingerprint,key_assert_hostname,key_block,key_blocksize,key_ca_cert_data,key_ca_cert_dir,key_ca_certs,key_cert_file,key_cert_reqs,key_class,key_class._fields,key_fn_by_scheme,key_headers,key_host,key_key_file,key_key_password,key_maxsize,key_port,key_retries,key_scheme,key_server_hostname,key_socket_options,key_source_address,key_ssl_context,key_ssl_maximum_version,key_ssl_minimum_version,key_ssl_version,key_timeout,pool_key,pool_key_constructor,self.key_fn_by_scheme
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/util/request.py:authorization,basic_auth,proxy-authorization,proxy_basic_auth,token
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/util/ssl_.py:context.post_handshake_auth,key_file,key_password
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/util/ssl_match_hostname.py:key
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/util/timeout.py:token
opt/borg/lib/python3.13/site-packages/pip/_vendor/urllib3/util/url.py:auth
usr/lib/<arch>/perl-base/Config_heavy.pl:key
usr/lib/<arch>/perl-base/Getopt/Long.pm:key,key_valid
usr/lib/<arch>/perl-base/Tie/Hash.pm:key
usr/lib/<arch>/perl-base/fields.pm:PRIVATE
usr/lib/<arch>/perl-base/warnings.pm:private_use
usr/local/include/python3.13/cpython/setobject.h:key
usr/local/include/python3.13/internal/mimalloc/mimalloc.h:private
usr/local/include/python3.13/internal/mimalloc/mimalloc/prim.h:_mi_heap_default_key
usr/local/include/python3.13/internal/pycore_opcode_metadata.h:BUILD_CONST_KEY_MAP
usr/local/include/python3.13/internal/pycore_uop_metadata.h:_BUILD_CONST_KEY_MAP
usr/local/lib/python3.13/_collections_abc.py:key
usr/local/lib/python3.13/_opcode_metadata.py:BUILD_CONST_KEY_MAP
usr/local/lib/python3.13/_pydecimal.py:other_key,private,self_key
usr/local/lib/python3.13/_pyrepl/completing_reader.py:key
usr/local/lib/python3.13/_pyrepl/input.py:key
usr/local/lib/python3.13/_pyrepl/keymap.py:key
usr/local/lib/python3.13/_pyrepl/reader.py:key
usr/local/lib/python3.13/_pyrepl/windows_console.py:KEY_EVENT,key,key_event,key_event.wVirtualKeyCode,raw_key
usr/local/lib/python3.13/_strptime.py:group_key,key
usr/local/lib/python3.13/_sysconfigdata__linux_<arch>.py:HAVE_CURSES_HAS_KEY,HAVE_STRUCT_PASSWD_PW_GECOS,HAVE_STRUCT_PASSWD_PW_PASSWD,PTHREAD_KEY_T_IS_COMPATIBLE_WITH_INT,SIZEOF_PTHREAD_KEY_T
usr/local/lib/python3.13/_threading_local.py:key,self.key
usr/local/lib/python3.13/ast.py:key
usr/local/lib/python3.13/asyncio/base_events.py:key
usr/local/lib/python3.13/asyncio/selector_events.py:key
usr/local/lib/python3.13/asyncio/streams.py:key
usr/local/lib/python3.13/asyncio/windows_events.py:key
usr/local/lib/python3.13/bisect.py:key
usr/local/lib/python3.13/collections/__init__.py:key,link.key
usr/local/lib/python3.13/concurrent/futures/_base.py:key
usr/local/lib/python3.13/config-3.13-<arch>/Makefile:regen-token
usr/local/lib/python3.13/configparser.py:key
usr/local/lib/python3.13/copy.py:key
usr/local/lib/python3.13/copyreg.py:key
usr/local/lib/python3.13/csv.py:key
usr/local/lib/python3.13/ctypes/_aix.py:key
usr/local/lib/python3.13/ctypes/util.py:key
usr/local/lib/python3.13/curses/has_key.py:_curses.KEY_A1,_curses.KEY_A3,_curses.KEY_B2,_curses.KEY_BACKSPACE,_curses.KEY_BEG,_curses.KEY_BTAB,_curses.KEY_C1,_curses.KEY_C3,_curses.KEY_CANCEL,_curses.KEY_CATAB,_curses.KEY_CLEAR,_curses.KEY_CLOSE,_curses.KEY_COMMAND,_curses.KEY_COPY,_curses.KEY_CREATE,_curses.KEY_CTAB,_curses.KEY_DC,_curses.KEY_DL,_curses.KEY_DOWN,_curses.KEY_EIC,_curses.KEY_END,_curses.KEY_ENTER,_curses.KEY_EOL,_curses.KEY_EOS,_curses.KEY_EXIT,_curses.KEY_F0,_curses.KEY_F1,_curses.KEY_F10,_curses.KEY_F11,_curses.KEY_F12,_curses.KEY_F13,_curses.KEY_F14,_curses.KEY_F15,_curses.KEY_F16,_curses.KEY_F17,_curses.KEY_F18,_curses.KEY_F19,_curses.KEY_F2,_curses.KEY_F20,_curses.KEY_F21,_curses.KEY_F22,_curses.KEY_F23,_curses.KEY_F24,_curses.KEY_F25,_curses.KEY_F26,_curses.KEY_F27,_curses.KEY_F28,_curses.KEY_F29,_curses.KEY_F3,_curses.KEY_F30,_curses.KEY_F31,_curses.KEY_F32,_curses.KEY_F33,_curses.KEY_F34,_curses.KEY_F35,_curses.KEY_F36,_curses.KEY_F37,_curses.KEY_F38,_curses.KEY_F39,_curses.KEY_F4,_curses.KEY_F40,_curses.KEY_F41,_curses.KEY_F42,_curses.KEY_F43,_curses.KEY_F44,_curses.KEY_F45,_curses.KEY_F46,_curses.KEY_F47,_curses.KEY_F48,_curses.KEY_F49,_curses.KEY_F5,_curses.KEY_F50,_curses.KEY_F51,_curses.KEY_F52,_curses.KEY_F53,_curses.KEY_F54,_curses.KEY_F55,_curses.KEY_F56,_curses.KEY_F57,_curses.KEY_F58,_curses.KEY_F59,_curses.KEY_F6,_curses.KEY_F60,_curses.KEY_F61,_curses.KEY_F62,_curses.KEY_F63,_curses.KEY_F7,_curses.KEY_F8,_curses.KEY_F9,_curses.KEY_FIND,_curses.KEY_HELP,_curses.KEY_HOME,_curses.KEY_IC,_curses.KEY_IL,_curses.KEY_LEFT,_curses.KEY_LL,_curses.KEY_MARK,_curses.KEY_MESSAGE,_curses.KEY_MOVE,_curses.KEY_NEXT,_curses.KEY_NPAGE,_curses.KEY_OPEN,_curses.KEY_OPTIONS,_curses.KEY_PPAGE,_curses.KEY_PREVIOUS,_curses.KEY_PRINT,_curses.KEY_REDO,_curses.KEY_REFERENCE,_curses.KEY_REFRESH,_curses.KEY_REPLACE,_curses.KEY_RESTART,_curses.KEY_RESUME,_curses.KEY_RIGHT,_curses.KEY_SAVE,_curses.KEY_SBEG,_curses.KEY_SCANCEL,_curses.KEY_SCOMMAND,_curses.KEY_SCOPY,_curses.KEY_SCREATE,_curses.KEY_SDC,_curses.KEY_SDL,_curses.KEY_SELECT,_curses.KEY_SEND,_curses.KEY_SEOL,_curses.KEY_SEXIT,_curses.KEY_SF,_curses.KEY_SFIND,_curses.KEY_SHELP,_curses.KEY_SHOME,_curses.KEY_SIC,_curses.KEY_SLEFT,_curses.KEY_SMESSAGE,_curses.KEY_SMOVE,_curses.KEY_SNEXT,_curses.KEY_SOPTIONS,_curses.KEY_SPREVIOUS,_curses.KEY_SPRINT,_curses.KEY_SR,_curses.KEY_SREDO,_curses.KEY_SREPLACE,_curses.KEY_SRIGHT,_curses.KEY_SRSUME,_curses.KEY_SSAVE,_curses.KEY_SSUSPEND,_curses.KEY_STAB,_curses.KEY_SUNDO,_curses.KEY_SUSPEND,_curses.KEY_UNDO,_curses.KEY_UP
usr/local/lib/python3.13/curses/textpad.py:KEY_BACKSPACE,KEY_DOWN,KEY_LEFT,KEY_RIGHT,KEY_UP
usr/local/lib/python3.13/dataclasses.py:Key,Private
usr/local/lib/python3.13/dbm/dumb.py:key
usr/local/lib/python3.13/dbm/sqlite3.py:DELETE_KEY,LOOKUP_KEY,key
usr/local/lib/python3.13/difflib.py:format_key
usr/local/lib/python3.13/doctest.py:key
usr/local/lib/python3.13/email/_header_value_parser.py:TOKEN_ENDS,_non_token_end_matcher,key,last.token_type,mailbox.token_type,obs_local_part.token_type,p.token_type,param.token_type,part.token_type,self.token_type,t.token_type,tok.token_type,token,token.token_type,token_type,value.token_type,x.token_type
usr/local/lib/python3.13/email/generator.py:token
usr/local/lib/python3.13/email/message.py:key
usr/local/lib/python3.13/email/utils.py:key
usr/local/lib/python3.13/enum.py:key
usr/local/lib/python3.13/ftplib.py:passwd
usr/local/lib/python3.13/functools.py:cache_token,current_token,key,make_key
usr/local/lib/python3.13/getpass.py:Password,passwd
usr/local/lib/python3.13/gettext.py:_token_pattern,key
usr/local/lib/python3.13/heapq.py:key
usr/local/lib/python3.13/hmac.py:key
usr/local/lib/python3.13/http/client.py:context.post_handshake_auth,token
usr/local/lib/python3.13/http/cookiejar.py:HEADER_JOIN_TOKEN_RE,HEADER_TOKEN_RE,key,token
usr/local/lib/python3.13/http/cookies.py:_is_legal_key,key,self._key
usr/local/lib/python3.13/http/server.py:authorization,key
usr/local/lib/python3.13/idlelib/browser.py:key
usr/local/lib/python3.13/idlelib/config.py:key
usr/local/lib/python3.13/idlelib/config_key.py:final_key,key,key_sequences,self.current_key_sequences,self.key_string
usr/local/lib/python3.13/idlelib/configdialog.py:create_new_key_set,current_key_sequences,current_key_set_name,custom_key_list,frame_key_sets,key,key_set,key_set_changes,prev_key_set_name,save_new_key_set
usr/local/lib/python3.13/idlelib/debugobj.py:key
usr/local/lib/python3.13/idlelib/editor.py:key
usr/local/lib/python3.13/idlelib/filelist.py:key
usr/local/lib/python3.13/idlelib/help.py:KEY
usr/local/lib/python3.13/idlelib/multicall.py:key
usr/local/lib/python3.13/idlelib/stackviewer.py:key
usr/local/lib/python3.13/idlelib/tree.py:key
usr/local/lib/python3.13/imaplib.py:AUTH,PASSWD,password,self.password
usr/local/lib/python3.13/importlib/_bootstrap.py:key,self.key
usr/local/lib/python3.13/importlib/_bootstrap_external.py:REGISTRY_KEY,REGISTRY_KEY_DEBUG,_CASE_INSENSITIVE_PLATFORMS_BYTES_KEY,_CASE_INSENSITIVE_PLATFORMS_STR_KEY,key,registry_key
usr/local/lib/python3.13/importlib/metadata/__init__.py:key
usr/local/lib/python3.13/importlib/metadata/_adapters.py:key
usr/local/lib/python3.13/importlib/metadata/_collections.py:key
usr/local/lib/python3.13/importlib/metadata/_itertools.py:key
usr/local/lib/python3.13/importlib/metadata/_meta.py:key
usr/local/lib/python3.13/importlib/resources/readers.py:key
usr/local/lib/python3.13/inspect.py:_token_info,key,token,token_stream
usr/local/lib/python3.13/ipaddress.py:_private_networks,_private_networks_exceptions,address.is_private,key
usr/local/lib/python3.13/json/decoder.py:key
usr/local/lib/python3.13/json/encoder.py:key,key_separator,self.key_separator
usr/local/lib/python3.13/linecache.py:key
usr/local/lib/python3.13/logging/config.py:result.key
usr/local/lib/python3.13/logging/handlers.py:LOG_AUTH,auth,self.password
usr/local/lib/python3.13/mailbox.py:bad_key,key,key_list,new_key,self._next_key,temp_key
usr/local/lib/python3.13/multiprocessing/managers.py:self._token,token,token.address
usr/local/lib/python3.13/multiprocessing/resource_sharer.py:key,self._key
usr/local/lib/python3.13/multiprocessing/util.py:self._key
usr/local/lib/python3.13/netrc.py:password,token
usr/local/lib/python3.13/os.py:key
usr/local/lib/python3.13/pdb.py:token.NAME,token_string,token_type
usr/local/lib/python3.13/pickle.py:key
usr/local/lib/python3.13/platform.py:key
usr/local/lib/python3.13/plistlib.py:key_refs,self.current_key,token
usr/local/lib/python3.13/poplib.py:AUTH-RESP-CODE,secret
usr/local/lib/python3.13/pprint.py:_safe_key,key
usr/local/lib/python3.13/pstats.py:key
usr/local/lib/python3.13/pyclbr.py:key,lineno_key
usr/local/lib/python3.13/pydoc.py:key
usr/local/lib/python3.13/pydoc_data/module_docs.py:token
usr/local/lib/python3.13/pydoc_data/topics.py:key,key_value_pattern
usr/local/lib/python3.13/re/__init__.py:key
usr/local/lib/python3.13/reprlib.py:key
usr/local/lib/python3.13/selectors.py:fd_to_key,fd_to_key_get,key,key.data,key.events,self._fd_to_key
usr/local/lib/python3.13/shlex.py:Token,self.token,token
usr/local/lib/python3.13/site-packages/pip/_internal/cache.py:key_parts
usr/local/lib/python3.13/site-packages/pip/_internal/cli/cmdoptions.py:KEY
usr/local/lib/python3.13/site-packages/pip/_internal/cli/index_command.py:session.auth.keyring_provider,session.auth.prompting
usr/local/lib/python3.13/site-packages/pip/_internal/cli/parser.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/commands/configuration.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/commands/install.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/commands/list.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/commands/search.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/commands/show.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/configuration.py:key,orig_key
usr/local/lib/python3.13/site-packages/pip/_internal/exceptions.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/index/package_finder.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/locations/__init__.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/metadata/_json.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/models/target_python.py:key_values
usr/local/lib/python3.13/site-packages/pip/_internal/network/auth.py:Password,auth,index_url_user_password,key,kr_auth,netrc_auth,password,url_user_password
usr/local/lib/python3.13/site-packages/pip/_internal/network/cache.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/network/session.py:self.auth,self.auth.index_urls
usr/local/lib/python3.13/site-packages/pip/_internal/operations/build/build_tracker.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/operations/check.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/operations/freeze.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/operations/install/wheel.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/req/req_set.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/req/req_uninstall.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/resolution/resolvelib/factory.py:cache_key,key
usr/local/lib/python3.13/site-packages/pip/_internal/resolution/resolvelib/resolver.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/self_outdated_check.py:key,key_bytes
usr/local/lib/python3.13/site-packages/pip/_internal/utils/misc.py:auth,password,secret,self.secret
usr/local/lib/python3.13/site-packages/pip/_internal/utils/pylock.py:key
usr/local/lib/python3.13/site-packages/pip/_internal/vcs/bazaar.py:repo,repo_name,ssh
usr/local/lib/python3.13/site-packages/pip/_internal/vcs/git.py:repo_name,repo_root,repo_url,ssh
usr/local/lib/python3.13/site-packages/pip/_internal/vcs/mercurial.py:repo_config,repo_name,repo_root
usr/local/lib/python3.13/site-packages/pip/_internal/vcs/subversion.py:password,repo_name,ssh
usr/local/lib/python3.13/site-packages/pip/_internal/vcs/versioncontrol.py:inner_most_repo_path,key,password,repo,repo_dir,repo_name,repo_path,repo_root,repo_url,secret_password
usr/local/lib/python3.13/site-packages/pip/_vendor/cachecontrol/cache.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/cachecontrol/caches/file_cache.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/cachecontrol/caches/redis_cache.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/cachecontrol/controller.py:private
usr/local/lib/python3.13/site-packages/pip/_vendor/distlib/compat.py:call_key,key,result.key
usr/local/lib/python3.13/site-packages/pip/_vendor/distlib/util.py:DEFAULT_REPOSITORY,password,repository
usr/local/lib/python3.13/site-packages/pip/_vendor/distro/distro.py:Key,token
usr/local/lib/python3.13/site-packages/pip/_vendor/msgpack/fallback.py:key,self._strict_map_key,strict_map_key
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/_parser.py:extra_token,name_token,token
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/_tokenizer.py:Token,close_token,open_token,self.next_token,token
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/direct_url.py:key,strip_user_password
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/licenses/__init__.py:final_token,token
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/licenses/_spdx.py:ssh-keyscan,ssh-openssh,ssh-short
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/markers.py:environment_key,key
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/metadata.py:private
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/pylock.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/specifiers.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/tags.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/packaging/version.py:_key_cache,key,new_version._key_cache,other._key_cache,self._key,self._key_cache
usr/local/lib/python3.13/site-packages/pip/_vendor/pkg_resources/__init__.py:canonical_key,distribution_key,key,req.key,self._key,self.by_key,self.key
usr/local/lib/python3.13/site-packages/pip/_vendor/platformdirs/unix.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/platformdirs/windows.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/pygments/lexer.py:cls.token_variants,key,token
usr/local/lib/python3.13/site-packages/pip/_vendor/pygments/lexers/__init__.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/pygments/sphinxext.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/pygments/style.py:Token
usr/local/lib/python3.13/site-packages/pip/_vendor/pygments/token.py:Token,Token.Number,Token.String,Token.Token
usr/local/lib/python3.13/site-packages/pip/_vendor/pygments/util.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/requests/adapters.py:conn.key_file,password
usr/local/lib/python3.13/site-packages/pip/_vendor/requests/api.py:Auth.,auth
usr/local/lib/python3.13/site-packages/pip/_vendor/requests/auth.py:auth,password,s_auth,self.password
usr/local/lib/python3.13/site-packages/pip/_vendor/requests/models.py:auth,key,self.auth,url_auth
usr/local/lib/python3.13/site-packages/pip/_vendor/requests/sessions.py:Auth.,auth,new_auth,password,self.auth
usr/local/lib/python3.13/site-packages/pip/_vendor/requests/utils.py:auth,key,key_without_value,password.
usr/local/lib/python3.13/site-packages/pip/_vendor/resolvelib/resolvers/resolution.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/resolvelib/structs.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/__init__.py:private
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/_emoji_codes.py:closed_lock_with_key,japanese_secret_button,key,locked_with_key,old_key,secret
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/_inspect.py:key,key_text,private,self.private
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/console.py:key,password
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/default_styles.py:json.key,scope.key,scope.key.special
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/layout.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/markup.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/measure.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/palette.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/pretty.py:child_node.key_repr,child_node.key_separator,key_repr,key_separator,node.key_repr,self.key_repr
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/prompt.py:password,self.password
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/scope.py:key,key_text
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/syntax.py:Token,token,token_type
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/text.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/rich/traceback.py:key,scope.key,scope.key.special,token_style
usr/local/lib/python3.13/site-packages/pip/_vendor/tomli/_parser.py:BARE_KEY_CHARS,KEY_INITIAL_CHARS,Key,MAX_KEY_PARTS,abs_key_parent,key,key_parent,key_part,key_stem,last_key
usr/local/lib/python3.13/site-packages/pip/_vendor/tomli/_types.py:Key
usr/local/lib/python3.13/site-packages/pip/_vendor/tomli_w/_writer.py:BARE_KEY_CHARS,key_part,only_bare_key_chars
usr/local/lib/python3.13/site-packages/pip/_vendor/truststore/_api.py:password,self._ctx.post_handshake_auth
usr/local/lib/python3.13/site-packages/pip/_vendor/truststore/_windows.py:OID_PKIX_KP_SERVER_AUTH
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/_base_connection.py:key_file,key_password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/_collections.py:key,key_lower
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/connection.py:key_file,key_password,self.key_file,self.key_password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/connectionpool.py:key_file,key_password,self.key_file,self.key_password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/contrib/emscripten/connection.py:key_file,key_password,self.key_file,self.key_password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/contrib/pyopenssl.py:password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/contrib/socks.py:password,proxy_password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/http2/probe.py:key,key_lock
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/poolmanager.py:key.,key__proxy,key__proxy_config,key__proxy_headers,key__socks_options,key_assert_fingerprint,key_assert_hostname,key_block,key_blocksize,key_ca_cert_data,key_ca_cert_dir,key_ca_certs,key_cert_file,key_cert_reqs,key_class,key_class._fields,key_fn_by_scheme,key_headers,key_host,key_key_file,key_key_password,key_maxsize,key_port,key_retries,key_scheme,key_server_hostname,key_socket_options,key_source_address,key_ssl_context,key_ssl_maximum_version,key_ssl_minimum_version,key_ssl_version,key_timeout,pool_key,pool_key_constructor,self.key_fn_by_scheme
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/util/request.py:authorization,basic_auth,proxy-authorization,proxy_basic_auth,token
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/util/ssl_.py:context.post_handshake_auth,key_file,key_password
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/util/ssl_match_hostname.py:key
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/util/timeout.py:token
usr/local/lib/python3.13/site-packages/pip/_vendor/urllib3/util/url.py:auth
usr/local/lib/python3.13/site.py:key
usr/local/lib/python3.13/smtplib.py:OLDSTYLE_AUTH,auth,auth_match,password,self._auth_challenge_count,self.password
usr/local/lib/python3.13/sqlite3/__init__.py:database
usr/local/lib/python3.13/ssl.py:CLIENT_AUTH,CLIENT_KEY_EXCHANGE,KEY_UPDATE,Purpose.CLIENT_AUTH,Purpose.SERVER_AUTH,SERVER_AUTH,SERVER_KEY_EXCHANGE
usr/local/lib/python3.13/statistics.py:key
usr/local/lib/python3.13/symtable.py:key
usr/local/lib/python3.13/sysconfig/__init__.py:key
usr/local/lib/python3.13/tarfile.py:key
usr/local/lib/python3.13/tkinter/__init__.py:Key,key
usr/local/lib/python3.13/tkinter/filedialog.py:key
usr/local/lib/python3.13/tkinter/font.py:key
usr/local/lib/python3.13/token.py:EXACT_TOKEN_TYPES
usr/local/lib/python3.13/tokenize.py:EXACT_TOKEN_TYPES,Token,token,token_range,token_type
usr/local/lib/python3.13/tomllib/_parser.py:BARE_KEY_CHARS,KEY_INITIAL_CHARS,Key,MAX_KEY_PARTS,abs_key_parent,key,key_parent,key_part,key_stem,last_key
usr/local/lib/python3.13/tomllib/_types.py:Key
usr/local/lib/python3.13/trace.py:key,token.INDENT,token.STRING
usr/local/lib/python3.13/traceback.py:key
usr/local/lib/python3.13/tracemalloc.py:key,key_type
usr/local/lib/python3.13/turtle.py:key
usr/local/lib/python3.13/unittest/loader.py:key
usr/local/lib/python3.13/unittest/mock.py:key
usr/local/lib/python3.13/unittest/runner.py:key
usr/local/lib/python3.13/urllib/parse.py:have_password,passwd,password
usr/local/lib/python3.13/urllib/request.py:auth,auth_header,auth_str,auth_val,context.post_handshake_auth,key,passwd,password,password_mgr,proxy_auth,proxy_auth_hdr,proxy_passwd,self.add_password,self.auth_cache,self.key_file,self.passwd,user_passwd
usr/local/lib/python3.13/urllib/robotparser.py:product_token
usr/local/lib/python3.13/venv/__init__.py:key
usr/local/lib/python3.13/warnings.py:key
usr/local/lib/python3.13/weakref.py:key,self.key
usr/local/lib/python3.13/wsgiref/headers.py:key
usr/local/lib/python3.13/wsgiref/validate.py:key
usr/local/lib/python3.13/xml/dom/minidom.py:key
usr/local/lib/python3.13/xml/dom/pulldom.py:token
usr/local/lib/python3.13/xml/dom/xmlbuilder.py:key
usr/local/lib/python3.13/xml/etree/ElementPath.py:cache_key,key,token
usr/local/lib/python3.13/xml/etree/ElementTree.py:key
usr/local/lib/python3.13/xmlrpc/client.py:auth
usr/local/lib/python3.13/zipfile/__init__.py:key
usr/local/lib/python3.13/zipimport.py:key
usr/local/lib/python3.13/zoneinfo/_common.py:key
usr/local/lib/python3.13/zoneinfo/_tzpath.py:key
usr/local/lib/python3.13/zoneinfo/_zoneinfo.py:key,obj._key
usr/share/perl5/Debconf/Config.pm:key
usr/share/perl5/Debconf/DbDriver/LDAP.pm:password
usr/share/perl5/Debconf/Format/822.pm:key
usr/share/perl5/Debconf/Template.pm:key
"""
ALLOWED_REVIEWED_SOURCE_ASSIGNMENTS = {
    path: frozenset(keys.split(","))
    for path, keys in (line.split(":", 1) for line in _REVIEWED_SOURCE_ASSIGNMENTS.splitlines() if line)
}
del _REVIEWED_SOURCE_ASSIGNMENTS


def normalize_reviewed_source_path(path):
    if re.fullmatch(r"usr/lib/[^/]+/perl-base/.+\.(?:pl|pm)", path) is not None:
        return re.sub(r"^usr/lib/[^/]+/perl-base/", "usr/lib/<arch>/perl-base/", path)
    if re.fullmatch(r"usr/local/lib/python3\.13/_sysconfigdata__linux_[^/]+\.py", path) is not None:
        return "usr/local/lib/python3.13/_sysconfigdata__linux_<arch>.py"
    if re.fullmatch(r"usr/local/lib/python3\.13/config-3\.13-[^/]+/Makefile", path) is not None:
        return "usr/local/lib/python3.13/config-3.13-<arch>/Makefile"
    return path


def runtime_assignment_allowed(path, key):
    if key in ALLOWED_RUNTIME_ASSIGNMENT_KEYS.get(path, ()):
        return True
    if path.startswith("var/lib/dpkg/info/libc6:") and path.endswith(".templates") and key == "ssh":
        return True
    normalized_path = normalize_reviewed_source_path(path)
    return key in ALLOWED_REVIEWED_SOURCE_ASSIGNMENTS.get(normalized_path, ())


def normalize_nul_private_record_path(path):
    if re.fullmatch(r"usr/lib/[^/]+/libgnutls\.so\.30\.34\.3", path) is not None:
        return "usr/lib/<arch>/libgnutls.so.30.34.3"
    return path


def nul_terminated_record_bounds(data):
    start = 0
    while True:
        terminator = data.find(b"\x00", start)
        if terminator < 0:
            return
        end = terminator + 1
        yield start, end
        start = end


def nul_private_record_allowed(data, path, match):
    record_end = data.find(b"\x00", match.end() - 1)
    if record_end < 0:
        return False
    record = data[match.start():record_end + 1]
    digest = hashlib.sha256(record).hexdigest()
    normalized_path = normalize_nul_private_record_path(path)
    return digest in ALLOWED_NUL_PRIVATE_RECORD_SHA256.get(normalized_path, ())


def check_file_contents(data, path, platform=None, location="exported regular file"):
    for sentinel in SAFE_SENTINELS:
        require(sentinel.encode() not in data, f"safe secret sentinel found in {location}: {path}")
    for header in PRIVATE_HEADERS:
        line_pattern = rb"(?:^|[\r\n])" + re.escape(header) + rb"\r?\n"
        require(re.search(line_pattern, data) is None, f"private-key material found in {location}: {path}")
        nul_pattern = rb"\x00" + re.escape(header) + rb"\r?\n[A-Za-z0-9+/=]{7,}(?:$|[\x00\r\n])"
        for match in re.finditer(nul_pattern, data):
            require(
                nul_private_record_allowed(data, path, match),
                f"private-key material found in {location}: {path}",
            )
    require(AGE_PRIVATE_IDENTITY.search(data) is None, f"age private identity found in {location}: {path}")
    require(
        AUTHORIZATION_CREDENTIAL_BYTES.search(data) is None
        and not standalone_bearer_credential_found(data, STANDALONE_BEARER_BYTES),
        f"bearer credential found in {location}: {path}",
    )
    record_bounds = iter(nul_terminated_record_bounds(data))
    current_record = next(record_bounds, None)
    current_record_digest = None
    normalized_assignment_path = NUL_ASSIGNMENT_PATH_NORMALIZATION.get(path, path)
    reviewed_assignment_hashes = ALLOWED_NUL_ASSIGNMENT_RECORD_SHA256.get(normalized_assignment_path, {})
    platform_path_hashes = REVIEWED_PLATFORM_BINARY_ASSIGNMENTS.get(platform, {}).get(path)
    file_digest = None
    for match in ASSIGNMENT_BYTES.finditer(data):
        key = match.group(1).decode("ascii", errors="ignore")
        if not sensitive_assignment_key(key):
            continue
        if platform_path_hashes is not None and file_digest is None:
            file_digest = hashlib.sha256(data).hexdigest()
        while current_record is not None and match.start() >= current_record[1]:
            current_record = next(record_bounds, None)
            current_record_digest = None
        if current_record is not None and current_record[0] <= match.start() < current_record[1]:
            allowed_hashes = reviewed_assignment_hashes.get(key, ()) if key == "strict_map_key" else ()
            if allowed_hashes:
                if current_record_digest is None:
                    current_record_digest = hashlib.sha256(data[current_record[0]:current_record[1]]).hexdigest()
                if current_record_digest in allowed_hashes:
                    continue
            if platform_path_hashes is not None:
                allowed_keys = platform_path_hashes.get(file_digest, ())
                if key in allowed_keys:
                    continue
        elif runtime_assignment_allowed(path, key):
            continue
        raise CheckFailure(f"secret-like assignment found in {location}: {path}")


def check_link_contents(data, path, location):
    for sentinel in SAFE_SENTINELS:
        require(sentinel.encode() not in data, f"safe secret sentinel found in {location}: {path}")
    for header in PRIVATE_HEADERS:
        require(header not in data, f"private-key material found in {location}: {path}")
    require(LINK_AGE_PRIVATE_IDENTITY.search(data) is None, f"age private identity found in {location}: {path}")
    require(
        AUTHORIZATION_CREDENTIAL_BYTES.search(data) is None
        and not standalone_bearer_credential_found(data, STANDALONE_BEARER_BYTES),
        f"bearer credential found in {location}: {path}",
    )
    for match in ASSIGNMENT_BYTES.finditer(data):
        key = match.group(1).decode("ascii", errors="ignore")
        if sensitive_assignment_key(key):
            raise CheckFailure(f"secret-like assignment found in {location}: {path}")


def normalize_tar_path(name):
    while name.startswith("./"):
        name = name[2:]
    return name.lstrip("/")


def check_link_target(member, path, location):
    if not (member.issym() or member.islnk()):
        return
    try:
        target = member.linkname.encode("utf-8", errors="surrogateescape")
    except UnicodeError as error:
        raise CheckFailure(f"cannot scan {location}: {path}") from error
    require(len(target) <= MAX_LINK_TARGET_SCAN_BYTES, f"{location} exceeds bounded scan limit: {path}")
    target_path = normalize_tar_path(member.linkname)
    require(
        PROHIBITED_PATH.search(target_path) is None,
        f"prohibited deployment artifact found in {location}: {path}",
    )
    check_link_contents(target, path, location)


def check_filesystem(archive_path, platform):
    require(platform in ALLOWED_IMAGE_PLATFORMS, "exported filesystem platform is not approved")
    scanned = 0
    try:
        archive = tarfile.open(archive_path)
    except (OSError, tarfile.TarError) as error:
        raise CheckFailure("cannot open exported image filesystem") from error
    with archive:
        for member in archive:
            path = normalize_tar_path(member.name)
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
            check_link_target(member, path, "exported filesystem link target")
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
            check_file_contents(data, path, platform)


def check_layers(image_save_path, platform):
    require(platform in ALLOWED_IMAGE_PLATFORMS, "saved image layers platform is not approved")
    scanned = 0
    try:
        image_save = tarfile.open(image_save_path)
    except (OSError, tarfile.TarError) as error:
        raise CheckFailure("cannot open saved image layers") from error
    with image_save:
        manifest_member = image_save.getmember("manifest.json")
        manifest_handle = image_save.extractfile(manifest_member)
        require(manifest_handle is not None, "saved image manifest is unreadable")
        try:
            manifest = json.load(manifest_handle)
        except (ValueError, TypeError) as error:
            raise CheckFailure("saved image manifest is malformed") from error
        require(isinstance(manifest, list) and len(manifest) == 1, "saved image manifest shape is not exact")
        layers = manifest[0].get("Layers") or []
        require(isinstance(layers, list) and layers, "saved image has no layers")
        for layer_name in layers:
            require(isinstance(layer_name, str), "saved image layer name is malformed")
            try:
                layer_member = image_save.getmember(layer_name)
            except KeyError as error:
                raise CheckFailure("saved image layer is unavailable") from error
            layer_handle = image_save.extractfile(layer_member)
            require(layer_handle is not None, "saved image layer is unreadable")
            try:
                layer = tarfile.open(fileobj=layer_handle, mode="r|*")
            except tarfile.TarError as error:
                raise CheckFailure("saved image layer archive is malformed") from error
            with layer:
                for member in layer:
                    path = normalize_tar_path(member.name)
                    require(PROHIBITED_PATH.search(path) is None, f"prohibited deployment artifact found in image layer: {path}")
                    check_link_target(member, path, "image layer link target")
                    if not member.isfile():
                        continue
                    require(member.size <= MAX_FILE_SCAN_BYTES, f"image-layer regular file exceeds bounded scan limit: {path}")
                    scanned += member.size
                    require(scanned <= MAX_TOTAL_SCAN_BYTES, "saved image layers exceed bounded content scan limit")
                    extracted = layer.extractfile(member)
                    require(extracted is not None, f"cannot read image-layer regular file: {path}")
                    data = extracted.read(MAX_FILE_SCAN_BYTES + 1)
                    require(len(data) == member.size, f"cannot completely scan image-layer regular file: {path}")
                    check_file_contents(data, path, platform, "image-layer regular file")


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


def canonical_bind_source(path):
    if path.startswith("/host_mnt/private/"):
        path = path[len("/host_mnt"):]
    elif path.startswith("/host_mnt/"):
        path = path[len("/host_mnt"):]
    return os.path.realpath(path)


def check_bind(inspect_path, expected_source):
    data = load_json(inspect_path)
    require(isinstance(data, list) and len(data) == 1, "runtime inspection shape is not exact")
    mounts = data[0].get("Mounts") or []
    matching = [mount for mount in mounts if mount.get("Destination") == "/data"]
    require(len(matching) == 1, "runtime data mount count is not exact")
    mount = matching[0]
    require(mount.get("Type") == "bind", "runtime data mount is not a bind mount")
    require(canonical_bind_source(mount.get("Source", "")) == os.path.realpath(expected_source), "runtime data bind source is not exact")
    require(mount.get("RW") is True, "runtime data bind is not writable")


RUNTIME_PATHS = {
    "config": (0o755, None),
    "secrets": (0o700, None),
    "config/age-recipient": (0o644, "file"),
    "config/known_hosts": (0o644, "file"),
    "secrets/upload-token": (0o600, "file"),
    "secrets/storage-ssh-key": (0o600, "file"),
    "secrets/borg-repository": (0o600, "file"),
}


def check_runtime_files(root, expected_uid, expected_gid):
    root = os.path.realpath(root)
    for relative, (expected_mode, kind) in RUNTIME_PATHS.items():
        path = os.path.join(root, relative)
        try:
            info = os.stat(path, follow_symlinks=False)
        except OSError as error:
            raise CheckFailure(f"required runtime fixture is unavailable: {relative}") from error
        if kind == "file":
            require(stat.S_ISREG(info.st_mode), f"runtime fixture is not a regular file: {relative}")
        else:
            require(stat.S_ISDIR(info.st_mode), f"runtime fixture is not a directory: {relative}")
        require(stat.S_IMODE(info.st_mode) == expected_mode, f"runtime fixture mode is not exact: {relative}")
        require(info.st_uid == expected_uid and info.st_gid == expected_gid, f"runtime fixture ownership is not exact: {relative}")


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
    elif command == "platform":
        require(len(argv) == 3, "platform check arguments are invalid")
        print(image_platform(argv[2]))
    elif command == "filesystem":
        require(len(argv) == 4, "filesystem check arguments are invalid")
        check_filesystem(argv[2], argv[3])
    elif command == "layers":
        require(len(argv) == 4, "layers check arguments are invalid")
        check_layers(argv[2], argv[3])
    elif command == "layout":
        require(len(argv) == 5, "layout check arguments are invalid")
        check_layout(argv[2], int(argv[3]), int(argv[4]))
    elif command == "bind":
        require(len(argv) == 4, "bind check arguments are invalid")
        check_bind(argv[2], argv[3])
    elif command == "runtime-files":
        require(len(argv) == 5, "runtime-files check arguments are invalid")
        check_runtime_files(argv[2], int(argv[3]), int(argv[4]))
    else:
        raise CheckFailure("unknown smoke check command")


if __name__ == "__main__":
    try:
        main(sys.argv)
    except (CheckFailure, OSError, subprocess.SubprocessError, ValueError) as error:
        print(f"collector smoke check failed: {error}", file=sys.stderr)
        sys.exit(1)
