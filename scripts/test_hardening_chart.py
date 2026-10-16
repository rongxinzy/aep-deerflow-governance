"""Render the zhiyuan-stack chart and assert the P0 hardening contract.

Covers: NetworkPolicy toggles and the AEP /internal guard, runtime pod
defaults handed to the operator, PSA namespace labels, resource
boundaries and the off-cluster PostgreSQL backup CronJob.
"""
import json
import os
from pathlib import Path
import subprocess
import unittest

import yaml

CHART = Path(__file__).resolve().parents[1] / "charts/zhiyuan-stack"

# Real-looking values for every <...> placeholder the guards reject.
BASE = [
    "aepStack.agentControlAdvertiseUrl=http://10.0.0.1:30182",
    "aepStack.modelGatewayBaseUrl=http://10.0.0.1:30181",
    "aepStack.bootstrapAdminPassword=x",
    "aepStack.credentialMasterKeyBase64=eA==",
    "aepStack.signingKeyBase64=eA==",
    "aepStack.databaseUrl=postgres://aep:x@postgres:5432/aep",
    "aepStack.dataPlaneReconcilerToken=t",
    "aepStack.gatewayLicenseStatusToken=t",
    "aepStack.minioAccessKey=a",
    "aepStack.minioSecretKey=b",
    "aepStack.postgres.password=x",
]


def render(*settings, sets=()):
    command = [os.environ.get("HELM_BIN", "helm"), "template", "acceptance", str(CHART),
               "--set", "weknora.enabled=false"]
    for setting in BASE + list(settings):
        command += ["--set-string", setting]
    for setting in sets:
        command += ["--set", setting]
    return subprocess.run(command, text=True, capture_output=True, check=False)


def objects(result):
    return [doc for doc in yaml.safe_load_all(result.stdout) if doc]


def find(docs, kind, name=None):
    return [d for d in docs if d["kind"] == kind and (name is None or d["metadata"]["name"] == name)]


