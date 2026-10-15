#!/usr/bin/env python3
"""Seed only the disposable, loopback Kubernetes/AEP acceptance stack.

Real AEP login determines ownership. Memory/model services are deliberately out
of scope; the fixture supplies an internal companion token, never a provider key.
Run on the control host with its isolated KUBECONFIG. No tokens are printed.
"""
import json
import os
import subprocess
import urllib.request

config = json.loads(subprocess.check_output(["kubectl", "config", "view", "--minify", "-o", "json"]))
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

apply("Service", "kube-dns", "kube-system", spec=dict(clusterIP="10.243.0.10", ports=[dict(port=53, protocol="UDP")]))
apply("Endpoints", "kube-dns", "kube-system", subsets=[])
apply("Namespace", "higress-system", None)
apply("Endpoints", "higress-gateway", "higress-system", subsets=[])
apply("Secret", "qa-companion", "cloud-qa", stringData={"WORKSPACE_INTERNAL_TOKEN": "isolated-companion-fixture"})
workspace = dict(apiVersion="digitalemployee.zhiyuan.io/v1alpha1", kind="PersonalWorkspace",
    metadata=dict(name="cloud-qa", namespace="cloud-qa"), spec=dict(owner=owner, runtime="microvm",
    credentials=dict(name="qa-companion"), templateTag="qa-v1", desktop=dict(enabled=True), resources=dict(vcpus=2, memory="4Gi", disk="2Gi"),
    companions=[dict(name="assistant", displayName="工作助手", model="qa-no-inference", memory=dict(userKeySecret="qa-companion"))]))
subprocess.run(["kubectl", "apply", "-f", "-"], input=json.dumps(workspace).encode(), check=True)
print("Real AEP owner assigned; isolated workspace seeded (inference and memory excluded).")
