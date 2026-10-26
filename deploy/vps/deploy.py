#!/usr/bin/env python3
"""Blue-green deploy of a sub2api image onto the single production VPS.

The script discovers the live container, image, and revision. It pulls the
requested image, skips when that revision is already running, and otherwise
backs up Postgres, starts a candidate on the loopback candidate port, checks
health and migration logs, switches Nginx, drains the previous container, and
promotes the candidate into the compose service. A failure before the new
container is serving restores the compose file, Nginx routes, and container.
"""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import pathlib
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
from datetime import datetime, timezone


DEFAULT_IMAGE = "ghcr.io/uttogg/sub2api:latest"
DEFAULT_DEPLOY_DIR = "/opt/sub2api-deploy"
DEFAULT_SOURCE = "https://github.com/uttogg/sub2api"
CANONICAL_PORT = 8080
CANDIDATE_PORT = 18081
APP_PORT = "8080"
POSTGRES_PORT = "5432"
REDIS_PORT = "6379"
REVISION_LABEL = "org.opencontainers.image.revision"
VERSION_LABEL = "org.opencontainers.image.version"
SOURCE_LABEL = "org.opencontainers.image.source"
IGNORED_TCP_STATES = {"LISTEN", "TIME-WAIT", "CLOSED"}
MIGRATION_ERROR_PATTERNS = (
    re.compile(r"auto setup failed", re.IGNORECASE),
    re.compile(r"setup failed:", re.IGNORECASE),
    re.compile(r"apply migration\b", re.IGNORECASE),
    re.compile(r"checksum mismatch", re.IGNORECASE),
    re.compile(r"\brecord migration\b", re.IGNORECASE),
    re.compile(r"\bbegin migration\b", re.IGNORECASE),
    re.compile(r"\bcommit migration\b", re.IGNORECASE),
    re.compile(r"create schema_migrations", re.IGNORECASE),
    re.compile(r"check migration .+:", re.IGNORECASE),
    re.compile(r"validate migration\b", re.IGNORECASE),
    re.compile(r"prepare migration\b", re.IGNORECASE),
)
VERSION_TEXT = re.compile(r"Sub2API\s+(\S+)\s+\(commit:\s+([^,\s]+)")
PROXY_PASS = re.compile(r"proxy_pass\s+http://127\.0\.0\.1:(?P<port>\d+)\s*;")
RELEASE_NAME = re.compile(r"^release-\d{8}T\d{6}Z(?:-.+)?$")
SAFE_TOKEN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:@+/.-]*$")
PROBES = (
    ("/health", 200, "status", "ok"),
    ("/v1/models", 401, "code", "API_KEY_REQUIRED"),
)


class DeployError(RuntimeError):
    """A deploy step failed. The message is safe to print."""


def parse_duration(value):
    match = re.fullmatch(r"(\d+)([smh]?)", str(value).strip())
    if not match:
        raise argparse.ArgumentTypeError(
            "expected seconds or a value like 30m or 2h"
        )
    amount = int(match.group(1))
    factor = {"": 1, "s": 1, "m": 60, "h": 3600}[match.group(2)]
    seconds = amount * factor
    if seconds <= 0:
        raise argparse.ArgumentTypeError("duration must be positive")
    return seconds


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description=(
            "Pull a sub2api image and blue-green deploy it on the production VPS."
        ),
        epilog=(
            "Example: python3 /opt/sub2api-deploy/deploy.py "
            "--image ghcr.io/uttogg/sub2api:latest"
        ),
    )
    parser.add_argument("--image", default=DEFAULT_IMAGE, help="image ref to deploy")
    parser.add_argument("--deploy-dir", default=DEFAULT_DEPLOY_DIR)
    parser.add_argument("--compose-file", default="")
    parser.add_argument("--service", default="sub2api")
    parser.add_argument("--container", default="sub2api")
    parser.add_argument("--postgres-container", default="sub2api-postgres")
    parser.add_argument("--redis-container", default="sub2api-redis")
    parser.add_argument("--project-name", default="")
    parser.add_argument("--nginx", default="/usr/local/nginx/sbin/nginx")
    parser.add_argument(
        "--vhost",
        action="append",
        default=None,
        help=(
            "Nginx site config. Repeat for each site. Append =optional to warn "
            "instead of failing when the file or probes are unavailable. "
            "Defaults: sub.520ikun.xyz (required) and sub.050213.xyz (optional)."
        ),
    )
    parser.add_argument("--canonical-port", type=int, default=CANONICAL_PORT)
    parser.add_argument("--candidate-port", type=int, default=CANDIDATE_PORT)
    parser.add_argument("--bind-host", default="127.0.0.1")
    parser.add_argument("--health-timeout", type=parse_duration, default=180)
    parser.add_argument(
        "--drain-timeout",
        type=parse_duration,
        default=1800,
        help="how long to wait for in-flight calls before proceeding (default 30m)",
    )
    parser.add_argument("--keep-releases", type=int, default=3)
    parser.add_argument("--state-file", default="")
    parser.add_argument("--expected-source", default=DEFAULT_SOURCE)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print the deploy plan and do not change the system",
    )
    args = parser.parse_args(argv)
    if args.canonical_port == args.candidate_port:
        parser.error("canonical and candidate ports must differ")
    if args.keep_releases < 1:
        parser.error("--keep-releases must be at least 1")
    if not SAFE_TOKEN.fullmatch(args.image) or ".." in args.image.split("/"):
        parser.error("refusing unsafe image reference")
    for name in (
        args.service,
        args.container,
        args.postgres_container,
        args.redis_container,
        args.project_name,
        args.bind_host,
    ):
        if name and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", name):
            parser.error(f"refusing unsafe name: {name}")
    deploy_dir = pathlib.Path(args.deploy_dir)
    args.deploy_dir = str(deploy_dir)
    args.compose_file = str(
        pathlib.Path(args.compose_file)
        if args.compose_file
        else deploy_dir / "docker-compose.yml"
    )
    args.env_file = str(deploy_dir / ".env")
    args.releases_dir = str(deploy_dir / "releases")
    args.lock_file = str(deploy_dir / ".deployment.lock")
    if not args.state_file:
        args.state_file = str(deploy_dir / "deploy-state.json")
    try:
        args.vhosts = parse_vhosts(args.vhost)
    except DeployError as error:
        parser.error(str(error))
    return args


