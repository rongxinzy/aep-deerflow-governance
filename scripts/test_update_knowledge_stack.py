import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("knowledge_update", Path(__file__).with_name("update_knowledge_stack.py"))
update = importlib.util.module_from_spec(spec)
spec.loader.exec_module(update)
OLD = "example/old@sha256:" + "a" * 64
NEW = "example/new@sha256:" + "b" * 64


def deployment():
    return {"metadata": {"uid": "deploy-uid", "resourceVersion": "12"},
            "spec": {"selector": {"matchLabels": {"app": "worker"}},
                     "template": {"metadata": {}, "spec": {"containers": [{"name": "worker", "image": OLD}]}}}}


class FakeCluster:
    def __init__(self):
        self.objects = {name: deployment() for name in update.COMPONENTS}
        self.patches = []

    def verify(self):
        pass

    def get(self, target):
        return copy.deepcopy(self.objects[target["name"]])

    def call(self, *args, input_text=None):
        if "patch" in args:
            name = args[4]
            ops = json.loads(input_text)
            obj = self.objects[name]
            obj["spec"]["template"]["spec"]["containers"][0]["image"] = ops[3]["value"]
            obj["spec"]["template"]["metadata"]["annotations"] = ops[4]["value"]
            obj["metadata"]["resourceVersion"] = str(int(obj["metadata"]["resourceVersion"]) + 1)
            self.patches.append(name)
        return "{}"


def manifest():
    components = {}
    for name in update.COMPONENTS:
        required = {key: {"secret": {"name": "test-only", "key": key}} if key in update.SECRET_ENV else {"value": "fixture"}
                    for key in update.REQUIRED_ENV.get(name, set())}
        if name == "mcp":
            required["PORTAL_KNOWLEDGE_AUTH_REQUIRED"] = {"value": "true"}
        if name == "portal":
            required["PORTAL_RUNTIME_IMAGE"] = {"value": NEW}
        components[name] = {"source": {"path": "/unused", "sha": "c" * 40}, "image": NEW,
                            "targets": [{"kind": "deployment", "namespace": "isolated", "name": name, "container": "worker"}],
                            "requiredEnv": required}
    return {"components": components, "databases": {}, "cluster": {"architecture": "arm64"}}