class HardeningChartTests(unittest.TestCase):
    def ok(self, *settings, sets=()):
        result = render(*settings, sets=sets)
        self.assertEqual(result.returncode, 0, result.stderr)
        return objects(result)

    def test_network_policies_default_on_and_toggleable(self):
        docs = self.ok()
        names = {(d["metadata"]["namespace"], d["metadata"]["name"]) for d in find(docs, "NetworkPolicy")}
        for namespace in ("aep-system", "digital-employees", "memory-system"):
            self.assertIn((namespace, "zhiyuan-default-deny-ingress"), names)
        self.assertIn(("digital-employees", "de-runtime-egress"), names)
        self.assertEqual(len(find(docs, "CiliumNetworkPolicy")), 1)
        off = self.ok(sets=["networkPolicy.enabled=false"])
        self.assertFalse(find(off, "NetworkPolicy"))
        self.assertFalse(find(off, "CiliumNetworkPolicy"))

    def test_runtime_cannot_reach_aep_internal(self):
        docs = self.ok()
        egress = find(docs, "NetworkPolicy", "de-runtime-egress")[0]["spec"]["egress"]
        # With the Cilium guard the L4 policy must not open control-service:
        # an L4 allow would override the L7 path filter.
        for rule in egress:
            for peer in rule.get("to", []):
                labels = peer.get("podSelector", {}).get("matchLabels", {})
                self.assertNotEqual(labels.get("app"), "aep-control-service")
                self.assertNotIn(labels.get("app"), ("rustfs",))
        cnp = find(docs, "CiliumNetworkPolicy", "de-runtime-aep-l7")[0]
        paths = [r["path"] for r in cnp["spec"]["egress"][0]["toPorts"][0]["rules"]["http"]]
        self.assertTrue(paths)
        self.assertFalse([p for p in paths if p.startswith("/internal") or p in (".*", "/.*")])
        none = self.ok("networkPolicy.aepInternalGuard=none")
        self.assertFalse(find(none, "CiliumNetworkPolicy"))
        bad = render("networkPolicy.aepInternalGuard=bogus")
        self.assertNotEqual(bad.returncode, 0)

    def test_runtime_ingress_excludes_other_runtimes(self):
        docs = self.ok()
        ingress = find(docs, "NetworkPolicy", "de-runtime-ingress")[0]["spec"]["ingress"]
        for rule in ingress:
            for peer in rule["from"]:
                if "podSelector" not in peer:
                    continue
                selector = peer["podSelector"]
                self.assertNotEqual(selector, {}, "runtime ingress must not allow the whole namespace")
                for expression in selector.get("matchExpressions", []):
                    self.assertNotEqual(expression["key"], "digitalemployee.zhiyuan.io/name")

    def test_operator_receives_runtime_defaults(self):
        docs = self.ok()
        env = {e["name"]: e.get("value") for e in
               find(docs, "Deployment", "de-operator")[0]["spec"]["template"]["spec"]["containers"][0]["env"]}
        resources = json.loads(env["RUNTIME_DEFAULT_RESOURCES"])
        self.assertIn("memory", resources["limits"])
        security = json.loads(env["RUNTIME_SECURITY_CONTEXT"])
        self.assertEqual(security["capabilities"]["drop"], ["ALL"])
        self.assertFalse(security["allowPrivilegeEscalation"])
        self.assertEqual(security["seccompProfile"]["type"], "RuntimeDefault")
        legacy = self.ok(sets=["runtimeDefaults.securityContext=null", "runtimeDefaults.resources=null"])
        env = {e["name"] for e in
               find(legacy, "Deployment", "de-operator")[0]["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertNotIn("RUNTIME_SECURITY_CONTEXT", env)

    def test_psa_labels_and_privileged_escape_hatch(self):
        docs = self.ok()
        main = find(docs, "Namespace", "digital-employees")[0]["metadata"]["labels"]
        self.assertEqual(main["pod-security.kubernetes.io/enforce"], "baseline")
        self.assertEqual(main["pod-security.kubernetes.io/warn"], "restricted")
        agent = self.ok(sets=["workspaceAgent.enabled=true"])
        main = find(agent, "Namespace", "digital-employees")[0]["metadata"]["labels"]
        self.assertEqual(main["pod-security.kubernetes.io/enforce"], "privileged")
        memory = find(agent, "Namespace", "memory-system")[0]["metadata"]["labels"]
        self.assertEqual(memory["pod-security.kubernetes.io/enforce"], "baseline")

    def test_resource_boundaries(self):
        docs = self.ok()
        self.assertEqual(len(find(docs, "LimitRange")), 1)
        self.assertFalse(find(docs, "ResourceQuota"))
        self.assertEqual(len(find(self.ok(sets=["resourceBoundaries.resourceQuota.enabled=true"]), "ResourceQuota")), 1)

    def test_backup_only_with_offsite_target(self):
        self.assertFalse(find(self.ok(), "CronJob", "pg-backup"))
        missing = render("backup.postgres.s3.bucket=b")
        self.assertNotEqual(missing.returncode, 0)
        self.assertIn("endpoint is required", missing.stderr)
        docs = self.ok("backup.postgres.s3.bucket=zy-backups", "backup.postgres.s3.endpoint=https://s3.example.com")
        cron = find(docs, "CronJob", "pg-backup")[0]
        pod = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]
        upload = pod["containers"][0]
        env = {e["name"]: e for e in upload["env"]}
        self.assertIn("secretKeyRef", env["RCLONE_CONFIG_DST_SECRET_ACCESS_KEY"]["valueFrom"])
        self.assertEqual(env["DEST"]["value"], "dst:zy-backups/zhiyuan/postgres")
        self.assertIn("--min-age", upload["args"][0])
        self.assertNotIn("SECRET_ACCESS_KEY=", cron.__repr__().replace("RCLONE_CONFIG_DST_SECRET_ACCESS_KEY", ""))


if __name__ == "__main__":
    unittest.main()