def parse_vhosts(values):
    if not values:
        values = [
            "/usr/local/nginx/vhost/sub.520ikun.xyz.conf=required",
            "/usr/local/nginx/vhost/sub.050213.xyz.conf=optional",
        ]
    vhosts = []
    for value in values:
        optional = False
        path_text = value
        if value.endswith("=optional"):
            optional = True
            path_text = value[: -len("=optional")]
        elif value.endswith("=required"):
            path_text = value[: -len("=required")]
        path = pathlib.Path(path_text)
        domain = path.name[:-5] if path.name.endswith(".conf") else path.name
        if not domain or "/" in domain:
            raise DeployError(f"Cannot derive a domain from vhost path: {path}")
        vhosts.append({"path": path, "domain": domain, "optional": optional})
    if not any(not item["optional"] for item in vhosts):
        raise DeployError("At least one Nginx vhost must be required")
    return vhosts


def image_repository(ref):
    value = ref.strip()
    if "@" in value:
        value = value.split("@", 1)[0]
    last = value.rsplit("/", 1)[-1]
    if ":" in last:
        value = value[: value.rfind(":")]
    return value


def normalize_id(value):
    text = (value or "").strip()
    if text.startswith("sha256:"):
        return text
    if re.fullmatch(r"[0-9a-f]{12,64}", text):
        return "sha256:" + text
    return text


def image_labels(info):
    return ((info or {}).get("Config") or {}).get("Labels") or {}


def label_value(info, key):
    value = image_labels(info).get(key) or ""
    if value == "<no value>":
        return ""
    return str(value).strip()


def same_revision(running_container, pulled_image):
    running_revision = label_value(running_container, REVISION_LABEL)
    pulled_revision = label_value(pulled_image, REVISION_LABEL)
    if running_revision and pulled_revision and running_revision == pulled_revision:
        return True
    running_id = normalize_id(running_container.get("Image"))
    pulled_id = normalize_id(pulled_image.get("Id"))
    return bool(running_id) and running_id == pulled_id


def parse_version_text(text):
    match = VERSION_TEXT.search(text or "")
    if not match:
        return "", ""
    return match.group(1), match.group(2)


def migration_error_lines(logs):
    found = []
    for line in (logs or "").splitlines():
        if any(pattern.search(line) for pattern in MIGRATION_ERROR_PATTERNS):
            found.append(line)
    return found


def runtime_contract_errors(info):
    config = (info or {}).get("Config") or {}
    health = config.get("Healthcheck") or {}
    errors = []
    if info.get("Os") != "linux" or info.get("Architecture") != "amd64":
        errors.append("image is not linux/amd64")
    if config.get("Entrypoint") != ["/app/docker-entrypoint.sh"]:
        errors.append("unexpected entrypoint")
    if config.get("Cmd") != ["/app/sub2api"]:
        errors.append("unexpected command")
    if config.get("WorkingDir") != "/app":
        errors.append("unexpected workdir")
    if not isinstance(health, dict) or not health.get("Test"):
        errors.append("missing healthcheck")
    return errors


def replace_service_image(text, service, new_image):
    lines = text.splitlines(keepends=True)
    services_at = None
    services_indent = None
    for index, raw in enumerate(lines):
        stripped = raw.split("#", 1)[0].strip()
        if stripped == "services:":
            services_at = index
            services_indent = len(raw) - len(raw.lstrip(" "))
            break
    if services_at is None:
        raise DeployError("Compose file has no services block")
    start = None
    service_indent = None
    end = len(lines)
    for index in range(services_at + 1, len(lines)):
        raw = lines[index]
        content = raw.split("#", 1)[0].rstrip()
        if not content.strip():
            continue
        indent = len(content) - len(content.lstrip(" "))
        name = content.strip().split(":", 1)[0]
        if indent <= services_indent:
            if start is None:
                continue
            end = index
            break
        if start is None and name == service and content.strip().endswith(":"):
            start = index
            service_indent = indent
            continue
        if start is not None and indent <= service_indent:
            end = index
            break
    if start is None:
        raise DeployError(f"Compose service not found: {service}")
    image_line = None
    image_re = re.compile(r"^(\s*)image:\s*(['\"]?)(.+?)\2\s*$")
    for index in range(start + 1, end):
        match = image_re.match(lines[index].rstrip("\r\n"))
        if not match:
            continue
        if image_line is not None:
            raise DeployError(f"Service {service} declares more than one image")
        image_line = (index, match)
    if image_line is None:
        raise DeployError(f"Service {service} has no image line")
    index, match = image_line
    indent, old_image = match.group(1), match.group(3).strip()
    newline = "\n"
    if lines[index].endswith("\r\n"):
        newline = "\r\n"
    elif not lines[index].endswith("\n"):
        newline = ""
    lines[index] = f'{indent}image: "{new_image}"{newline}'
    return "".join(lines), old_image


def rewrite_proxy_pass(text, canonical_port, candidate_port, target_port):
    accepted = {int(canonical_port), int(candidate_port)}
    matches = [
        match
        for match in PROXY_PASS.finditer(text)
        if int(match.group("port")) in accepted
    ]
    if len(matches) != 1:
        raise DeployError(
            "expected exactly one loopback proxy_pass for "
            f"{canonical_port}/{candidate_port}, found {len(matches)}"
        )
    match = matches[0]
    replacement = f"proxy_pass http://127.0.0.1:{int(target_port)};"
    updated = text[: match.start()] + replacement + text[match.end() :]
    if updated.count(replacement) != 1:
        raise DeployError("proxy_pass rewrite did not land on a single route")
    return updated


def split_endpoint(value):
    if value.startswith("["):
        host, _, port = value[1:].partition("]:")
        return host, port
    host, _, port = value.rpartition(":")
    return host, port


def connection_counts(listing, excluded_peers, app_port=APP_PORT):
    """Classify container connections.

    Returns inbound connections to the app port, other upstream connections,
    and a detail list. PostgreSQL, Redis, and Docker DNS are excluded.
    Loopback traffic is counted: it can be a local proxy rather than a probe.
    """
    inbound = 0
    upstream = 0
    details = []
    for line in (listing or "").splitlines():
        fields = line.split()
        if len(fields) < 5:
            continue
        state, local, peer = fields[0], fields[3], fields[4]
        if state in IGNORED_TCP_STATES:
            continue
        _local_host, local_port = split_endpoint(local)
        peer_host, peer_port = split_endpoint(peer)
        record = {"state": state, "local": local, "peer": peer}
        if local_port == str(app_port):
            inbound += 1
            record["classified"] = "inbound"
            details.append(record)
            continue
        if peer_host == "127.0.0.11" and peer_port == "53" and state in {
            "ESTAB",
            "SYN-SENT",
        }:
            record["classified"] = "docker_dns"
            details.append(record)
            continue
        if f"{peer_host}:{peer_port}" in excluded_peers:
            record["classified"] = "infrastructure"
            details.append(record)
            continue
        upstream += 1
        record["classified"] = "upstream"
        details.append(record)
    return inbound, upstream, details