class StackUpdateTests(unittest.TestCase):
    def setUp(self):
        self.cluster = FakeCluster()
        self.manifest = manifest()
        self.mocks = [patch.object(update, name) for name in ("source_check", "backup_check", "prepare_images", "env_check")]
        for mock in self.mocks:
            mock.start()
            self.addCleanup(mock.stop)

    def plan(self):
        return update.make_plan(self.manifest, self.cluster)

    def test_all_six_order_and_no_raw_object(self):
        plan = self.plan()
        self.assertEqual([r["component"] for r in plan["records"]], list(update.COMPONENTS))
        self.assertNotIn("resourceVersion", json.dumps(plan["records"]))

    def test_incomplete_components_rejected(self):
        del self.manifest["components"]["runtime"]
        with self.assertRaises(update.UpdateError):
            self.plan()

    def test_tag_and_mutable_rollback_rejected(self):
        for target in ("new", "old"):
            with self.subTest(target=target):
                manifest_value = manifest()
                cluster = FakeCluster()
                if target == "new":
                    manifest_value["components"]["admin"]["image"] = "example/admin:latest"
                else:
                    cluster.objects["admin"]["spec"]["template"]["spec"]["containers"][0]["image"] = "example/admin:old"
                with self.assertRaises(update.UpdateError):
                    update.make_plan(manifest_value, cluster)

    def test_empty_targets_rejected(self):
        self.manifest["components"]["runtime"]["targets"] = []
        with self.assertRaises(update.UpdateError):
            self.plan()

    def test_secret_in_plain_env_rejected(self):
        self.manifest["components"]["mcp"]["requiredEnv"]["WEKNORA_API_KEY"] = {"value": "synthetic"}
        with self.assertRaises(update.UpdateError):
            self.plan()

    def test_missing_config_and_optional_auth_rejected(self):
        for mutation in ("missing", "auth"):
            value = manifest()
            if mutation == "missing":
                value["components"]["portal"]["requiredEnv"] = {}
            else:
                value["components"]["mcp"]["requiredEnv"]["PORTAL_KNOWLEDGE_AUTH_REQUIRED"] = {"value": "false"}
            with self.assertRaises(update.UpdateError):
                update.make_plan(value, self.cluster)

    def test_drift_and_replaced_uid_no_patch(self):
        record = self.plan()["records"][0]
        for field, value in (("uid", "new-uid"),):
            self.cluster.objects["weknora"]["metadata"][field] = value
            with self.assertRaises(update.UpdateError):
                update.patch_image(self.cluster, record, OLD, NEW, "run")
        self.assertEqual(self.cluster.patches, [])

    def test_rollback_requires_run_marker(self):
        record = self.plan()["records"][0]
        self.cluster.objects["weknora"]["spec"]["template"]["spec"]["containers"][0]["image"] = NEW
        with self.assertRaises(update.UpdateError):
            update.patch_image(self.cluster, record, NEW, OLD, "run", rollback=True)

    def test_rollout_failure_restores_failed_and_previous_reverse_order(self):
        plan = self.plan()
        calls = 0

        def ready(*args):
            nonlocal calls
            calls += 1
            if calls == 3:
                raise update.UpdateError("rollout failed")

        with tempfile.TemporaryDirectory() as temp, patch.object(update, "wait_ready", side_effect=ready):
            with self.assertRaisesRegex(update.UpdateError, "attempted images restored"):
                update.apply(plan, self.cluster, Path(temp) / "journal.json")
            self.assertEqual(self.cluster.patches, ["weknora", "mcp", "operator", "operator", "mcp", "weknora"])
            self.assertEqual(json.loads((Path(temp) / "journal.json.result").read_text())["state"], "rolled-back")

    def test_apply_live_drift_before_any_mutation(self):
        plan = self.plan()
        self.cluster.objects["admin"]["metadata"]["uid"] = "different"
        with tempfile.TemporaryDirectory() as temp:
            with self.assertRaises(update.UpdateError):
                update.apply(plan, self.cluster, Path(temp) / "journal.json")
        self.assertEqual(self.cluster.patches, [])

    def test_patch_response_lost_restores_committed_component(self):
        plan = self.plan()
        original = self.cluster.call
        lost = False

        def call(*args, **kwargs):
            nonlocal lost
            result = original(*args, **kwargs)
            if "patch" in args and not lost:
                lost = True
                raise update.UpdateError("response lost")
            return result

        with tempfile.TemporaryDirectory() as temp, patch.object(self.cluster, "call", side_effect=call), patch.object(update, "wait_ready"):
            with self.assertRaisesRegex(update.UpdateError, "attempted images restored"):
                update.apply(plan, self.cluster, Path(temp) / "journal.json")
        self.assertEqual(self.cluster.patches, ["weknora", "weknora"])

    def test_external_image_drift_stops_rollback_and_records_incomplete(self):
        plan = self.plan()

        def ready(*args):
            self.cluster.objects["weknora"]["spec"]["template"]["spec"]["containers"][0]["image"] = "other@sha256:" + "d" * 64
            raise update.UpdateError("external update")

        with tempfile.TemporaryDirectory() as temp, patch.object(update, "wait_ready", side_effect=ready):
            with self.assertRaisesRegex(update.UpdateError, "rollback incomplete"):
                update.apply(plan, self.cluster, Path(temp) / "journal.json")
            self.assertEqual(json.loads((Path(temp) / "journal.json.result").read_text())["failures"], ["weknora"])
        self.assertEqual(self.cluster.patches, ["weknora"])


