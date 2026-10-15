#!/usr/bin/env python3
"""Seed a disposable workspace using an isolated AEP account.

Real AEP login determines ownership. Memory/model services are deliberately out
of scope; the fixture supplies an internal companion token, never a provider key.
The shared-cluster mode writes only into a labeled cloud-qa-* namespace.
It never writes shared DNS or gateway resources. No tokens are printed.
"""
import argparse
import json
import os
import subprocess
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--shared-cluster-acceptance", action="store_true", help="Use an explicitly labeled disposable namespace on the real cluster")
parser.add_argument("--namespace", default="cloud-qa")
parser.add_argument("--template", default="qa-v1")
args = parser.parse_args()
if args.shared_cluster_acceptance:
    assert args.namespace.startswith("cloud-qa-"), "disposable namespace required"
    namespace = json.loads(subprocess.check_output(["kubectl", "get", "namespace", args.namespace, "-o", "json"]))
    assert namespace["metadata"].get("labels", {}).get("zhiyuan.io/acceptance") == "harbor-daemonset", "acceptance namespace label required"
config = json.loads(subprocess.check_output(["kubectl", "config", "view", "--minify", "-o", "json"]))
if not args.shared_cluster_acceptance:
    assert config["clusters"][0]["cluster"]["server"] == "https://127.0.0.1:16443", "isolated API required"
base = "http://127.0.0.1:18080"
headers = {"Content-Type": "application/json", "X-AEP-Protocol-Version": "1.0"}
body = dict(deploymentId="demo", sessionId="cloud-control-qa-seed", username="admin",
            password=os.environ["WORKPLACE_ACCEPTANCE_PASSWORD"])
with urllib.request.urlopen(urllib.request.Request(base + "/aep/v1/auth/password/login", json.dumps(body).encode(), headers)) as response:
    token = json.load(response)["accessToken"]
with urllib.request.urlopen(urllib.request.Request(base + "/aep/v1/user/me", headers={**headers, "Authorization": "Bearer " + token})) as response:
    owner = json.load(response)["user"]["id"]

def apply(kind, name, namespace, **fields):
    obj = dict(apiVersion="v1", kind=kind, metadata=dict(name=name, namespace=namespace), **fields)
    subprocess.run(["kubectl", "apply", "-f", "-"], input=json.dumps(obj).encode(), check=True)

if not args.shared_cluster_acceptance:
    apply("Service", "kube-dns", "kube-system", spec=dict(clusterIP="10.243.0.10", ports=[dict(port=53, protocol="UDP")]))
    apply("Endpoints", "kube-dns", "kube-system", subsets=[])
    apply("Namespace", "higress-system", None)
    apply("Endpoints", "higress-gateway", "higress-system", subsets=[])
apply("Secret", "qa-companion", args.namespace, stringData={"WORKSPACE_INTERNAL_TOKEN": "isolated-companion-fixture"})
workspace = dict(apiVersion="digitalemployee.zhiyuan.io/v1alpha1", kind="PersonalWorkspace",
    metadata=dict(name="cloud-qa", namespace=args.namespace), spec=dict(owner=owner, runtime="microvm",
    credentials=dict(name="qa-companion"), templateTag=args.template, desktop=dict(enabled=True), resources=dict(vcpus=2, memory="4Gi", disk="2Gi"),
    companions=[dict(name="assistant", displayName="工作助手", model="qa-no-inference", memory=dict(userKeySecret="qa-companion"))]))
subprocess.run(["kubectl", "apply", "-f", "-"], input=json.dumps(workspace).encode(), check=True)
print("Real AEP owner assigned; isolated workspace seeded (inference and memory excluded).")