def releases_to_delete(entries, keep, current_name):
    """entries are (name, sort_key). Keep the newest `keep` release directories."""
    ordered = [
        name
        for name, _sort_key in sorted(entries, key=lambda item: item[1], reverse=True)
        if RELEASE_NAME.fullmatch(name)
    ]
    deleted = ordered[keep:]
    return [name for name in deleted if name != current_name]


def images_to_remove(rows, keep_ids, repositories):
    """rows are (image_id, repository, tag). Keep every tag of the kept ids."""
    kept = {normalize_id(value) for value in keep_ids if value}
    remove = []
    for image_id, repository, tag in rows:
        if repository not in repositories:
            continue
        if normalize_id(image_id) in kept:
            continue
        remove.append((normalize_id(image_id), repository, tag))
    return remove


def decide_rollback(flags):
    if flags.get("canonical_serving"):
        return "keep_canonical"
    if flags.get("compose_updated") or flags.get("canonical_replaced"):
        return "restore_old"
    if flags.get("nginx_on_candidate"):
        if flags.get("old_healthy"):
            return "revert_nginx"
        return "retain_candidate"
    if flags.get("candidate_started"):
        return "remove_candidate"
    return "none"


def render_plan(args, discovered=None):
    discovered = discovered or {}
    lines = [
        "dry-run: no changes will be made",
        f"image: {args.image}",
        f"deploy_dir: {args.deploy_dir}",
        f"compose_file: {args.compose_file}",
        f"service: {args.service}",
        f"container: {args.container}",
        f"postgres_container: {args.postgres_container}",
        f"redis_container: {args.redis_container}",
        f"nginx: {args.nginx}",
        f"canonical_port: {args.canonical_port}",
        f"candidate_port: {args.candidate_port}",
        f"drain_timeout_seconds: {args.drain_timeout}",
        f"health_timeout_seconds: {args.health_timeout}",
        f"keep_releases: {args.keep_releases}",
        f"state_file: {args.state_file}",
        f"expected_source: {args.expected_source}",
    ]
    for vhost in args.vhosts:
        role = "optional" if vhost["optional"] else "required"
        lines.append(f"vhost: {vhost['path']} domain={vhost['domain']} {role}")
    if discovered:
        lines.append("discovered: " + json.dumps(discovered, ensure_ascii=False, sort_keys=True))
    lines.extend(
        [
            "plan:",
            "1. Take an exclusive lock at the deploy directory.",
            "2. Discover the running container, image id, and revision.",
            "3. docker pull the requested image and read its OCI labels.",
            "4. If the running revision already matches, write state and exit 0.",
            "5. pg_dump the database into a new release directory and keep the last "
            f"{args.keep_releases} releases.",
            "6. Start a candidate with a compose override of the live service on "
            f"{args.bind_host}:{args.candidate_port}.",
            "7. Wait until the candidate is healthy and its logs contain no migration errors.",
            "8. Switch Nginx proxy_pass to the candidate, run nginx -t, reload, and probe "
            "/health (200) and /v1/models (401).",
            "9. Drain the old container. When the drain timeout is reached, proceed.",
            "10. Update the compose image line and run "
            f"docker compose up -d --no-deps {args.service}.",
            "11. Switch Nginx back to "
            f"{args.bind_host}:{args.canonical_port} and probe again.",
            "12. Drain and remove the candidate.",
            "13. Prune old sub2api images, keeping the current image and the previous one.",
            "14. Write the JSON state file and a final status line.",
            "An optional vhost that is missing or does not respond is a warning.",
            "Failure before the new container is serving restores compose, Nginx, and the previous container.",
        ]
    )
    return "\n".join(lines) + "\n"


def emit(event, **fields):
    print(json.dumps({"event": event, **fields}, ensure_ascii=False), flush=True)


def run_command(command, timeout=None, stdin=None):
    try:
        result = subprocess.run(
            command,
            input=stdin,
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
        )
    except FileNotFoundError as error:
        raise DeployError(f"Command not found: {command[0]}") from error
    except subprocess.TimeoutExpired as error:
        raise DeployError(f"Timed out: {' '.join(command)}") from error
    if result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip()
        if len(detail) > 4000:
            detail = detail[-4000:]
        raise DeployError(
            f"Command failed ({result.returncode}): {' '.join(command)}\n{detail}"
        )
    return result


def run_to_file(command, path):
    with path.open("wb") as stream:
        try:
            result = subprocess.run(
                command,
                stdout=stream,
                stderr=subprocess.PIPE,
                check=False,
            )
        except FileNotFoundError as error:
            raise DeployError(f"Command not found: {command[0]}") from error
    if result.returncode != 0:
        detail = (result.stderr or b"").decode("utf-8", "replace")[-4000:]
        raise DeployError(
            f"Command failed ({result.returncode}): {' '.join(command)}\n{detail}"
        )


def docker_inspect(name, timeout=30):
    result = run_command(["docker", "inspect", name], timeout=timeout)
    payload = json.loads(result.stdout)
    if not isinstance(payload, list) or len(payload) != 1:
        raise DeployError(f"Unexpected docker inspect result for {name}")
    return payload[0]


def command_text(command, timeout=60):
    result = run_command(command, timeout=timeout)
    return (result.stdout or "") + (result.stderr or "")


def atomic_write(path, content):
    path = pathlib.Path(path)
    temporary = path.with_name(path.name + ".deploy.tmp")
    if temporary.exists():
        raise DeployError(f"Unfinished previous change: {temporary}")
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o644
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    descriptor = os.open(temporary, flags, mode)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    except Exception:
        if temporary.exists():
            os.unlink(temporary)
        raise


def sha256_file(path):
    digest = hashlib.sha256()
    with pathlib.Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def now_stamp():
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def iso_now():
    return datetime.now(timezone.utc).isoformat()


def container_ips(info):
    networks = ((info.get("NetworkSettings") or {}).get("Networks") or {})
    addresses = []
    for network in networks.values():
        address = (network or {}).get("IPAddress") or ""
        if re.fullmatch(r"\d+\.\d+\.\d+\.\d+", address):
            addresses.append(address)
    return addresses


def env_value(info, key, default):
    for item in ((info.get("Config") or {}).get("Env") or []):
        name, separator, value = item.partition("=")
        if separator and name == key and value:
            return value
    return default


def health_ok(port):
    try:
        with urllib.request.urlopen(
            f"http://127.0.0.1:{int(port)}/health", timeout=5
        ) as response:
            body = response.read(4096)
            if response.status != 200:
                return False
            payload = json.loads(body.decode("utf-8"))
            return isinstance(payload, dict) and payload.get("status") == "ok"
    except Exception:
        return False


def container_status(info):
    state = info.get("State") or {}
    health = (state.get("Health") or {}).get("Status") or ""
    return f"{state.get('Status') or ''}|{health}"