class GatesTests(unittest.TestCase):
    def test_only_dedicated_context(self):
        for context in ("default", "kind-production", "shared"):
            with self.assertRaises(update.UpdateError):
                update.Cluster({"context": context, "kubeconfig": "/unused"})

    def test_loopback_cluster_check(self):
        cluster = update.Cluster({"context": "kind-zhiyuan-knowledge-test", "kubeconfig": "/unused", "node": "test"})
        for server in ("https://example.com", "http://127.0.0.1", "https://127.0.0.1.example.com"):
            with patch.object(cluster, "call", return_value=json.dumps({"clusters": [{"cluster": {"server": server}}]})):
                with self.assertRaises(update.UpdateError):
                    cluster.verify()

    def test_cr_uses_runtime_image_and_metadata_annotations(self):
        obj = {"metadata": {"annotations": {"existing": "yes"}}, "spec": {"runtime": {"image": OLD}}}
        target = {"kind": "digitalemployee"}
        self.assertEqual(update.image_path(obj, target), ("/spec/runtime/image", OLD))
        self.assertEqual(update.marker_location(obj, target)[0], "/metadata/annotations")

    def test_env_selector_only(self):
        obj = deployment()
        obj["spec"]["template"]["spec"]["containers"][0]["env"] = [{"name": "KEY", "valueFrom": {"secretKeyRef": {"name": "fixture", "key": "value"}}}]
        update.env_check(obj, {"KEY": {"secret": {"name": "fixture", "key": "value"}}})
        with self.assertRaises(update.UpdateError):
            update.env_check(obj, {"KEY": {"secret": {"name": "other", "key": "value"}}})

    def test_private_backup_hash_receipt(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            backup = root / "fixture.dump"
            backup.write_bytes(b"synthetic backup")
            backup.chmod(0o600)
            digest = update.hashlib.sha256(backup.read_bytes()).hexdigest()
            receipt = root / "restore.json"
            receipt.write_text(json.dumps({"backupSHA256": digest, "isolated": True, "checksPassed": True, "database": "knowledge_acceptance_fixture"}))
            item = {"artifact": str(backup), "sha256": digest, "restoreReceipt": str(receipt)}
            update.backup_check({"portal": item, "weknora": item})
            backup.chmod(0o644)
            with self.assertRaises(update.UpdateError):
                update.backup_check({"portal": item, "weknora": item})

    def test_actual_pod_owner_chain_and_digest(self):
        target = {"kind": "deployment", "name": "worker", "namespace": "isolated", "container": "worker"}
        obj = deployment()
        obj["spec"]["template"]["spec"]["containers"][0]["image"] = NEW
        rs = {"metadata": {"uid": "rs-uid", "ownerReferences": [{"controller": True, "kind": "Deployment", "uid": "deploy-uid"}]}}
        pod = {"metadata": {"ownerReferences": [{"controller": True, "kind": "ReplicaSet", "name": "worker-rs", "uid": "rs-uid"}]},
               "status": {"containerStatuses": [{"name": "worker", "ready": True, "imageID": "docker-pullable://" + NEW}]}}
        cluster = unittest.mock.Mock()
        cluster.get.return_value = obj

        def call(*args):
            if "pods" in args:
                return json.dumps({"items": [pod]})
            if "replicaset" in args:
                return json.dumps(rs)
            return ""

        cluster.call.side_effect = call
        update.wait_ready(cluster, {"target": target}, NEW)
        for mutation in ("digest", "owner", "ready"):
            with self.subTest(mutation=mutation):
                damaged = copy.deepcopy(pod)
                if mutation == "digest":
                    damaged["status"]["containerStatuses"][0]["imageID"] = OLD
                elif mutation == "owner":
                    damaged["metadata"]["ownerReferences"][0]["uid"] = "other"
                else:
                    damaged["status"]["containerStatuses"][0]["ready"] = False
                with patch.object(cluster, "call", side_effect=lambda *args: json.dumps({"items": [damaged]}) if "pods" in args else json.dumps(rs) if "replicaset" in args else ""):
                    with self.assertRaises(update.UpdateError):
                        update.wait_ready(cluster, {"target": target}, NEW)


if __name__ == "__main__":
    unittest.main()
