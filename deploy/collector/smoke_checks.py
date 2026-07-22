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
    r"(?=(?<![A-Za-z0-9_.-])[\"']?([A-Za-z_][A-Za-z0-9_.-]{1,127})[\"']?\s*(?:=|:)\s*([^\s\x00]{1,4096}))"
)
BEARER_CREDENTIAL = re.compile(
    r"(?<![A-Za-z0-9_.-])(?:proxy[_.-]?)?authorization\s*(?:=|:)\s*(?:bearer|token)\s+[^\s\"']+",
    re.IGNORECASE,
)
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


def check_assignments(text, context):
    if BEARER_CREDENTIAL.search(text) is not None:
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
AGE_PRIVATE_IDENTITY = re.compile(rb"(?:^|[\x00\r\n])AGE-SECRET-KEY-1[0-9A-Z]+(?:$|[\x00\r\n])")
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
    rb"(?=(?<![A-Za-z0-9_.-])[\"']?([A-Za-z_][A-Za-z0-9_.-]{1,127})[\"']?\s*(?:=|:)\s*([^\s\x00]{1,4096}))"
)
BEARER_CREDENTIAL_BYTES = re.compile(
    rb"(?<![A-Za-z0-9_.-])(?:proxy[_.-]?)?authorization\s*(?:=|:)\s*(?:bearer|token)\s+[^\s\"']+",
    re.IGNORECASE,
)
MAX_FILE_SCAN_BYTES = 64 * 1024 * 1024
MAX_TOTAL_SCAN_BYTES = 1024 * 1024 * 1024

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


def nul_private_record_allowed(data, path, match):
    record_end = data.find(b"\x00", match.end() - 1)
    if record_end < 0:
        return False
    record = data[match.start():record_end + 1]
    digest = hashlib.sha256(record).hexdigest()
    normalized_path = normalize_nul_private_record_path(path)
    return digest in ALLOWED_NUL_PRIVATE_RECORD_SHA256.get(normalized_path, ())


def check_file_contents(data, path):
    for sentinel in SAFE_SENTINELS:
        require(sentinel.encode() not in data, f"safe secret sentinel found in exported regular file: {path}")
    for header in PRIVATE_HEADERS:
        line_pattern = rb"(?:^|[\r\n])" + re.escape(header) + rb"\r?\n"
        require(re.search(line_pattern, data) is None, f"private-key material found in exported regular file: {path}")
        nul_pattern = rb"\x00" + re.escape(header) + rb"\r?\n[A-Za-z0-9+/=]{7,}(?:$|[\x00\r\n])"
        for match in re.finditer(nul_pattern, data):
            require(
                nul_private_record_allowed(data, path, match),
                f"private-key material found in exported regular file: {path}",
            )
    require(AGE_PRIVATE_IDENTITY.search(data) is None, f"age private identity found in exported regular file: {path}")
    require(BEARER_CREDENTIAL_BYTES.search(data) is None, f"bearer credential found in exported regular file: {path}")
    if b"\x00" in data:
        return
    for match in ASSIGNMENT_BYTES.finditer(data):
        key = match.group(1).decode("ascii", errors="ignore")
        if sensitive_assignment_key(key) and not runtime_assignment_allowed(path, key):
            raise CheckFailure(f"secret-like assignment found in exported regular file: {path}")


def normalize_tar_path(name):
    while name.startswith("./"):
        name = name[2:]
    return name.lstrip("/")


def check_filesystem(archive_path):
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


def check_layers(image_save_path):
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
                    if not member.isfile():
                        continue
                    path = normalize_tar_path(member.name)
                    require(PROHIBITED_PATH.search(path) is None, f"prohibited deployment artifact found in image layer: {path}")
                    require(member.size <= MAX_FILE_SCAN_BYTES, f"image-layer regular file exceeds bounded scan limit: {path}")
                    scanned += member.size
                    require(scanned <= MAX_TOTAL_SCAN_BYTES, "saved image layers exceed bounded content scan limit")
                    extracted = layer.extractfile(member)
                    require(extracted is not None, f"cannot read image-layer regular file: {path}")
                    data = extracted.read(MAX_FILE_SCAN_BYTES + 1)
                    require(len(data) == member.size, f"cannot completely scan image-layer regular file: {path}")
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
    elif command == "filesystem":
        require(len(argv) == 3, "filesystem check arguments are invalid")
        check_filesystem(argv[2])
    elif command == "layers":
        require(len(argv) == 3, "layers check arguments are invalid")
        check_layers(argv[2])
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