def require_tools(args):
    for tool in ("docker", "curl", "nsenter", "ss"):
        if shutil.which(tool) is None:
            raise DeployError(f"Required command is not installed: {tool}")
    if not pathlib.Path(args.nginx).is_file():
        raise DeployError(f"Nginx binary not found: {args.nginx}")


def discover_running(args):
    info = docker_inspect(args.container)
    labels = info.get("Config", {}).get("Labels") or {}
    project = args.project_name or labels.get("com.docker.compose.project") or ""
    service = labels.get("com.docker.compose.service") or ""
    if service and service != args.service:
        raise DeployError(
            f"Container {args.container} belongs to compose service {service}, "
            f"not {args.service}"
        )
    version_text = command_text(
        ["docker", "exec", args.container, "/app/sub2api", "--version"],
        timeout=30,
    )
    version, commit = parse_version_text(version_text)
    if not version or not commit:
        raise DeployError(f"Cannot parse running version: {version_text.strip()}")
    return {
        "info": info,
        "id": info.get("Id") or "",
        "image": (info.get("Config") or {}).get("Image") or "",
        "image_id": info.get("Image") or "",
        "revision": label_value(info, REVISION_LABEL) or commit,
        "version": label_value(info, VERSION_LABEL) or version,
        "version_text": version_text.strip(),
        "project": project,
        "status": container_status(info),
    }


def compose_config(args, project):
    command = ["docker", "compose", "--project-directory", args.deploy_dir]
    if project:
        command.extend(["--project-name", project])
    env_file = pathlib.Path(args.env_file)
    if env_file.is_file():
        command.extend(["--env-file", str(env_file)])
    command.extend(["--file", args.compose_file, "config", "--format", "json"])
    result = run_command(command, timeout=60)
    text = result.stdout
    start = text.find("{")
    if start < 0:
        raise DeployError("docker compose config did not return JSON")
    return json.loads(text[start:])


def service_networks(config, service):
    service_def = (config.get("services") or {}).get(service) or {}
    attached = service_def.get("networks") or {}
    keys = list(attached) if isinstance(attached, list) else list(attached.keys())
    declared = config.get("networks") or {}
    networks = []
    for key in keys:
        meta = declared.get(key) or {}
        name = meta.get("name") or key
        if not SAFE_TOKEN.fullmatch(key) or not SAFE_TOKEN.fullmatch(name):
            raise DeployError(f"Unsafe compose network name: {key} -> {name}")
        networks.append((key, name))
    if not networks:
        raise DeployError(f"Service {service} has no networks in compose config")
    return networks


def resolved_service_image(config, service):
    service_def = (config.get("services") or {}).get(service) or {}
    image = service_def.get("image") or ""
    if not image:
        raise DeployError(f"Compose config has no image for {service}")
    return image


def select_vhosts(args):
    selected = []
    warnings = []
    for vhost in args.vhosts:
        path = vhost["path"]
        if not path.is_file():
            message = f"Nginx site is missing: {path}"
            if vhost["optional"]:
                warnings.append(message)
                emit("vhost_skipped", path=str(path), reason="missing")
                continue
            raise DeployError(message)
        text = path.read_text(encoding="utf-8")
        try:
            rewrite_proxy_pass(
                text, args.canonical_port, args.candidate_port, args.candidate_port
            )
        except DeployError as error:
            if vhost["optional"]:
                warnings.append(f"{path}: {error}")
                emit("vhost_skipped", path=str(path), reason=str(error))
                continue
            raise
        selected.append({**vhost, "text": text})
    if not selected:
        raise DeployError("No Nginx vhosts can be switched")
    if not any(not item["optional"] for item in selected):
        raise DeployError("The required Nginx vhost could not be selected")
    return selected, warnings


def port_is_free(host, port):
    try:
        with socket.socket() as probe:
            probe.bind((host, int(port)))
    except OSError as error:
        raise DeployError(f"Candidate port is in use: {host}:{port}") from error


