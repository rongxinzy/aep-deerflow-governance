#!/usr/bin/env python3
"""Safely plan/build/deploy a pinned local de-portal image overlay.

Only the digital-employees/de-portal Deployment may be changed. Cluster
commands are deliberately routed through `k3s`; this helper never reads Secret
values or persists a raw Kubernetes object.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
from typing import Any


TARGET_NAME = "de-portal"
TARGET_NAMESPACE = "digital-employees"
TARGET_CONTAINER = "portal"
EXPECTED_BRANCH = "feat/knowledge-phase-a"
IMAGE_PREFIX = "docker.io/zhiyuan-local/de-portal"
UPDATE_ANNOTATION = "local.zhiyuan.portal-update"
SAFE_RUN_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$")
REQUIRED_KNOWLEDGE_ENV = (
    "PORTAL_KNOWLEDGE_TENANT_ID",
    "PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID",
)
LOCAL_KUBECONFIG = "/etc/rancher/k3s/k3s.yaml"


class UpdateError(RuntimeError):
    pass


def run(args: list[str], *, input_text: str | None = None, env: dict[str, str] | None = None) -> str:
    result = subprocess.run(
        args,
        input=input_text,
        text=True,
        capture_output=True,
        check=False,
        env=env,
    )
    if result.returncode:
        # Never echo command output: kubectl errors can include object details.
        raise UpdateError(f"command failed ({result.returncode}): {args[0]}")
    return result.stdout


def kubectl(*args: str, input_text: str | None = None) -> str:
    isolated_env = os.environ.copy()
    isolated_env["KUBECONFIG"] = LOCAL_KUBECONFIG
    return run(["k3s", "kubectl", *args], input_text=input_text, env=isolated_env)


def git(repo: Path, *args: str) -> str:
    marker = repo / ".git"
    if marker.is_dir():
        git_dir = marker
    elif marker.is_file():
        pointer = marker.read_text(encoding="utf-8").strip()
        match = re.fullmatch(r"gitdir:\s*(.+)", pointer, flags=re.IGNORECASE)
        if not match:
            raise UpdateError("RepositoryPath has an invalid linked-worktree marker")
        raw = match.group(1).replace("\\", "/")
        drive_path = re.match(r"^([A-Za-z]):/(.*)$", raw)
        git_dir = Path(f"/mnt/{drive_path.group(1).lower()}/{drive_path.group(2)}") if drive_path else Path(raw)
    else:
        raise UpdateError("RepositoryPath has no Git metadata")
    return run([
        "git", "-c", f"safe.directory={repo.resolve()}",
        f"--git-dir={git_dir}", f"--work-tree={repo.resolve()}", *args,
    ]).strip()


def verify_source(repo: Path, expected_commit: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{40}", expected_commit):
        raise UpdateError("ExpectedCommit must be a full 40-character lowercase SHA")
    if not repo.is_dir():
        raise UpdateError("RepositoryPath is not a directory")
    branch = git(repo, "branch", "--show-current")
    head = git(repo, "rev-parse", "HEAD")
    dirty = git(repo, "status", "--porcelain", "--untracked-files=all")
    if branch != EXPECTED_BRANCH:
        raise UpdateError(f"source branch must be {EXPECTED_BRANCH}")
    if head != expected_commit:
        raise UpdateError("source HEAD does not match ExpectedCommit")
    if dirty:
        raise UpdateError("source worktree must be clean, including untracked files")
    return head


def _named_refs(ref: dict[str, Any] | None) -> list[dict[str, str]]:
    if not isinstance(ref, dict):
        return []
    result = []
    for key in ("secretRef", "configMapRef", "secretKeyRef", "configMapKeyRef"):
        value = ref.get(key)
        if isinstance(value, dict) and isinstance(value.get("name"), str):
            item = {"kind": key.removesuffix("Ref"), "name": value["name"]}
            if isinstance(value.get("key"), str):
                item["key"] = value["key"]
            result.append(item)
    return result


def sanitized_snapshot(deployment: dict[str, Any]) -> dict[str, Any]:
    """Keep rollback-critical values and safe references, never env literals."""
    metadata = deployment.get("metadata") or {}
    spec = deployment.get("spec") or {}
    template = (spec.get("template") or {}).get("spec") or {}
    pod_metadata = (spec.get("template") or {}).get("metadata") or {}
    containers = template.get("containers") or []
    if len(containers) != 1:
        raise UpdateError("target Deployment must have exactly one container")
    container = containers[0]
    if container.get("name") != TARGET_CONTAINER:
        raise UpdateError("target container must be named portal")
    env_names = sorted(
        item["name"]
        for item in (container.get("env") or [])
        if isinstance(item, dict) and isinstance(item.get("name"), str)
    )
    env_refs = []
    for item in container.get("env") or []:
        if isinstance(item, dict):
            env_refs.extend(_named_refs(item.get("valueFrom")))
    env_from = []
    for item in container.get("envFrom") or []:
        env_from.extend(_named_refs(item))
    volumes = []
    for volume in template.get("volumes") or []:
        entry = {"name": volume.get("name", "")}
        for key in ("persistentVolumeClaim", "secret", "configMap"):
            value = volume.get(key)
            if isinstance(value, dict):
                entry[key] = value.get("claimName", value.get("secretName", value.get("name", "")))
        volumes.append(entry)
    annotations = pod_metadata.get("annotations") or {}
    old_marker = annotations.get(UPDATE_ANNOTATION)
    if old_marker is not None and (not isinstance(old_marker, str) or not SAFE_RUN_ID.fullmatch(old_marker)):
        raise UpdateError("existing Portal update marker is not safe to preserve")
    node_selector = template.get("nodeSelector") or {}
    snapshot = {
        "name": metadata.get("name", ""),
        "namespace": metadata.get("namespace", ""),
        "uid": metadata.get("uid", ""),
        "resourceVersion": metadata.get("resourceVersion", ""),
        "replicas": spec.get("replicas", 1),
        "serviceAccountName": template.get("serviceAccountName", ""),
        "container": {
            "name": container.get("name", ""),
            "image": container.get("image", ""),
            "imagePullPolicy": container.get("imagePullPolicy"),
            "environmentNames": env_names,
            "environmentReferences": sorted(env_refs, key=lambda item: (item["kind"], item["name"])),
            "envFromReferences": sorted(env_from, key=lambda item: (item["kind"], item["name"])),
            "volumeMounts": [
                {"name": item.get("name", ""), "mountPath": item.get("mountPath", ""), "readOnly": bool(item.get("readOnly", False))}
                for item in (container.get("volumeMounts") or [])
            ],
        },
        "volumes": volumes,
        "templateAnnotations": {UPDATE_ANNOTATION: old_marker} if old_marker is not None else {},
        "nodeSelector": {
            key: node_selector[key]
            for key in ("kubernetes.io/os", "kubernetes.io/arch")
            if key in node_selector
        },
    }
    if not snapshot["uid"] or not snapshot["container"]["image"]:
        raise UpdateError("Deployment is missing UID or image; refusing to plan")
    return snapshot


def verify_linux_amd64_nodes() -> None:
    try:
        config = json.loads(kubectl("config", "view", "--minify", "-o", "json"))
        cluster = (config.get("clusters") or [])[0]["cluster"]
        server = cluster["server"]
        host = re.match(r"^https?://([^/:]+|\[[^]]+\])(?::\d+)?(?:/.*)?$", server)
        if not host or host.group(1).strip("[]").lower() not in {"127.0.0.1", "localhost", "::1"}:
            raise UpdateError("local k3s API server must use a loopback address")
    except (KeyError, IndexError, TypeError, json.JSONDecodeError) as exc:
        raise UpdateError("cannot verify the fixed local k3s context") from exc
    nodes = json.loads(kubectl("get", "nodes", "-o", "json")).get("items") or []
    if len(nodes) != 1:
        raise UpdateError("local k3s must have exactly one node for Never-policy image safety")
    node = nodes[0]
    labels = (node.get("metadata") or {}).get("labels") or {}
    conditions = (node.get("status") or {}).get("conditions") or []
    ready = any(item.get("type") == "Ready" and item.get("status") == "True" for item in conditions)
    if (node.get("spec") or {}).get("unschedulable", False) or not ready:
        raise UpdateError("the single local k3s node must be Ready and schedulable")
    if labels.get("kubernetes.io/os") != "linux" or labels.get("kubernetes.io/arch") != "amd64":
        raise UpdateError("the single local node must be linux/amd64 for this overlay")


def image_cache() -> set[str]:
    return set(run(["k3s", "ctr", "images", "list", "-q"]).splitlines())


def image_digest(reference: str) -> str:
    rows = run(["k3s", "ctr", "images", "list"]).splitlines()
    wanted = canonical_image_ref(reference)
    for row in rows[1:]:
        columns = row.split()
        if len(columns) >= 3 and canonical_image_ref(columns[0]) == wanted and re.fullmatch(r"sha256:[0-9a-f]{64}", columns[2]):
            return columns[2]
    raise UpdateError("cannot resolve the cached Portal image manifest digest")


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(path.parent, 0o700)
    temp = path.with_suffix(path.suffix + ".tmp")
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(json.dumps(value, indent=2, ensure_ascii=False) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temp, 0o600)
        os.replace(temp, path)
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except Exception:
        try:
            temp.unlink(missing_ok=True)
        except OSError:
            pass
        raise


def read_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(value, dict):
            raise UpdateError(f"update record must be a JSON object: {path.name}")
        return value
    except UpdateError:
        raise
    except (OSError, json.JSONDecodeError) as exc:
        raise UpdateError(f"cannot read update record: {path.name}") from exc


def validate_plan(plan: dict[str, Any], run_root: Path) -> None:
    run_id = run_root.name
    if not isinstance(plan, dict) or plan.get("schemaVersion") != 1:
        raise UpdateError("unsupported or invalid plan schema")
    if not SAFE_RUN_ID.fullmatch(run_id) or plan.get("runId") != run_id:
        raise UpdateError("plan run ID does not match RunRoot")
    if plan.get("branch") != EXPECTED_BRANCH:
        raise UpdateError("plan branch is not the fixed knowledge phase branch")
    commit = plan.get("sourceCommit")
    if not isinstance(commit, str) or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise UpdateError("plan source commit is invalid")
    if plan.get("target") != {"name": TARGET_NAME, "namespace": TARGET_NAMESPACE, "container": TARGET_CONTAINER}:
        raise UpdateError("plan target is not the fixed de-portal Deployment")
    expected_image = re.compile(rf"^{re.escape(IMAGE_PREFIX)}:phase-a-{commit[:12]}-\d{{8}}t\d{{6}}z$")
    if not isinstance(plan.get("targetImage"), str) or not expected_image.fullmatch(plan["targetImage"]):
        raise UpdateError("plan target image is outside the fixed Portal image namespace")
    before = plan.get("before")
    if not isinstance(before, dict) or before.get("name") != TARGET_NAME or before.get("namespace") != TARGET_NAMESPACE:
        raise UpdateError("plan before-snapshot identity is invalid")
    if not before.get("uid") or before.get("container", {}).get("name") != TARGET_CONTAINER:
        raise UpdateError("plan before-snapshot is incomplete")
    if plan.get("baseImage") != before.get("container", {}).get("image"):
        raise UpdateError("plan base image does not match its before-snapshot")
    if not isinstance(plan.get("baseImageDigest"), str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", plan["baseImageDigest"]):
        raise UpdateError("plan base image manifest digest is invalid")
    rollout_state = (plan.get("rollout") or {}).get("state")
    if rollout_state not in {"planned", "prepared", "deployed"}:
        raise UpdateError("plan rollout state is invalid")


def validate_journal(journal: dict[str, Any], plan: dict[str, Any], run_root: Path) -> None:
    validate_plan(plan, run_root)
    before = plan["before"]
    expected_marker = before.get("templateAnnotations", {}).get(UPDATE_ANNOTATION)
    if (
        journal.get("runId") != plan["runId"]
        or journal.get("uid") != before["uid"]
        or journal.get("oldImage") != before["container"]["image"]
        or journal.get("oldPolicy") != before["container"].get("imagePullPolicy")
        or journal.get("oldMarker") != expected_marker
        or journal.get("targetImage") != plan["targetImage"]
    ):
        raise UpdateError("deployment journal does not match the immutable plan identity")
    if journal.get("state") not in {"prepared", "patch_attempted", "patched", "deployed", "rolled_back", "already_rolled_back", "rollback_failed"}:
        raise UpdateError("deployment journal state is invalid")


def resolve_oci(archive: tarfile.TarFile, descriptor: dict[str, Any]) -> dict[str, Any]:
    algorithm, digest = descriptor["digest"].split(":", 1)
    raw = archive.extractfile(f"blobs/{algorithm}/{digest}")
    if raw is None:
        raise UpdateError("image archive is missing a referenced blob")
    data = raw.read()
    if hashlib.sha256(data).hexdigest() != digest:
        raise UpdateError("image archive blob checksum mismatch")
    parsed = json.loads(data)
    if "manifests" in parsed:
        matches = [
            item for item in parsed["manifests"]
            if (item.get("platform") or {}).get("os") == "linux"
            and (item.get("platform") or {}).get("architecture") == "amd64"
        ]
        if len(matches) != 1:
            raise UpdateError("cached image must have exactly one linux/amd64 manifest")
        return resolve_oci(archive, matches[0])
    return parsed


def verify_oci_platform(path: Path) -> str:
    try:
        with tarfile.open(path) as archive:
            index = json.load(archive.extractfile("index.json"))
            if len(index.get("manifests") or []) != 1:
                raise UpdateError("exported image must contain one manifest")
            manifest = resolve_oci(archive, index["manifests"][0])
            config_descriptor = manifest["config"]
            config = json.loads(archive.extractfile("blobs/sha256/" + config_descriptor["digest"].split(":", 1)[1]).read())
            if config.get("os") != "linux" or config.get("architecture") != "amd64":
                raise UpdateError("cached image platform is not linux/amd64")
            return index["manifests"][0]["digest"]
    except (OSError, KeyError, TypeError, tarfile.TarError, json.JSONDecodeError) as exc:
        raise UpdateError("cannot verify cached OCI image platform") from exc


def export_base(base: str, destination: Path, expected_digest: str | None = None) -> None:
    run(["k3s", "ctr", "images", "export", "--platform", "linux/amd64", "--skip-manifest-json", str(destination), base])
    exported_digest = verify_oci_platform(destination)
    if expected_digest and exported_digest != expected_digest:
        raise UpdateError("cached base image manifest changed since plan")


def canonical_image_ref(reference: str) -> str:
    name, separator, tag_or_digest = reference.partition("@")
    if not separator:
        slash = name.rfind("/")
        colon = name.rfind(":")
        if colon > slash:
            name, tag_or_digest = name[:colon], name[colon + 1:]
        else:
            tag_or_digest = "latest"
    has_path = "/" in name
    first, _, _ = name.partition("/")
    if "." not in first and ":" not in first and first != "localhost":
        name = f"docker.io/{name}"
        if not has_path:
            name = f"docker.io/library/{name.removeprefix('docker.io/')}"
    return f"{name}@{tag_or_digest}" if "@" in reference else f"{name}:{tag_or_digest}"


def make_plan(repo: Path, expected_commit: str, run_root: Path) -> dict[str, Any]:
    commit = verify_source(repo, expected_commit)
    if run_root.resolve() == repo.resolve() or repo.resolve() in run_root.resolve().parents:
        raise UpdateError("RunRoot must be outside RepositoryPath")
    if run_root.exists() and any(run_root.iterdir()):
        raise UpdateError("RunRoot must be empty; refusing to overwrite prior records")
    if not SAFE_RUN_ID.fullmatch(run_root.name):
        raise UpdateError("RunRoot directory name must be a safe 1-80 character run ID")
    run_root.mkdir(parents=True, exist_ok=True, mode=0o700)
    verify_linux_amd64_nodes()
    deployment = json.loads(kubectl("-n", TARGET_NAMESPACE, "get", "deployment", TARGET_NAME, "-o", "json"))
    snapshot = sanitized_snapshot(deployment)
    if snapshot["name"] != TARGET_NAME or snapshot["namespace"] != TARGET_NAMESPACE:
        raise UpdateError("unexpected Deployment identity")
    if snapshot["nodeSelector"]:
        if snapshot["nodeSelector"].get("kubernetes.io/os", "linux") != "linux" or snapshot["nodeSelector"].get("kubernetes.io/arch", "amd64") != "amd64":
            raise UpdateError("Deployment nodeSelector is incompatible with linux/amd64")
    base = snapshot["container"]["image"]
    cached_refs = {canonical_image_ref(item) for item in image_cache()}
    if canonical_image_ref(base) not in cached_refs:
        raise UpdateError("current Portal image is not present in the local containerd cache")
    base_digest = image_digest(base)
    with tempfile.TemporaryDirectory(prefix="portal-plan-") as temp:
        export_base(canonical_image_ref(base), Path(temp) / "base.oci.tar", base_digest)
    env_names = set(snapshot["container"]["environmentNames"])
    missing = sorted(set(REQUIRED_KNOWLEDGE_ENV) - env_names)
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    target = f"{IMAGE_PREFIX}:phase-a-{commit[:12]}-{stamp.lower()}"
    plan = {
        "schemaVersion": 1,
        "runId": run_root.name,
        "createdAtUtc": dt.datetime.now(dt.timezone.utc).isoformat(),
        "repositoryPath": str(repo.resolve()),
        "branch": EXPECTED_BRANCH,
        "sourceCommit": commit,
        "target": {"name": TARGET_NAME, "namespace": TARGET_NAMESPACE, "container": TARGET_CONTAINER},
        "before": snapshot,
        "baseImage": base,
        "baseImageDigest": base_digest,
        "targetImage": target,
        "configurationGaps": {
            "missingEnvironmentVariableNames": missing,
            "apiKeySecretFieldDefault": "WEKNORA_KNOWLEDGE_API_KEY",
            "apiKeySecretValueRead": False,
            "embeddingInference": "unverified",
            "storage": "unverified",
            "parser": "unverified",
            "notes": [
                "Tenant ID and embedding model ID must be explicitly configured; this updater never patches environment variables.",
                "The model directory was previously observed empty; no model is created or inferred by this updater.",
                "Existing Secret values are neither read nor persisted. Configure the API key field through a separately reviewed change.",
            ],
        },
        "database": {
            "changes": ["knowledge_base_registry table", "knowledge_audit_events table", "parent_resource_id column/index"],
            "migrationTriggeredByPortalStartup": True,
            "rollbackRevertsDDL": False,
        },
        "rollout": {"state": "planned", "changed": False},
    }
    write_json(run_root / "plan.json", plan)
    return plan


def build_preflight(repo: Path, expected_commit: str, run_root: Path) -> dict[str, Any]:
    plan = read_json(run_root / "plan.json")
    validate_plan(plan, run_root)
    verify_source(repo, expected_commit)
    if plan.get("sourceCommit") != expected_commit or plan.get("repositoryPath") != str(repo.resolve()):
        raise UpdateError("plan source does not match requested repository and commit")
    return plan


def build_binary(repo: Path, expected_commit: str, run_root: Path) -> Path:
    build_preflight(repo, expected_commit, run_root)
    binary = run_root / "de-portal"
    if not binary.is_file():
        raise UpdateError("Windows Go build did not produce the Linux Portal binary")
    os.chmod(binary, 0o755)
    return binary


def run_linux_tests(test_binary: Path) -> int:
    if not test_binary.is_file():
        raise UpdateError("Linux Go test binary does not exist")
    fd, temp_name = tempfile.mkstemp(prefix="portal-tests-")
    os.close(fd)
    temp_binary = Path(temp_name)
    try:
        import shutil

        shutil.copyfile(test_binary, temp_binary)
        os.chmod(temp_binary, 0o700)
        return subprocess.run([str(temp_binary), "-test.v"], check=False).returncode
    finally:
        temp_binary.unlink(missing_ok=True)


def add_blob(archive: tarfile.TarFile, data: bytes, media_type: str | None = None) -> dict[str, Any]:
    digest = hashlib.sha256(data).hexdigest()
    member = tarfile.TarInfo(f"blobs/sha256/{digest}")
    member.size = len(data)
    archive.addfile(member, io.BytesIO(data))
    result = {"digest": f"sha256:{digest}", "size": len(data)}
    if media_type:
        result["mediaType"] = media_type
    return result


def build_overlay(base_tar: Path, binary: Path, output: Path, target_image: str, commit: str) -> None:
    layer_buffer = io.BytesIO()
    with tarfile.open(fileobj=layer_buffer, mode="w") as layer:
        info = tarfile.TarInfo("de-portal")
        info.mode = 0o755
        info.size = binary.stat().st_size
        with binary.open("rb") as source:
            layer.addfile(info, source)
    layer_data = layer_buffer.getvalue()
    with tarfile.open(base_tar, "r") as base, tarfile.open(output, "w") as result:
        index = json.load(base.extractfile("index.json"))
        if len(index.get("manifests") or []) != 1:
            raise UpdateError("base archive must contain exactly one manifest")
        manifest_desc = index["manifests"][0]
        manifest = resolve_oci(base, manifest_desc)
        cfg_algo, cfg_hash = manifest["config"]["digest"].split(":", 1)
        config_file = base.extractfile(f"blobs/{cfg_algo}/{cfg_hash}")
        if config_file is None:
            raise UpdateError("base image config is missing")
        config = json.loads(config_file.read())
        if config.get("os") != "linux" or config.get("architecture") != "amd64":
            raise UpdateError("base image is not linux/amd64")
        needed = {item["digest"].split(":", 1)[1] for item in manifest["layers"]}
        needed.add(cfg_hash)
        for member in base:
            if member.isfile() and member.name.startswith("blobs/sha256/") and member.name.rsplit("/", 1)[1] in needed:
                extracted = base.extractfile(member)
                if extracted is None:
                    raise UpdateError("base image layer is unreadable")
                result.addfile(member, extracted)
        layer_descriptor = add_blob(result, layer_data, "application/vnd.oci.image.layer.v1.tar")
        layer_digest = layer_descriptor["digest"]
        config.setdefault("rootfs", {}).setdefault("diff_ids", []).append(layer_digest)
        config.setdefault("config", {}).update({"Entrypoint": ["/de-portal"], "Cmd": []})
        config.setdefault("config", {}).setdefault("Labels", {}).update({"local.zhiyuan.governance.commit": commit})
        config.setdefault("history", []).append({"created_by": "Local pinned de-portal source overlay"})
        manifest.setdefault("layers", []).append(layer_descriptor)
        manifest["config"] = add_blob(result, json.dumps(config, separators=(",", ":")).encode(), "application/vnd.oci.image.config.v1+json")
        manifest_descriptor = add_blob(result, json.dumps(manifest, separators=(",", ":")).encode(), "application/vnd.oci.image.manifest.v1+json")
        manifest_descriptor.update({"annotations": {"org.opencontainers.image.ref.name": target_image}, "platform": {"architecture": "amd64", "os": "linux"}})
        for name, value in (("oci-layout", {"imageLayoutVersion": "1.0.0"}), ("index.json", {"schemaVersion": 2, "manifests": [manifest_descriptor]})):
            data = json.dumps(value, separators=(",", ":")).encode()
            info = tarfile.TarInfo(name)
            info.size = len(data)
            result.addfile(info, io.BytesIO(data))
    verify_oci_platform(output)


def prepare_image(repo: Path, expected_commit: str, run_root: Path) -> str:
    plan = read_json(run_root / "plan.json")
    validate_plan(plan, run_root)
    binary = run_root / "de-portal"
    if not binary.is_file():
        raise UpdateError("build the Portal binary before prepare")
    verify_source(repo, expected_commit)
    if plan.get("sourceCommit") != expected_commit or plan.get("repositoryPath") != str(repo.resolve()):
        raise UpdateError("plan source does not match requested repository and commit")
    if image_digest(plan["baseImage"]) != plan.get("baseImageDigest"):
        raise UpdateError("cached base image manifest changed since plan")
    with tempfile.TemporaryDirectory(prefix="portal-base-") as temp:
        base_tar = Path(temp) / "base.oci.tar"
        export_base(canonical_image_ref(plan["baseImage"]), base_tar, plan["baseImageDigest"])
        overlay = run_root / "de-portal-overlay.oci.tar"
        build_overlay(base_tar, binary, overlay, plan["targetImage"], expected_commit)
    run(["k3s", "ctr", "images", "import", "--platform", "linux/amd64", str(overlay)])
    plan["rollout"] = {"state": "prepared", "changed": False}
    write_json(run_root / "plan.json", plan)
    return plan["targetImage"]


def current_target() -> dict[str, Any]:
    return json.loads(kubectl("-n", TARGET_NAMESPACE, "get", "deployment", TARGET_NAME, "-o", "json"))


def assert_expected_live(plan: dict[str, Any], live: dict[str, Any], *, allow_target: bool = False) -> None:
    before = plan["before"]
    if live["uid"] != before["uid"]:
        raise UpdateError("Deployment UID changed since plan; refusing to overwrite another rollout")
    image = live["container"]["image"]
    policy = live["container"]["imagePullPolicy"]
    old_image = before["container"]["image"]
    if image != old_image and not (allow_target and image == plan["targetImage"]):
        raise UpdateError("current image changed since plan; refusing to overwrite another update")
    if image == old_image:
        if policy != before["container"]["imagePullPolicy"]:
            raise UpdateError("current imagePullPolicy changed since plan; refusing to overwrite another update")
        expected_marker = before.get("templateAnnotations", {}).get(UPDATE_ANNOTATION)
        current_marker = live.get("templateAnnotations", {}).get(UPDATE_ANNOTATION)
        if current_marker != expected_marker:
            raise UpdateError("current update marker changed since plan; refusing to overwrite another update")
def patch_image(name: str, namespace: str, container: str, image: str, pull_policy: str | None, marker: str | None, uid: str, resource_version: str) -> None:
    if (name, namespace, container) != (TARGET_NAME, TARGET_NAMESPACE, TARGET_CONTAINER):
        raise UpdateError("patch target must be the fixed de-portal Deployment/container")
    annotations: dict[str, Any] = {UPDATE_ANNOTATION: marker}
    patch = {"metadata": {"uid": uid, "resourceVersion": resource_version}, "spec": {"template": {"metadata": {"annotations": annotations}, "spec": {"containers": [{"name": container, "image": image, "imagePullPolicy": pull_policy}]}}}}
    kubectl("-n", namespace, "patch", "deployment", name, "--type=strategic", "--patch-file=/dev/stdin", input_text=json.dumps(patch))


def rollout(namespace: str) -> None:
    if namespace != TARGET_NAMESPACE:
        raise UpdateError("rollout namespace must be the fixed Portal namespace")
    kubectl("-n", namespace, "rollout", "status", f"deployment/{TARGET_NAME}", "--timeout=180s")


def rollback_run(run_root: Path) -> None:
    plan = read_json(run_root / "plan.json")
    validate_plan(plan, run_root)
    journal_path = run_root / "journal.json"
    if not journal_path.is_file():
        raise UpdateError("no deployment journal exists")
    journal = read_json(journal_path)
    validate_journal(journal, plan, run_root)
    live = sanitized_snapshot(current_target())
    if live["uid"] != journal["uid"]:
        raise UpdateError("Deployment UID changed; refusing rollback over a recreated Deployment")
    current_image = live["container"]["image"]
    current_policy = live["container"].get("imagePullPolicy")
    current_marker = live.get("templateAnnotations", {}).get(UPDATE_ANNOTATION)
    if current_image == journal["oldImage"]:
        if current_policy != journal.get("oldPolicy") or current_marker != journal.get("oldMarker"):
            raise UpdateError("old image is present but pull policy or update marker differs; refusing to claim rollback")
        journal["state"] = "already_rolled_back"
        write_json(journal_path, journal)
        return
    if current_image != journal["targetImage"]:
        raise UpdateError("current image is neither planned old nor target image; refusing rollback")
    if current_marker != journal["runId"]:
        raise UpdateError("target image update marker does not belong to this run; refusing rollback")
    original_marker = journal.get("oldMarker")
    try:
        patch_image(TARGET_NAME, TARGET_NAMESPACE, TARGET_CONTAINER, journal["oldImage"], journal.get("oldPolicy"), original_marker, live["uid"], live["resourceVersion"])
        rollout(TARGET_NAMESPACE)
        journal["state"] = "rolled_back"
        write_json(journal_path, journal)
    except Exception as exc:
        journal["state"] = "rollback_failed"
        write_json(journal_path, journal)
        raise UpdateError(f"Portal rollback failed; inspect run record {journal['runId']}") from exc


def deploy(run_root: Path, confirm_db_backup: bool) -> None:
    if not confirm_db_backup:
        raise UpdateError("deploy requires --confirm-db-backup")
    plan = read_json(run_root / "plan.json")
    validate_plan(plan, run_root)
    if plan.get("rollout", {}).get("state") != "prepared":
        raise UpdateError("prepare the image before deploy")
    if (run_root / "journal.json").exists():
        raise UpdateError("deployment journal already exists; use a new RunRoot to preserve prior evidence")
    cached = image_cache()
    if plan["targetImage"] not in cached:
        raise UpdateError("prepared target image is missing from local containerd cache")
    live = sanitized_snapshot(current_target())
    assert_expected_live(plan, live)
    live_image = live["container"]["image"]
    live_policy = live["container"]["imagePullPolicy"]
    old_marker = live["templateAnnotations"].get("local.zhiyuan.portal-update")
    journal = {
        "runId": plan["runId"],
        "uid": live["uid"],
        "oldImage": live_image,
        "oldPolicy": live_policy,
        "oldMarker": old_marker,
        "targetImage": plan["targetImage"],
        "state": "prepared",
    }
    journal_path = run_root / "journal.json"
    write_json(journal_path, journal)
    # Mark the change as potentially applied before issuing the patch. If the
    # client loses the response after apiserver commit, the failure still rolls back.
    journal["state"] = "patch_attempted"
    write_json(journal_path, journal)
    try:
        patch_image(TARGET_NAME, TARGET_NAMESPACE, TARGET_CONTAINER, plan["targetImage"], "Never", plan["runId"], live["uid"], live["resourceVersion"])
        journal["state"] = "patched"
        write_json(journal_path, journal)
        rollout(TARGET_NAMESPACE)
        journal["state"] = "deployed"
        write_json(journal_path, journal)
        plan["rollout"] = {"state": "deployed", "changed": True}
        write_json(run_root / "plan.json", plan)
    except Exception as original:
        try:
            rollback_run(run_root)
        except Exception as rollback_error:
            raise UpdateError(f"deploy failed and automatic rollback failed; inspect run record {plan['runId']}: {rollback_error}") from original
        raise UpdateError(f"deploy failed; original Portal image restored (run {plan['runId']})") from original


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("plan", "build-preflight", "build", "run-tests", "prepare", "deploy", "rollback"))
    parser.add_argument("--repository-path", type=Path, required=True)
    parser.add_argument("--expected-commit", required=True)
    parser.add_argument("--run-root", type=Path, required=True)
    parser.add_argument("--test-binary", type=Path)
    parser.add_argument("--confirm-db-backup", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        if args.mode == "plan":
            plan = make_plan(args.repository_path.resolve(), args.expected_commit, args.run_root.resolve())
            print(json.dumps({"runId": plan["runId"], "sourceCommit": plan["sourceCommit"], "targetImage": plan["targetImage"], "configurationGaps": plan["configurationGaps"], "snapshot": plan["before"]}, indent=2, ensure_ascii=False))
        elif args.mode == "build-preflight":
            build_preflight(args.repository_path.resolve(), args.expected_commit, args.run_root.resolve())
            print("Portal source and plan build preflight passed")
        elif args.mode == "build":
            binary = build_binary(args.repository_path.resolve(), args.expected_commit, args.run_root.resolve())
            print(f"Portal binary built: {binary.name}")
        elif args.mode == "run-tests":
            if args.test_binary is None:
                raise UpdateError("run-tests requires --test-binary")
            result = run_linux_tests(args.test_binary.resolve())
            if result:
                raise UpdateError(f"Linux Portal Go tests failed ({result})")
            print("Linux Portal Go tests passed")
        elif args.mode == "prepare":
            image = prepare_image(args.repository_path.resolve(), args.expected_commit, args.run_root.resolve())
            print(f"Prepared local Portal image: {image}")
        elif args.mode == "deploy":
            repo = args.repository_path.resolve()
            commit = verify_source(repo, args.expected_commit)
            plan = read_json(args.run_root.resolve() / "plan.json")
            validate_plan(plan, args.run_root.resolve())
            if plan.get("sourceCommit") != commit or plan.get("repositoryPath") != str(repo):
                raise UpdateError("plan source does not match requested repository and commit")
            deploy(args.run_root.resolve(), args.confirm_db_backup)
            print("de-portal rollout passed")
        else:
            repo = args.repository_path.resolve()
            commit = verify_source(repo, args.expected_commit)
            plan = read_json(args.run_root.resolve() / "plan.json")
            validate_plan(plan, args.run_root.resolve())
            if plan.get("sourceCommit") != commit or plan.get("repositoryPath") != str(repo):
                raise UpdateError("plan source does not match requested repository and commit")
            rollback_run(args.run_root.resolve())
            print("de-portal rollback passed")
        return 0
    except UpdateError as exc:
        print(f"Portal update stopped: {exc}", file=os.sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
