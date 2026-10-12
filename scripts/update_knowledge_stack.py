#!/usr/bin/env python3
"""Six-component update transaction for an explicitly isolated local cluster.

Prepared images and database restore evidence are inputs, never inferred from
a tag or a checkbox. This tool does not build images, migrate/restore databases,
read Secrets, or provision a cluster. All Kubernetes writes are JSON patches.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
import uuid
from urllib.parse import urlparse

COMPONENTS = ("weknora", "mcp", "operator", "runtime", "portal", "admin")
DIGEST = re.compile(r"^[\w./:-]+@sha256:[0-9a-f]{64}$")
SHA = re.compile(r"^[0-9a-f]{40}$")
MARKER = "local.zhiyuan.knowledge-update"
REQUIRED_ENV = {
    "portal": {"PORTAL_KNOWLEDGE_TENANT_ID", "PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID", "PORTAL_KNOWLEDGE_API_KEY_SECRET_FIELD", "PORTAL_CREDENTIALS_SECRET", "PORTAL_RUNTIME_IMAGE"},
    "mcp": {"PORTAL_KNOWLEDGE_AUTH_URL", "PORTAL_KNOWLEDGE_AUTH_REQUIRED", "WEKNORA_API_KEY", "MCP_SERVER_AUTH_TOKEN"},
}
SECRET_ENV = {"WEKNORA_API_KEY", "MCP_SERVER_AUTH_TOKEN"}


class UpdateError(RuntimeError):
    pass


def run(args, input_text=None):
    result = subprocess.run(args, input=input_text, text=True, capture_output=True)
    if result.returncode:
        # Neither stdout nor stderr is safe to publish for cluster commands.
        raise UpdateError(f"command failed: {args[0]}")
    return result.stdout


def write_json(path, value):
    path = Path(path)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        json.dump(value, stream, indent=2)


def source_check(source):
    root = Path(source["path"]).resolve()
    if not SHA.fullmatch(source["sha"]):
        raise UpdateError("source needs a full SHA")
    if run(["git", "-C", str(root), "rev-parse", "HEAD"]).strip() != source["sha"]:
        raise UpdateError("source HEAD changed")
    if run(["git", "-C", str(root), "status", "--porcelain", "--untracked-files=all"]).strip():
        raise UpdateError("source must be clean")


def backup_check(evidence):
    if set(evidence) != {"portal", "weknora"}:
        raise UpdateError("both databases need backup and isolated restore evidence")
    for item in evidence.values():
        raw_path = Path(item["artifact"])
        path = raw_path.resolve()
        if raw_path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
            raise UpdateError("backup must be a private regular file")
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        receipt = json.loads(Path(item["restoreReceipt"]).read_text())
        if (digest != item["sha256"] or receipt.get("backupSHA256") != digest
                or receipt.get("isolated") is not True or receipt.get("checksPassed") is not True
                or not receipt.get("database", "").startswith("knowledge_acceptance_")):
            raise UpdateError("backup or isolated restore evidence does not match")


class Cluster:
    def __init__(self, config):
        self.config = config
        if not config["context"].startswith("kind-zhiyuan-knowledge-"):
            raise UpdateError("only a dedicated knowledge acceptance kind context is allowed")
        self.prefix = ["kubectl", "--kubeconfig", str(Path(config["kubeconfig"]).resolve()),
                       "--context", config["context"]]

    def call(self, *args, input_text=None):
        return run([*self.prefix, *args], input_text)

    def verify(self):
        config = json.loads(self.call("config", "view", "--minify", "-o", "json"))
        server = urlparse(config["clusters"][0]["cluster"]["server"])
        if server.scheme != "https" or server.hostname not in {"127.0.0.1", "localhost", "::1"}:
            raise UpdateError("cluster endpoint must be loopback HTTPS")
        nodes = json.loads(self.call("get", "nodes", "-o", "json"))["items"]
        if len(nodes) != 1 or any(n["metadata"]["labels"].get("kubernetes.io/hostname", "")
                            != self.config["node"] for n in nodes):
            raise UpdateError("unexpected local cluster node")

    def get(self, target):
        return json.loads(self.call("-n", target["namespace"], "get", target["kind"],
                                    target["name"], "-o", "json"))


def image_path(obj, target):
    if target["kind"] == "digitalemployee":
        return "/spec/runtime/image", obj["spec"]["runtime"]["image"]
    containers = obj["spec"]["template"]["spec"]["containers"]
    matches = [(i, c) for i, c in enumerate(containers) if c["name"] == target["container"]]
    if len(matches) != 1:
        raise UpdateError("target container must exist exactly once")
    i, container = matches[0]
    return f"/spec/template/spec/containers/{i}/image", container["image"]


def env_check(obj, required, container=None):
    # Checks selectors/references, never fetches Secret values.
    entries = {e["name"]: e for c in obj["spec"]["template"]["spec"]["containers"] if container is None or c["name"] == container
               for e in c.get("env", [])}
    for name, expectation in required.items():
        entry = entries.get(name, {})
        if expectation.get("secret"):
            if entry.get("valueFrom", {}).get("secretKeyRef") != expectation["secret"]:
                raise UpdateError("required Secret selector differs")
        elif entry.get("value") != expectation.get("value"):
            raise UpdateError("required non-secret configuration differs")


def prepare_images(manifest):
    """Verify already-built local images; never manufacture build provenance."""
    if set(manifest["components"]) != set(COMPONENTS):
        raise UpdateError("all six components are mandatory")
    for component in manifest["components"].values():
        source_check(component["source"])
        image = component["image"]
        if not DIGEST.fullmatch(image):
            raise UpdateError("target images require immutable digests")
        data = json.loads(run(["docker", "image", "inspect", image]))
        if len(data) != 1 or image not in (data[0].get("RepoDigests") or []):
            raise UpdateError("prepared image digest missing locally")
        labels = data[0].get("Config", {}).get("Labels", {}) or {}
        if labels.get("org.opencontainers.image.revision") != component["source"]["sha"]:
            raise UpdateError("image source revision provenance differs")
        if data[0].get("Os") != "linux" or data[0].get("Architecture") != manifest["cluster"]["architecture"]:
            raise UpdateError("prepared image platform differs")


def marker_location(obj, target):
    if target["kind"] == "digitalemployee":
        return "/metadata/annotations", obj["metadata"].get("annotations", {})
    return "/spec/template/metadata/annotations", obj["spec"]["template"]["metadata"].get("annotations", {})


def make_plan(manifest, cluster):
    if set(manifest) != {"cluster", "components", "databases"}:
        raise UpdateError("unexpected manifest fields")
    if set(manifest["components"]) != set(COMPONENTS):
        raise UpdateError("all six components are mandatory")
    cluster.verify()
    prepare_images(manifest)
    backup_check(manifest["databases"])
    records = []
    seen = set()
    for name in COMPONENTS:
        component = manifest["components"][name]
        if set(component) - {"source", "image", "targets", "requiredEnv"} or set(component["source"]) != {"path", "sha"}:
            raise UpdateError("unexpected component fields")
        if not DIGEST.fullmatch(component["image"]):
            raise UpdateError("target images require immutable registry digests")
        source_check(component["source"])
        required = component.get("requiredEnv", {})
        if set(required) - REQUIRED_ENV.get(name, set()):
            raise UpdateError("unexpected configuration fields; credentials must not enter plans")
        if not REQUIRED_ENV.get(name, set()).issubset(required):
            raise UpdateError("component is missing required configuration checks")
        if any(key in SECRET_ENV and not value.get("secret") for key, value in required.items()):
            raise UpdateError("credentials must use Secret selectors")
        if name == "mcp" and required["PORTAL_KNOWLEDGE_AUTH_REQUIRED"] != {"value": "true"}:
            raise UpdateError("MCP authorization must remain required")
        if name == "portal" and required["PORTAL_RUNTIME_IMAGE"] != {"value": manifest["components"]["runtime"]["image"]}:
            raise UpdateError("new employees must use the prepared runtime")
        if not component["targets"]:
            raise UpdateError("each component needs a live target")
        for target in component["targets"]:
            if set(target) - {"kind", "namespace", "name", "container", "deployment"}:
                raise UpdateError("unexpected target fields")
            key = (target["namespace"], target["kind"], target["name"], target.get("container"))
            if key in seen or target["kind"] not in {"deployment", "digitalemployee"}:
                raise UpdateError("invalid or duplicate target")
            if target["kind"] == "digitalemployee" and name != "runtime":
                raise UpdateError("only runtime may target employee CRs")
            seen.add(key)
            obj = cluster.get(target)
            path, old_image = image_path(obj, target)
            if not DIGEST.fullmatch(old_image):
                raise UpdateError("rollback images must also have immutable digests")
            if target["kind"] == "deployment":
                env_check(obj, required, target["container"])
            _, annotations = marker_location(obj, target)
            records.append({"component": name, "target": target, "path": path,
                            "uid": obj["metadata"]["uid"], "oldImage": old_image,
                            "image": component["image"], "oldMarker": annotations.get(MARKER)})
    return {"version": 1, "records": records, "manifest": manifest}


def patch_image(cluster, record, expected, desired, run_id, rollback=False):
    obj = cluster.get(record["target"])
    path, current = image_path(obj, record["target"])
    if obj["metadata"]["uid"] != record["uid"] or path != record["path"] or current != expected:
        raise UpdateError("target identity or image drifted; refusing overwrite")
    annotations_path, annotations = marker_location(obj, record["target"])
    if rollback and annotations.get(MARKER) != run_id:
        raise UpdateError("update marker differs; refusing rollback")
    updated_annotations = dict(annotations)
    if rollback and record["oldMarker"] is None:
        updated_annotations.pop(MARKER, None)
    else:
        updated_annotations[MARKER] = record["oldMarker"] if rollback else run_id
    patch = [{"op": "test", "path": "/metadata/uid", "value": record["uid"]},
             {"op": "test", "path": "/metadata/resourceVersion", "value": obj["metadata"]["resourceVersion"]},
             {"op": "test", "path": path, "value": expected},
             {"op": "replace", "path": path, "value": desired},
             {"op": "add", "path": annotations_path, "value": updated_annotations}]
    target = record["target"]
    cluster.call("-n", target["namespace"], "patch", target["kind"], target["name"],
                 "--type=json", "--patch-file=/dev/stdin", input_text=json.dumps(patch))


def wait_ready(cluster, record, image):
    target = record["target"]
    # Runtime targets explicitly specify the controller-owned Deployment. Its
    # rollout may lag the CR write; wait for the desired image before rollout.
    deployment = dict(target, kind="deployment", name=target.get("deployment", target["name"]))
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        obj = cluster.get(deployment)
        _, current = image_path(obj, deployment)
        if current == image:
            break
        time.sleep(1)
    else:
        raise UpdateError("controller did not reconcile desired runtime image")
    cluster.call("-n", target["namespace"], "rollout", "status",
                 f"deployment/{deployment['name']}", "--timeout=180s")
    # Verify the Pod -> ReplicaSet -> Deployment chain, not synthetic statuses
    # on Deployment objects. Every selected live Pod must run the target digest.
    obj = cluster.get(deployment)
    selector = obj["spec"]["selector"].get("matchLabels", {})
    if not selector:
        raise UpdateError("deployment requires explicit matchLabels")
    pods = json.loads(cluster.call("-n", target["namespace"], "get", "pods", "-l",
                                  ",".join(f"{k}={v}" for k, v in sorted(selector.items())), "-o", "json"))["items"]
    live = [p for p in pods if not p["metadata"].get("deletionTimestamp")]
    if not live:
        raise UpdateError("no running Pods")
    for pod in live:
        owners = [o for o in pod["metadata"].get("ownerReferences", []) if o.get("controller") and o["kind"] == "ReplicaSet"]
        if len(owners) != 1:
            raise UpdateError("Pod owner is invalid")
        rs = json.loads(cluster.call("-n", target["namespace"], "get", "replicaset", owners[0]["name"], "-o", "json"))
        if rs["metadata"]["uid"] != owners[0]["uid"] or not any(o.get("controller") and o["uid"] == obj["metadata"]["uid"] for o in rs["metadata"].get("ownerReferences", [])):
            raise UpdateError("Pod does not belong to target Deployment")
        statuses = [s for s in pod.get("status", {}).get("containerStatuses", []) if s["name"] == target["container"]]
        if len(statuses) != 1 or not statuses[0].get("ready") or not statuses[0].get("imageID", "").endswith(image.split("@", 1)[1]):
            raise UpdateError("Pod readiness or image digest differs")


def apply(plan, cluster, journal):
    # Re-plan from the same clean sources and evidence before any mutation.
    fresh = make_plan(plan["manifest"], cluster)
    if fresh["records"] != plan["records"]:
        raise UpdateError("live state changed since plan")
    attempted = []
    run_id = str(uuid.uuid4())
    write_json(journal, {"state": "started", "runId": run_id, "plan": plan})
    try:
        for record in plan["records"]:
            # Include failed patch requests: the transport may lose a successful
            # response. Rollback resolves live state rather than assuming failure.
            attempted.append(record)
            patch_image(cluster, record, record["oldImage"], record["image"], run_id)
            wait_ready(cluster, record, record["image"])
    except Exception as failure:
        failures = []
        for record in reversed(attempted):
            try:
                obj = cluster.get(record["target"])
                _, current = image_path(obj, record["target"])
                _, annotations = marker_location(obj, record["target"])
                if obj["metadata"]["uid"] != record["uid"]:
                    raise UpdateError("object replaced; refusing rollback")
                if current != record["oldImage"]:
                    patch_image(cluster, record, record["image"], record["oldImage"], run_id, rollback=True)
                    wait_ready(cluster, record, record["oldImage"])
                elif annotations.get(MARKER) != record["oldMarker"]:
                    raise UpdateError("old image ownership differs")
            except Exception:
                failures.append(record["component"])
        write_json(str(journal) + ".result", {"state": "rollback-incomplete" if failures else "rolled-back", "failures": failures, "runId": run_id})
        raise UpdateError("update failed; rollback incomplete: " + ",".join(failures) if failures else "update failed; attempted images restored") from failure
    write_json(str(journal) + ".result", {"state": "updated", "runId": run_id})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "plan", "apply"), nargs="?", default="plan")
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--plan", type=Path, required=True)
    parser.add_argument("--journal", type=Path)
    args = parser.parse_args()
    manifest = json.loads(args.manifest.read_text())
    cluster = Cluster(manifest["cluster"])
    if args.action == "prepare":
        prepare_images(manifest)
        write_json(args.plan, {"state": "prepared", "components": list(COMPONENTS)})
    elif args.action == "plan":
        write_json(args.plan, make_plan(manifest, cluster))
    else:
        if args.journal is None:
            raise UpdateError("apply requires a new private journal path")
        plan = json.loads(args.plan.read_text())
        if plan["manifest"] != manifest:
            raise UpdateError("manifest changed since plan")
        apply(plan, cluster, args.journal)
    print({"prepare": "prepared image metadata verified; no deployment performed",
           "plan": "plan prepared; no deployment performed",
           "apply": "six-component images updated; business acceptance still required"}[args.action])


if __name__ == "__main__":
    try:
        main()
    except (UpdateError, KeyError, ValueError, OSError):
        print("knowledge stack update rejected; inspect inputs and private journal")
        raise SystemExit(1)