def candidate_absent(name):
    result = subprocess.run(
        ["docker", "inspect", name],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if result.returncode == 0:
        raise DeployError(f"Candidate container already exists: {name}")


def nginx_preflight(args):
    run_command([args.nginx, "-t"], timeout=30)
    dump = command_text([args.nginx, "-T"], timeout=30)
    if re.search(r"^\s*worker_shutdown_timeout\s+", dump, re.MULTILINE):
        raise DeployError("Nginx has a finite worker_shutdown_timeout")


def pull_image(image):
    run_command(["docker", "pull", image], timeout=1800)
    return docker_inspect(image)


def validate_pulled(info, expected_source):
    errors = runtime_contract_errors(info)
    revision = label_value(info, REVISION_LABEL)
    version = label_value(info, VERSION_LABEL)
    source = label_value(info, SOURCE_LABEL)
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        errors.append("image is missing a full git revision label")
    if not version:
        errors.append("image is missing a version label")
    if source != expected_source:
        errors.append(f"image source label is {source or 'empty'}")
    if errors:
        raise DeployError("Refusing image: " + "; ".join(errors))
    return revision, version


def write_state(path, payload):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    atomic_write(path, json.dumps(payload, indent=2, sort_keys=True) + "\n")


def backup_database(args, directory, postgres_info):
    user = env_value(postgres_info, "POSTGRES_USER", "sub2api")
    database = env_value(postgres_info, "POSTGRES_DB", "sub2api")
    if not SAFE_TOKEN.fullmatch(user) or not SAFE_TOKEN.fullmatch(database):
        raise DeployError("Refusing unusual Postgres user or database name")
    before = migration_snapshot(args, user, database)
    dump = directory / "database.before.dump"
    run_to_file(
        [
            "docker",
            "exec",
            args.postgres_container,
            "pg_dump",
            "-U",
            user,
            "-d",
            database,
            "--format=custom",
        ],
        dump,
    )
    os.chmod(dump, 0o600)
    if dump.stat().st_size == 0:
        raise DeployError("Database backup is empty")
    with dump.open("rb") as stream:
        result = subprocess.run(
            ["docker", "exec", "-i", args.postgres_container, "pg_restore", "--list"],
            stdin=stream,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            check=False,
        )
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", "replace")[-2000:]
        raise DeployError(f"Backup failed pg_restore --list\n{detail}")
    with (directory / "schema_migrations.before.csv").open("wb") as stream:
        result = subprocess.run(
            [
                "docker",
                "exec",
                args.postgres_container,
                "psql",
                "-U",
                user,
                "-d",
                database,
                "-v",
                "ON_ERROR_STOP=1",
                "-c",
                "COPY (SELECT filename, checksum FROM schema_migrations "
                "ORDER BY filename) TO STDOUT WITH CSV HEADER",
            ],
            stdout=stream,
            stderr=subprocess.PIPE,
            check=False,
        )
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", "replace")[-2000:]
        raise DeployError(f"Could not export schema_migrations\n{detail}")
    before.update(
        user=user,
        database=database,
        backup_file=dump.name,
        backup_bytes=dump.stat().st_size,
        backup_sha256=sha256_file(dump),
    )
    atomic_write(
        directory / "database.before.json",
        json.dumps(before, indent=2, sort_keys=True) + "\n",
    )
    return before


def migration_snapshot(args, user, database):
    query = (
        "SELECT json_build_object("
        "'count', (SELECT count(*) FROM schema_migrations), "
        "'latest', (SELECT filename FROM schema_migrations "
        "ORDER BY filename DESC LIMIT 1))"
    )
    result = run_command(
        [
            "docker",
            "exec",
            args.postgres_container,
            "psql",
            "-U",
            user,
            "-d",
            database,
            "-v",
            "ON_ERROR_STOP=1",
            "-tAc",
            query,
        ],
        timeout=60,
    )
    payload = json.loads(result.stdout.strip())
    if not isinstance(payload.get("count"), int):
        raise DeployError("schema_migrations count was not numeric")
    return payload


def rotate_releases(releases_dir, keep, current_name):
    releases = pathlib.Path(releases_dir)
    if not releases.is_dir():
        return []
    entries = []
    for path in releases.iterdir():
        if not path.is_dir() or not RELEASE_NAME.fullmatch(path.name):
            continue
        entries.append((path.name, path.stat().st_mtime))
    removed = []
    for name in releases_to_delete(entries, keep, current_name):
        target = releases / name
        if not target.resolve().is_relative_to(releases.resolve()):
            raise DeployError(f"Refusing to delete outside releases: {target}")
        shutil.rmtree(target)
        removed.append(name)
    return removed


def render_candidate_compose(args, image, container_name, networks, mounts):
    volume_lines = ["    volumes: !override []"] if not mounts else ["    volumes: !override"]
    volume_defs = []
    for index, mount in enumerate(mounts):
        destination = mount.get("Destination") or ""
        mount_type = mount.get("Type") or ""
        if not destination.startswith("/") or ":" in destination:
            raise DeployError(f"Unsafe mount destination: {destination}")
        if mount_type == "volume":
            volume_name = mount.get("Name") or ""
            if not SAFE_TOKEN.fullmatch(volume_name):
                raise DeployError(f"Unsafe volume name: {volume_name}")
            key = f"live{index}"
            suffix = "" if mount.get("RW", True) else ":ro"
            volume_lines.append(f"      - {key}:{destination}{suffix}")
            if not volume_defs:
                volume_defs = ["volumes:"]
            volume_defs.extend(
                [
                    f"  {key}:",
                    "    external: true",
                    f"    name: {volume_name}",
                ]
            )
        elif mount_type == "bind":
            source = mount.get("Source") or ""
            if not source.startswith("/") or ":" in source:
                raise DeployError(f"Unsafe bind source: {source}")
            suffix = "" if mount.get("RW", True) else ":ro"
            volume_lines.append(f"      - {source}:{destination}{suffix}")
        else:
            raise DeployError(f"Unsupported mount type on the live container: {mount_type}")
    network_lines = ["networks:"]
    for key, name in networks:
        network_lines.extend(
            [
                f"  {key}:",
                "    external: true",
                f"    name: {name}",
            ]
        )
    image_line = json.dumps(image)
    body = [
        "services:",
        "  sub2api_candidate:",
        "    extends:",
        f"      file: {json.dumps(args.compose_file)}",
        f"      service: {args.service}",
        f"    image: {image_line}",
        f"    container_name: {container_name}",
        '    restart: "no"',
        "    depends_on: !reset {}",
        "    ports: !override",
        f'      - "{args.bind_host}:{args.candidate_port}:{APP_PORT}"',
        *volume_lines,
        *network_lines,
        *volume_defs,
        "",
    ]
    return "\n".join(body)


def compose_base(args, project, compose_file):
    command = ["docker", "compose", "--project-directory", args.deploy_dir]
    if project:
        command.extend(["--project-name", project])
    env_file = pathlib.Path(args.env_file)
    if env_file.is_file():
        command.extend(["--env-file", str(env_file)])
    command.extend(["--file", str(compose_file)])
    return command


def wait_healthy(args, name, port, image_id, version, commit):
    deadline = time.monotonic() + args.health_timeout
    last_status = ""
    while time.monotonic() < deadline:
        info = docker_inspect(name)
        last_status = container_status(info)
        if image_id and normalize_id(info.get("Image")) != normalize_id(image_id):
            raise DeployError(f"{name} is not running the expected image")
        if last_status == "running|healthy" and health_ok(port):
            version_text = command_text(
                ["docker", "exec", name, "/app/sub2api", "--version"],
                timeout=30,
            )
            if version and version not in version_text:
                raise DeployError(f"Unexpected version in {name}: {version_text.strip()}")
            if commit and commit not in version_text:
                raise DeployError(f"Unexpected commit in {name}: {version_text.strip()}")
            emit("healthy", container=name, port=port, version=version_text.strip())
            return version_text.strip()
        if last_status.startswith(("exited|", "dead|")):
            logs = container_logs(name)[-4000:]
            raise DeployError(f"Container exited before becoming healthy: {name}\n{logs}")
        time.sleep(3)
    raise DeployError(f"Health deadline exceeded for {name} ({last_status})")


def container_logs(name):
    result = subprocess.run(
        ["docker", "logs", name],
        text=True,
        capture_output=True,
        check=False,
    )
    return (result.stdout or "") + (result.stderr or "")


def check_migrations(args, before, candidate, user, database):
    errors = migration_error_lines(container_logs(candidate))
    if errors:
        preview = "\n".join(errors[:20])
        raise DeployError("Candidate logged migration errors:\n" + preview)
    after = migration_snapshot(args, user, database)
    if after["count"] < before["count"]:
        raise DeployError(
            "schema_migrations shrank after the candidate started: "
            f"{before['count']} -> {after['count']}"
        )
    return after


def probe_url(domain, path):
    command = [
        "curl",
        "--silent",
        "--show-error",
        "--max-time",
        "10",
        "--resolve",
        f"{domain}:443:127.0.0.1",
        "--write-out",
        "\n%{http_code}",
        f"https://{domain}{path}",
    ]
    try:
        result = subprocess.run(
            command,
            text=True,
            capture_output=True,
            timeout=20,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as error:
        raise DeployError(f"Probe failed for {domain}{path}: {error}") from error
    output = (result.stdout or "") + ("" if result.returncode == 0 else "")
    if result.returncode != 0 and not result.stdout:
        detail = (result.stderr or "").strip()
        raise DeployError(f"Probe failed for {domain}{path}: {detail or result.returncode}")
    if "\n" not in (result.stdout or ""):
        raise DeployError(f"Probe returned no status for {domain}{path}")
    body, code = result.stdout.rsplit("\n", 1)
    try:
        status = int(code.strip())
    except ValueError as error:
        raise DeployError(f"Probe status unreadable for {domain}{path}") from error
    return status, body


def evaluate_probe(domain, path, status, body):
    expected = next(item for item in PROBES if item[0] == path)
    _path, expected_status, field, marker = expected
    try:
        payload = json.loads(body)
    except json.JSONDecodeError:
        payload = None
    actual = payload.get(field) if isinstance(payload, dict) else None
    if status == expected_status and actual == marker:
        return None
    return (
        f"{domain}{path} expected HTTP {expected_status} {field}={marker}, "
        f"got HTTP {status} {field}={actual}"
    )


def switch_nginx(args, vhosts, port, flags):
    """Point the selected vhosts at `port`. Restore the originals if the switch fails."""
    written = []
    flags["nginx_on_candidate"] = port == args.candidate_port
    try:
        for vhost in vhosts:
            text = rewrite_proxy_pass(
                vhost["original"],
                args.canonical_port,
                args.candidate_port,
                port,
            )
            atomic_write(vhost["path"], text)
            written.append(vhost["path"])
        run_command([args.nginx, "-t"], timeout=30)
        run_command([args.nginx, "-s", "reload"], timeout=30)
        if not health_ok(port):
            raise DeployError(f"Backend on port {port} failed health during Nginx switch")
        warnings = []
        for vhost in vhosts:
            for path, _expected_status, _field, marker in PROBES:
                try:
                    status, body = probe_url(vhost["domain"], path)
                    problem = evaluate_probe(vhost["domain"], path, status, body)
                except DeployError as error:
                    problem = str(error)
                    status = 0
                if problem:
                    if vhost["optional"]:
                        warnings.append(problem)
                        emit(
                            "public_route_warning",
                            domain=vhost["domain"],
                            path=path,
                            detail=problem,
                        )
                        continue
                    raise DeployError(problem)
                emit(
                    "public_route_healthy",
                    domain=vhost["domain"],
                    path=path,
                    backend_port=port,
                    http_status=status,
                    marker=marker,
                )
        emit("nginx_switched", backend_port=port, warnings=warnings)
        return warnings
    except Exception:
        restored = True
        for vhost in vhosts:
            if vhost["path"] not in written:
                continue
            try:
                atomic_write(vhost["path"], vhost["original"])
            except DeployError:
                restored = False
        if restored:
            try:
                run_command([args.nginx, "-t"], timeout=30)
                run_command([args.nginx, "-s", "reload"], timeout=30)
                flags["nginx_on_candidate"] = False
                emit("nginx_routes_restored", failed_backend_port=port)
            except DeployError as restore_error:
                flags["nginx_on_candidate"] = port == args.candidate_port
                emit("nginx_restore_failed", error=str(restore_error))
        else:
            flags["nginx_on_candidate"] = True
            emit("nginx_restore_failed", error="could not rewrite one or more vhosts")
        raise


def excluded_peers(args):
    peers = set()
    for name, port in (
        (args.postgres_container, POSTGRES_PORT),
        (args.redis_container, REDIS_PORT),
    ):
        addresses = container_ips(docker_inspect(name))
        if not addresses:
            raise DeployError(f"Cannot resolve an address for {name}")
        peers.update(f"{address}:{port}" for address in addresses)
    return peers


def wait_drained(args, name, container_id):
    deadline = time.monotonic() + args.drain_timeout
    consecutive_empty = 0
    last_notice = 0.0
    peers = excluded_peers(args)
    emit("drain_exclusions", container=name, excluded_peers=sorted(peers))
    while True:
        info = docker_inspect(name)
        if info.get("Id") != container_id:
            raise DeployError(f"Container identity changed while draining: {name}")
        pid = str((info.get("State") or {}).get("Pid") or "")
        if not pid.isdigit() or int(pid) <= 0:
            emit("drain_skipped_stopped", container=name)
            return False
        listing = command_text(["nsenter", "-t", pid, "-n", "ss", "-Htan"], timeout=20)
        inbound, upstream, details = connection_counts(listing, peers)
        if inbound == 0 and upstream == 0:
            consecutive_empty += 1
        else:
            consecutive_empty = 0
        now = time.monotonic()
        if now - last_notice >= 15 or consecutive_empty == 3:
            emit(
                "drain_progress",
                container=name,
                inbound_connections=inbound,
                upstream_connections=upstream,
                consecutive_empty=consecutive_empty,
                connections=details,
            )
            last_notice = now
        if consecutive_empty >= 3:
            emit("drained", container=name, container_id=container_id)
            return True
        if time.monotonic() >= deadline:
            emit(
                "drain_timeout",
                container=name,
                inbound_connections=inbound,
                upstream_connections=upstream,
                timeout_seconds=args.drain_timeout,
            )
            return False
        time.sleep(5)


def remove_container(name, container_id):
    info = docker_inspect(name)
    if info.get("Id") != container_id:
        raise DeployError(f"Refusing to remove {name}; identity changed")
    run_command(["docker", "stop", "--time", "30", name], timeout=60)
    run_command(["docker", "rm", name], timeout=60)


def list_images():
    result = run_command(
        ["docker", "image", "ls", "--no-trunc", "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}"],
        timeout=60,
    )
    rows = []
    for line in result.stdout.splitlines():
        image_id, repository, tag = (line.split("\t") + ["", ""])[:3]
        rows.append((image_id, repository, tag))
    return rows


def prune_images(keep_ids, repositories):
    removed = []
    warnings = []
    seen = set()
    for image_id, repository, tag in images_to_remove(list_images(), keep_ids, repositories):
        if image_id in seen:
            continue
        seen.add(image_id)
        result = subprocess.run(
            ["docker", "image", "rm", image_id],
            text=True,
            capture_output=True,
            check=False,
        )
        if result.returncode == 0:
            removed.append(image_id)
        else:
            warnings.append(
                f"{repository}:{tag} {image_id}: {(result.stderr or '').strip()}"
            )
    return removed, warnings


def compose_supports_pull_policy():
    result = subprocess.run(
        ["docker", "compose", "up", "--help"],
        text=True,
        capture_output=True,
        check=False,
    )
    return "--pull" in (result.stdout or "")


def recreate_service(args, project, expected_image_id):
    command = compose_base(args, project, args.compose_file)
    pull = ["--pull", "never"] if compose_supports_pull_policy() else []
    run_command(
        command + ["up", "-d", "--no-deps", *pull, args.service],
        timeout=300,
    )
    current = normalize_id(docker_inspect(args.container).get("Image"))
    if expected_image_id and current != normalize_id(expected_image_id):
        run_command(
            command + ["up", "-d", "--no-deps", *pull, "--force-recreate", args.service],
            timeout=300,
        )


def capture_vhosts(vhosts, release_dir):
    captured = []
    for vhost in vhosts:
        original = vhost["text"]
        saved = release_dir / (vhost["path"].name + ".before")
        atomic_write(saved, original)
        captured.append({**vhost, "original": original})
    return captured


def try_discover(args):
    if shutil.which("docker") is None:
        return {"docker": "not installed"}
    try:
        info = docker_inspect(args.container, timeout=15)
    except Exception as error:
        return {"docker_error": str(error)}
    state = info.get("State") or {}
    return {
        "container": args.container,
        "running_image": (info.get("Config") or {}).get("Image") or "",
        "image_id": info.get("Image") or "",
        "revision": label_value(info, REVISION_LABEL),
        "status": state.get("Status") or "",
        "health": (state.get("Health") or {}).get("Status") or "",
    }


def deploy(args):
    deploy_dir = pathlib.Path(args.deploy_dir)
    if not deploy_dir.is_dir():
        raise DeployError(f"Deploy directory does not exist: {deploy_dir}")
    lock_path = pathlib.Path(args.lock_file)
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    lock = lock_path.open("a+", encoding="utf-8")
    try:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise DeployError("Another deployment holds the lock") from error
        lock.seek(0)
        lock.truncate()
        lock.write(f"{os.getpid()}\n")
        lock.flush()
        return deploy_locked(args)
    finally:
        try:
            fcntl.flock(lock, fcntl.LOCK_UN)
        finally:
            lock.close()


def deploy_locked(args):
    require_tools(args)
    running = discover_running(args)
    if running["status"] != "running|healthy" or not health_ok(args.canonical_port):
        raise DeployError(
            f"Live container is not healthy ({running['status']}); refusing to deploy"
        )
    compose_path = pathlib.Path(args.compose_file)
    original_compose = compose_path.read_text(encoding="utf-8")
    config = compose_config(args, running["project"])
    resolved_image = resolved_service_image(config, args.service)
    if resolved_image != running["image"]:
        raise DeployError(
            "Compose image does not match the running container: "
            f"{resolved_image} != {running['image']}"
        )
    _updated, compose_image = replace_service_image(
        original_compose, args.service, args.image
    )
    if compose_image not in {running["image"], resolved_image} and "${" not in compose_image:
        raise DeployError(
            "Compose image line does not match the running container: "
            f"{compose_image}"
        )
    nginx_preflight(args)
    vhosts, vhost_warnings = select_vhosts(args)
    candidate_name = "sub2api-candidate-" + now_stamp().lower()
    candidate_absent(candidate_name)
    port_is_free(args.bind_host, args.candidate_port)
    postgres_info = docker_inspect(args.postgres_container)
    networks = service_networks(config, args.service)
    mounts = (running["info"].get("Mounts") or [])

    emit("pulling", image=args.image)
    pulled = pull_image(args.image)
    revision, version = validate_pulled(pulled, args.expected_source)
    if same_revision(running["info"], pulled):
        state = {
            "status": "already_up_to_date",
            "image": args.image,
            "image_id": pulled.get("Id"),
            "revision": revision,
            "version": version,
            "running_container_id": running["id"],
            "running_image": running["image"],
            "completed": iso_now(),
            "vhost_warnings": vhost_warnings,
        }
        write_state(args.state_file, state)
        emit("already_up_to_date", revision=revision, image=args.image)
        print("already up to date", flush=True)
        print(
            f"STATUS already up to date revision={revision} image={args.image}",
            flush=True,
        )
        return 0

    release_name = f"release-{now_stamp()}-{revision[:12]}"
    release_dir = pathlib.Path(args.releases_dir) / release_name
    release_dir.mkdir(parents=True, exist_ok=False)
    os.chmod(release_dir, 0o700)
    flags = {
        "canonical_serving": False,
        "compose_updated": False,
        "canonical_replaced": False,
        "nginx_on_candidate": False,
        "old_healthy": True,
        "candidate_started": False,
    }
    state = {
        "status": "running",
        "started": iso_now(),
        "image": args.image,
        "image_id": pulled.get("Id"),
        "revision": revision,
        "version": version,
        "previous_image": running["image"],
        "previous_image_id": running["image_id"],
        "previous_revision": running["revision"],
        "previous_version": running["version"],
        "previous_container_id": running["id"],
        "release_dir": str(release_dir),
        "stage": "backup_database",
        "active_backend_port": args.canonical_port,
        "vhost_warnings": list(vhost_warnings),
        "drain_timeout_seconds": args.drain_timeout,
    }
    context = {
        "args": args,
        "running": running,
        "pulled": pulled,
        "original_compose": original_compose,
        "release_dir": release_dir,
        "candidate_name": candidate_name,
        "vhosts": [],
        "flags": flags,
        "state": state,
        "user": env_value(postgres_info, "POSTGRES_USER", "sub2api"),
        "database": env_value(postgres_info, "POSTGRES_DB", "sub2api"),
    }

    def save():
        write_state(release_dir / "deployment.state.json", state)
        write_state(args.state_file, state)

    save()
    try:
        atomic_write(release_dir / "docker-compose.yml.before", original_compose)
        state["database_before"] = backup_database(args, release_dir, postgres_info)
        state["rotated_releases"] = rotate_releases(
            args.releases_dir, args.keep_releases, release_name
        )
        context["vhosts"] = capture_vhosts(vhosts, release_dir)
        candidate_file = release_dir / "candidate.compose.yml"
        candidate_file.write_text(
            render_candidate_compose(
                args, args.image, candidate_name, networks, mounts
            ),
            encoding="utf-8",
        )
        candidate_project = "sub2api-green-" + revision[:8]
        candidate_command = compose_base(args, candidate_project, candidate_file)
        state["stage"] = "start_candidate"
        save()
        run_command(candidate_command + ["config", "--quiet"], timeout=60)
        pull = ["--pull", "never"] if compose_supports_pull_policy() else []
        run_command(
            candidate_command
            + ["up", "-d", "--no-deps", *pull, "sub2api_candidate"],
            timeout=300,
        )
        flags["candidate_started"] = True
        candidate_info = docker_inspect(candidate_name)
        state["candidate_container_id"] = candidate_info.get("Id")
        save()
        state["candidate_version"] = wait_healthy(
            args,
            candidate_name,
            args.candidate_port,
            pulled.get("Id"),
            version,
            revision,
        )
        state["database_after_candidate"] = check_migrations(
            args,
            state["database_before"],
            candidate_name,
            context["user"],
            context["database"],
        )
        state["stage"] = "switch_candidate"
        save()
        warnings = switch_nginx(args, context["vhosts"], args.candidate_port, flags)
        state["active_backend_port"] = args.candidate_port
        state["vhost_warnings"].extend(warnings)
        state["stage"] = "drain_old"
        save()
        state["old_drained"] = wait_drained(args, args.container, running["id"])
        state["stage"] = "replace_canonical"
        save()
        updated_compose, _old = replace_service_image(
            original_compose, args.service, args.image
        )
        atomic_write(compose_path, updated_compose)
        flags["compose_updated"] = True
        recreate_service(args, running["project"], pulled.get("Id"))
        flags["canonical_replaced"] = True
        state["canonical_version"] = wait_healthy(
            args,
            args.container,
            args.canonical_port,
            pulled.get("Id"),
            version,
            revision,
        )
        warnings = switch_nginx(args, context["vhosts"], args.canonical_port, flags)
        flags["canonical_serving"] = True
        state["active_backend_port"] = args.canonical_port
        state["vhost_warnings"].extend(warnings)
        state["stage"] = "drain_candidate"
        save()
        state["candidate_drained"] = wait_drained(
            args, candidate_name, state["candidate_container_id"]
        )
        remove_container(candidate_name, state["candidate_container_id"])
        flags["candidate_started"] = False
        repositories = {
            image_repository(args.image),
            image_repository(running["image"]),
            "sub2api",
        }
        removed, prune_warnings = prune_images(
            [pulled.get("Id"), running["image_id"]], repositories
        )
        state.update(
            status="deployed",
            stage="complete",
            completed=iso_now(),
            new_container_id=docker_inspect(args.container).get("Id"),
            removed_images=removed,
            prune_warnings=prune_warnings,
            candidate_removed=True,
            rollback_triggered=False,
        )
        save()
        emit("deployment_complete", revision=revision, version=version, image=args.image)
        print(
            f"STATUS deployed image={args.image} revision={revision} version={version}",
            flush=True,
        )
        return 0
    except Exception as error:
        state["error"] = str(error)
        state["failed_at"] = iso_now()
        if flags["canonical_serving"]:
            state["status"] = "deployed_cleanup_incomplete"
            save()
            emit("cleanup_incomplete", error=str(error))
            print(f"STATUS deployed cleanup incomplete error={error}", flush=True)
            return 1
        flags["old_healthy"] = container_still_healthy(args, running["id"])
        try:
            apply_rollback(context)
            state["status"] = "rolled_back"
            state["rollback_triggered"] = True
            save()
            emit("rolled_back", error=str(error), action=state.get("rollback"))
            retained = " candidate retained" if state.get("candidate_retained") else ""
            print(f"STATUS rolled back{retained} error={error}", flush=True)
        except Exception as rollback_error:
            state["status"] = "rollback_failed"
            state["rollback_error"] = str(rollback_error)
            save()
            emit(
                "rollback_failed",
                error=str(error),
                rollback_error=str(rollback_error),
            )
            print(
                f"STATUS rollback failed error={error} rollback_error={rollback_error}",
                flush=True,
            )
        return 1


def container_still_healthy(args, container_id):
    try:
        info = docker_inspect(args.container)
    except DeployError:
        return False
    return info.get("Id") == container_id and container_status(info) == "running|healthy" and health_ok(
        args.canonical_port
    )


def apply_rollback(context):
    args = context["args"]
    flags = context["flags"]
    state = context["state"]
    action = decide_rollback(flags)
    if action == "revert_nginx" and not container_still_healthy(
        args, context["running"]["id"]
    ):
        action = "retain_candidate"
    state["rollback_action"] = action
    if action == "keep_canonical":
        state["rollback"] = "canonical_retained"
        return
    if action == "retain_candidate":
        state["rollback"] = "candidate_retained_previous_backend_unhealthy"
        state["candidate_retained"] = True
        return
    if action == "restore_old":
        restore_previous_container(context)
        restore_nginx(context)
        flags["nginx_on_candidate"] = False
        remove_candidate_best_effort(context)
        state["rollback"] = "previous_release_restored"
        return
    if action == "revert_nginx":
        restore_nginx(context)
        flags["nginx_on_candidate"] = False
        remove_candidate_best_effort(context)
        state["rollback"] = "nginx_restored_candidate_removed"
        return
    if action == "remove_candidate":
        remove_candidate_best_effort(context)
        state["rollback"] = (
            "candidate_retained"
            if state.get("candidate_retained")
            else "candidate_removed"
        )
        return
    state["rollback"] = "no_changes_to_roll_back"


def restore_previous_container(context):
    args = context["args"]
    running = context["running"]
    atomic_write(args.compose_file, context["original_compose"])
    context["flags"]["compose_updated"] = False
    run_command(["docker", "tag", running["image_id"], running["image"]], timeout=60)
    recreate_service(args, running["project"], running["image_id"])
    context["flags"]["canonical_replaced"] = False
    wait_healthy(
        args,
        args.container,
        args.canonical_port,
        running["image_id"],
        running["version"],
        running["revision"],
    )


def restore_nginx(context):
    args = context["args"]
    for vhost in context["vhosts"]:
        atomic_write(vhost["path"], vhost["original"])
    run_command([args.nginx, "-t"], timeout=30)
    run_command([args.nginx, "-s", "reload"], timeout=30)
    if not health_ok(args.canonical_port):
        raise DeployError("Previous backend was not healthy after restoring Nginx")
    for vhost in context["vhosts"]:
        if vhost["optional"]:
            continue
        for path, _status, _field, _marker in PROBES:
            status, body = probe_url(vhost["domain"], path)
            problem = evaluate_probe(vhost["domain"], path, status, body)
            if problem:
                raise DeployError("Probe failed while restoring Nginx: " + problem)


def remove_candidate_best_effort(context):
    if not context["flags"].get("candidate_started"):
        return
    name = context["candidate_name"]
    container_id = context["state"].get("candidate_container_id")
    if not container_id:
        return
    try:
        wait_drained(context["args"], name, container_id)
        remove_container(name, container_id)
        context["flags"]["candidate_started"] = False
        context["state"]["candidate_removed"] = True
    except Exception as error:
        context["state"]["candidate_retained"] = True
        context["state"]["cleanup_error"] = str(error)


def main(argv=None):
    args = parse_args(argv)
    if args.dry_run:
        discovered = try_discover(args)
        sys.stdout.write(render_plan(args, discovered))
        return 0
    try:
        return deploy(args)
    except DeployError as error:
        emit("deployment_failed", error=str(error))
        print(f"STATUS failed error={error}", flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
