import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


MODULE_PATH = Path(__file__).with_name("update_local_portal.py")
SPEC = importlib.util.spec_from_file_location("update_local_portal", MODULE_PATH)
update = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(update)

COMMIT = "a" * 40
OLD_IMAGE = "registry.local/de-portal:current"
TARGET_IMAGE = f"docker.io/zhiyuan-local/de-portal:phase-a-{COMMIT[:12]}-20261009t120000z"
BASE_DIGEST = "sha256:" + "1" * 64


def deployment(image=OLD_IMAGE, uid="uid-1", resource_version="10", containers=None):
    if containers is None:
        containers = [{
            "name": "portal",
            "image": image,
            "imagePullPolicy": "IfNotPresent",
            "env": [
                {"name": "DATABASE_URL", "value": "sensitive-literal-never-persist"},
                {"name": "PORTAL_AEP_DEPLOYMENT_ID", "valueFrom": {"secretKeyRef": {"name": "portal-config", "key": "deployment-id"}}},
            ],
            "envFrom": [{"secretRef": {"name": "de-platform-creds"}}],
            "volumeMounts": [{"name": "portal-db-certs", "mountPath": "/etc/db", "readOnly": True}],
        }]
    return {
        "metadata": {"name": "de-portal", "namespace": "digital-employees", "uid": uid, "resourceVersion": resource_version},
        "spec": {"replicas": 1, "template": {"metadata": {"annotations": {
            "keep": "unchanged",
            "authorization": "annotation-token-never-persist",
            "kubectl.kubernetes.io/last-applied-configuration": "sensitive-last-applied-config",
        }}, "spec": {
            "serviceAccountName": "de-portal",
            "nodeSelector": {"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64", "internal-zone": "secret-zone-label"},
            "containers": containers,
            "volumes": [
                {"name": "portal-db-certs", "secret": {"secretName": "db-client-cert"}},
                {"name": "portal-data", "persistentVolumeClaim": {"claimName": "portal-data-pvc"}},
            ],
        }}}
    }


def plan_fixture(root: Path, before=None):
    before = before or update.sanitized_snapshot(deployment())
    plan = {
        "schemaVersion": 1,
        "runId": root.name,
        "repositoryPath": str(root.parent / "repo"),
        "branch": update.EXPECTED_BRANCH,
        "sourceCommit": COMMIT,
        "target": {"name": "de-portal", "namespace": "digital-employees", "container": "portal"},
        "before": before,
        "baseImage": OLD_IMAGE,
        "baseImageDigest": BASE_DIGEST,
        "targetImage": TARGET_IMAGE,
        "rollout": {"state": "prepared", "changed": False},
    }
    update.write_json(root / "plan.json", plan)
    return plan


def add_oci_blob(archive, data, media_type=None):
    import hashlib
    import tarfile

    digest = hashlib.sha256(data).hexdigest()
    info = tarfile.TarInfo(f"blobs/sha256/{digest}")
    info.size = len(data)
    archive.addfile(info, io.BytesIO(data))
    result = {"digest": f"sha256:{digest}", "size": len(data)}
    if media_type:
        result["mediaType"] = media_type
    return result


def create_oci_archive(path, arch="amd64"):
    import hashlib
    import tarfile

    layer_buffer = io.BytesIO()
    with tarfile.open(fileobj=layer_buffer, mode="w") as layer:
        data = b"base-layer"
        info = tarfile.TarInfo("etc/base")
        info.size = len(data)
        layer.addfile(info, io.BytesIO(data))
    layer_data = layer_buffer.getvalue()
    with tarfile.open(path, "w") as archive:
        config = {
            "os": "linux", "architecture": arch,
            "rootfs": {"type": "layers", "diff_ids": ["sha256:" + hashlib.sha256(layer_data).hexdigest()]},
            "config": {"User": "65532:65532", "Entrypoint": ["/old-entrypoint"], "Cmd": []},
            "history": [],
        }
        config_desc = add_oci_blob(archive, json.dumps(config, separators=(",", ":")).encode(), "application/vnd.oci.image.config.v1+json")
        layer_desc = add_oci_blob(archive, layer_data, "application/vnd.oci.image.layer.v1.tar")
        manifest = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": config_desc, "layers": [layer_desc]}
        manifest_desc = add_oci_blob(archive, json.dumps(manifest, separators=(",", ":")).encode(), "application/vnd.oci.image.manifest.v1+json")
        manifest_desc.update({"platform": {"os": "linux", "architecture": arch}, "annotations": {"org.opencontainers.image.ref.name": "base:tag"}})
        for name, value in (("oci-layout", {"imageLayoutVersion": "1.0.0"}), ("index.json", {"schemaVersion": 2, "manifests": [manifest_desc]})):
            data = json.dumps(value, separators=(",", ":")).encode()
            info = tarfile.TarInfo(name)
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))


class VerifySourceTests(unittest.TestCase):
    def test_windows_checkout_conversion_does_not_hide_real_changes(self):
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            update.run(["git", "init", str(repo)])
            update.git(repo, "config", "user.name", "Updater test")
            update.git(repo, "config", "user.email", "updater@example.invalid")
            tracked = repo / "tracked.txt"
            tracked.write_bytes(b"original\r\n")
            update.git(repo, "add", "tracked.txt")
            update.git(repo, "commit", "-m", "test fixture")
            self.assertEqual("", update.git(repo, "status", "--porcelain"))
            tracked.write_bytes(b"changed\r\n")
            self.assertIn("tracked.txt", update.git(repo, "status", "--porcelain"))
            tracked.write_bytes(b"original\r\n")
            (repo / "untracked.txt").write_text("untracked", encoding="utf-8")
            self.assertIn("?? untracked.txt", update.git(repo, "status", "--porcelain", "--untracked-files=all"))

    def test_rejects_wrong_commit_dirty_tree_and_wrong_branch(self):
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp) / "actual-directory"
            repo.mkdir()
            cases = [
                (["feat/knowledge-phase-a", "b" * 40, ""], "HEAD does not match"),
                (["feat/knowledge-phase-a", COMMIT, "?? untracked.txt"], "worktree must be clean"),
                (["main", COMMIT, ""], "branch must be"),
            ]
            for outputs, expected in cases:
                with self.subTest(expected=expected), mock.patch.object(update, "git", side_effect=outputs) as git:
                    with self.assertRaisesRegex(update.UpdateError, expected):
                        update.verify_source(repo, COMMIT)
                    git.assert_called()

    def test_requires_full_sha(self):
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            with self.assertRaisesRegex(update.UpdateError, "full 40-character"):
                update.verify_source(repo, "deadbeef")


class SanitizationTests(unittest.TestCase):
    def test_snapshot_never_keeps_environment_literals_and_preserves_references(self):
        snapshot = update.sanitized_snapshot(deployment())
        serialized = json.dumps(snapshot)
        self.assertNotIn("sensitive-literal-never-persist", serialized)
        self.assertNotIn("annotation-token-never-persist", serialized)
        self.assertNotIn("sensitive-last-applied-config", serialized)
        self.assertNotIn("secret-zone-label", serialized)
        self.assertIn("DATABASE_URL", snapshot["container"]["environmentNames"])
        self.assertIn({"kind": "secretKey", "name": "portal-config", "key": "deployment-id"}, snapshot["container"]["environmentReferences"])
        self.assertEqual([{"kind": "secret", "name": "de-platform-creds"}], snapshot["container"]["envFromReferences"])
        self.assertEqual("portal-data-pvc", snapshot["volumes"][1]["persistentVolumeClaim"])
        self.assertEqual({"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}, snapshot["nodeSelector"])

    def test_snapshot_refuses_unknown_update_marker(self):
        item = deployment()
        item["spec"]["template"]["metadata"]["annotations"][update.UPDATE_ANNOTATION] = "bearer secret"
        with self.assertRaises(update.UpdateError):
            update.sanitized_snapshot(item)

    def test_write_json_fsyncs_file_and_parent_directory(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "run" / "record.json"
            with mock.patch.object(update.os, "fsync", wraps=update.os.fsync) as fsync:
                update.write_json(path, {"safe": True})
            self.assertGreaterEqual(fsync.call_count, 2)
            self.assertEqual({"safe": True}, update.read_json(path))

    def test_rejects_unknown_container_layout(self):
        with self.assertRaises(update.UpdateError):
            update.sanitized_snapshot(deployment(containers=[{"name": "portal"}, {"name": "sidecar"}]))
        with self.assertRaises(update.UpdateError):
            update.sanitized_snapshot(deployment(containers=[{"name": "unexpected", "image": OLD_IMAGE}]))


class PlanTests(unittest.TestCase):
    def test_plan_is_pinned_sanitized_and_does_not_patch_cluster(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = root / "repo"
            repo.mkdir()
            run_root = root / "run-1"
            with mock.patch.object(update, "verify_source", return_value=COMMIT), \
                    mock.patch.object(update, "verify_linux_amd64_nodes"), \
                    mock.patch.object(update, "kubectl", return_value=json.dumps(deployment())), \
                    mock.patch.object(update, "image_cache", return_value={OLD_IMAGE}), \
                    mock.patch.object(update, "image_digest", return_value=BASE_DIGEST), \
                    mock.patch.object(update, "export_base") as export, \
                    mock.patch.object(update, "patch_image") as patch_image:
                plan = update.make_plan(repo, COMMIT, run_root)
            self.assertEqual(COMMIT, plan["sourceCommit"])
            self.assertEqual(BASE_DIGEST, plan["baseImageDigest"])
            self.assertEqual(OLD_IMAGE, plan["before"]["container"]["image"])
            self.assertEqual("planned", plan["rollout"]["state"])
            self.assertEqual(sorted(update.REQUIRED_KNOWLEDGE_ENV), plan["configurationGaps"]["missingEnvironmentVariableNames"])
            self.assertFalse(plan["configurationGaps"]["apiKeySecretValueRead"])
            serialized = (run_root / "plan.json").read_text(encoding="utf-8")
            self.assertNotIn("sensitive-literal-never-persist", serialized)
            export.assert_called_once()
            patch_image.assert_not_called()

    def test_plan_rejects_runroot_inside_source_repo(self):
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp) / "repo"
            repo.mkdir()
            with mock.patch.object(update, "verify_source", return_value=COMMIT):
                with self.assertRaises(update.UpdateError):
                    update.make_plan(repo, COMMIT, repo / "run")

    def test_plan_fails_closed_when_current_image_is_not_cached(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = root / "repo"
            repo.mkdir()
            with mock.patch.object(update, "verify_source", return_value=COMMIT), \
                    mock.patch.object(update, "verify_linux_amd64_nodes"), \
                    mock.patch.object(update, "kubectl", return_value=json.dumps(deployment())), \
                    mock.patch.object(update, "image_cache", return_value=set()), \
                    mock.patch.object(update, "export_base") as export:
                with self.assertRaisesRegex(update.UpdateError, "not present in the local containerd cache"):
                    update.make_plan(repo, COMMIT, root / "run")
            export.assert_not_called()

    def test_plan_fails_closed_on_non_linux_amd64_node(self):
        nodes = {"items": [{"metadata": {"labels": {"kubernetes.io/os": "linux", "kubernetes.io/arch": "arm64"}}, "spec": {}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}]}
        with mock.patch.object(update, "kubectl", side_effect=[json.dumps({"clusters": [{"cluster": {"server": "https://127.0.0.1:6443"}}]}), json.dumps(nodes)]):
            with self.assertRaisesRegex(update.UpdateError, "linux/amd64"):
                update.verify_linux_amd64_nodes()

    def test_fixed_local_kubeconfig_ignores_inherited_kubeconfig(self):
        with mock.patch.dict(update.os.environ, {"KUBECONFIG": "/shared/remote.yaml"}), mock.patch.object(update, "run", return_value="{}") as run:
            update.kubectl("get", "nodes")
        self.assertEqual(update.LOCAL_KUBECONFIG, run.call_args.kwargs["env"]["KUBECONFIG"])

    def test_cluster_guard_requires_loopback_and_single_ready_local_node(self):
        nodes = {"items": [{"metadata": {"labels": {"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}}, "spec": {}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}]}
        for server, node_items, error in (
            ("https://10.0.0.5:6443", nodes["items"], "loopback"),
            ("https://127.0.0.1:6443", nodes["items"] * 2, "exactly one node"),
        ):
            with self.subTest(error=error), mock.patch.object(update, "kubectl", side_effect=[json.dumps({"clusters": [{"cluster": {"server": server}}]}), json.dumps({"items": node_items})]):
                with self.assertRaisesRegex(update.UpdateError, error):
                    update.verify_linux_amd64_nodes()

    def test_docker_short_image_reference_canonicalizes_to_cached_ref(self):
        self.assertEqual("docker.io/zhiyuan/de-portal:77b403f", update.canonical_image_ref("zhiyuan/de-portal:77b403f"))
        self.assertEqual("docker.io/zhiyuan/de-portal:77b403f", update.canonical_image_ref("docker.io/zhiyuan/de-portal:77b403f"))
        self.assertEqual("docker.io/library/nginx:latest", update.canonical_image_ref("nginx"))

    def test_image_digest_matches_canonical_containerd_reference(self):
        table = "REF TYPE DIGEST SIZE PLATFORMS LABELS\ndocker.io/zhiyuan/de-portal:77b403f application/vnd.oci.image.index.v1+json " + BASE_DIGEST + " 100 linux/amd64 -\n"
        with mock.patch.object(update, "run", return_value=table) as run:
            self.assertEqual(BASE_DIGEST, update.image_digest("zhiyuan/de-portal:77b403f"))
        self.assertEqual(["k3s", "ctr", "images", "list"], run.call_args.args[0])


class OCIOverlayTests(unittest.TestCase):
    def test_overlay_preserves_runtime_user_and_adds_only_portal_binary(self):
        import tarfile

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            base, binary, output = root / "base.tar", root / "de-portal", root / "overlay.tar"
            create_oci_archive(base)
            binary.write_bytes(b"compiled-portal")
            update.build_overlay(base, binary, output, TARGET_IMAGE, COMMIT)
            update.verify_oci_platform(output)
            with tarfile.open(output) as archive:
                index = json.load(archive.extractfile("index.json"))
                manifest = update.resolve_oci(archive, index["manifests"][0])
                config_raw = archive.extractfile("blobs/sha256/" + manifest["config"]["digest"].split(":", 1)[1]).read()
                config = json.loads(config_raw)
                self.assertEqual("65532:65532", config["config"]["User"])
                self.assertEqual(["/de-portal"], config["config"]["Entrypoint"])
                self.assertEqual(COMMIT, config["config"]["Labels"]["local.zhiyuan.governance.commit"])
                layer_desc = manifest["layers"][-1]
                layer_file = archive.extractfile("blobs/sha256/" + layer_desc["digest"].split(":", 1)[1])
                with tarfile.open(fileobj=layer_file) as layer:
                    self.assertEqual(b"compiled-portal", layer.extractfile("de-portal").read())

    def test_overlay_rejects_non_amd64_base(self):
        import tarfile

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            base = root / "base.tar"
            create_oci_archive(base, arch="arm64")
            with self.assertRaises(update.UpdateError):
                update.verify_oci_platform(base)


class BuildPreflightTests(unittest.TestCase):
    def test_build_requires_verified_plan_and_windows_compiled_artifact(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            run_root = root / "run-1"
            repo = root / "repo"
            repo.mkdir()
            plan = plan_fixture(run_root)
            plan["repositoryPath"] = str(repo.resolve())
            update.write_json(run_root / "plan.json", plan)
            with mock.patch.object(update, "verify_source", return_value=COMMIT):
                with self.assertRaisesRegex(update.UpdateError, "Windows Go build did not produce"):
                    update.build_binary(repo, COMMIT, run_root)
                binary = run_root / "de-portal"
                binary.write_bytes(b"linux-elf")
                self.assertEqual(binary, update.build_binary(repo, COMMIT, run_root))


class DeploymentTests(unittest.TestCase):
    def test_deploy_requires_database_backup_and_never_changes_other_target(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            with self.assertRaises(update.UpdateError):
                update.deploy(root, confirm_db_backup=False)

    def test_tampered_plan_or_journal_cannot_expand_target(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan = plan_fixture(root)
            plan["target"] = {"name": "operator", "namespace": "default", "container": "operator"}
            update.write_json(root / "plan.json", plan)
            with self.assertRaisesRegex(update.UpdateError, "fixed de-portal"):
                update.validate_plan(update.read_json(root / "plan.json"), root)

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            journal = {"runId": root.name, "uid": "uid-1", "oldImage": OLD_IMAGE, "oldPolicy": "IfNotPresent", "oldMarker": None, "targetImage": "docker.io/zhiyuan-local/operator:tag", "state": "patched"}
            with self.assertRaisesRegex(update.UpdateError, "journal does not match"):
                update.validate_journal(journal, update.read_json(root / "plan.json"), root)

    def test_successful_deploy_patches_only_pinned_portal_and_journals(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            with mock.patch.object(update, "image_cache", return_value={TARGET_IMAGE}), \
                    mock.patch.object(update, "current_target", return_value=deployment()), \
                    mock.patch.object(update, "patch_image") as patch_image, \
                    mock.patch.object(update, "rollout") as rollout:
                update.deploy(root, confirm_db_backup=True)
            patch_image.assert_called_once_with("de-portal", "digital-employees", "portal", TARGET_IMAGE, "Never", root.name, "uid-1", "10")
            rollout.assert_called_once_with("digital-employees")
            self.assertEqual("deployed", update.read_json(root / "journal.json")["state"])

    def test_uid_or_image_change_blocks_deploy(self):
        marker_drift = deployment()
        marker_drift["spec"]["template"]["metadata"]["annotations"][update.UPDATE_ANNOTATION] = "someone-elses-run"
        for changed in (deployment(uid="new-uid"), deployment(image="registry.local/de-portal:someone-else"), marker_drift):
            with self.subTest(uid=changed["metadata"]["uid"], image=changed["spec"]["template"]["spec"]["containers"][0]["image"]), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                plan_fixture(root)
                with mock.patch.object(update, "image_cache", return_value={TARGET_IMAGE}), \
                        mock.patch.object(update, "current_target", return_value=changed), \
                        mock.patch.object(update, "patch_image") as patch_image:
                    with self.assertRaises(update.UpdateError):
                        update.deploy(root, confirm_db_backup=True)
                patch_image.assert_not_called()

    def test_patch_failure_attempts_rollback_without_leaving_target(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            live = [deployment(), deployment()]
            with mock.patch.object(update, "image_cache", return_value={TARGET_IMAGE}), \
                    mock.patch.object(update, "current_target", side_effect=live), \
                    mock.patch.object(update, "patch_image", side_effect=RuntimeError("patch rejected")) as patch_image, \
                    mock.patch.object(update, "rollout"):
                with self.assertRaisesRegex(update.UpdateError, "original Portal image restored"):
                    update.deploy(root, confirm_db_backup=True)
            patch_image.assert_called_once()
            self.assertEqual("already_rolled_back", update.read_json(root / "journal.json")["state"])

    def test_rollout_failure_rolls_back_target(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            target_live = deployment(image=TARGET_IMAGE, resource_version="11")
            target_live["spec"]["template"]["metadata"]["annotations"][update.UPDATE_ANNOTATION] = root.name
            live = [deployment(), target_live]
            with mock.patch.object(update, "image_cache", return_value={TARGET_IMAGE}), \
                    mock.patch.object(update, "current_target", side_effect=live), \
                    mock.patch.object(update, "patch_image") as patch_image, \
                    mock.patch.object(update, "rollout", side_effect=[RuntimeError("new rollout failed"), None]):
                with self.assertRaisesRegex(update.UpdateError, "original Portal image restored"):
                    update.deploy(root, confirm_db_backup=True)
            self.assertEqual(2, patch_image.call_count)
            self.assertEqual(OLD_IMAGE, patch_image.call_args_list[1].args[3])
            self.assertEqual("rolled_back", update.read_json(root / "journal.json")["state"])

    def test_rollback_failure_is_journaled_and_uid_change_blocks_rollback(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            update.write_json(root / "journal.json", {"runId": root.name, "uid": "uid-1", "oldImage": OLD_IMAGE, "oldPolicy": "IfNotPresent", "oldMarker": None, "targetImage": TARGET_IMAGE, "state": "patched"})
            target_live = deployment(image=TARGET_IMAGE)
            target_live["spec"]["template"]["metadata"]["annotations"][update.UPDATE_ANNOTATION] = root.name
            with mock.patch.object(update, "current_target", return_value=target_live), \
                    mock.patch.object(update, "patch_image", side_effect=RuntimeError("rollback patch failed")):
                with self.assertRaises(update.UpdateError):
                    update.rollback_run(root)
            self.assertEqual("rollback_failed", update.read_json(root / "journal.json")["state"])
            with mock.patch.object(update, "current_target", return_value=deployment(image=TARGET_IMAGE, uid="uid-2")):
                with self.assertRaisesRegex(update.UpdateError, "UID changed"):
                    update.rollback_run(root)

    def test_rollback_refuses_target_image_without_this_run_marker(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            update.write_json(root / "journal.json", {"runId": root.name, "uid": "uid-1", "oldImage": OLD_IMAGE, "oldPolicy": "IfNotPresent", "oldMarker": None, "targetImage": TARGET_IMAGE, "state": "patched"})
            live = deployment(image=TARGET_IMAGE)
            live["spec"]["template"]["metadata"]["annotations"][update.UPDATE_ANNOTATION] = "someone-elses-run"
            with mock.patch.object(update, "current_target", return_value=live), mock.patch.object(update, "patch_image") as patch_image:
                with self.assertRaisesRegex(update.UpdateError, "does not belong to this run"):
                    update.rollback_run(root)
            patch_image.assert_not_called()

    def test_already_old_image_requires_matching_policy_and_marker(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            plan_fixture(root)
            update.write_json(root / "journal.json", {"runId": root.name, "uid": "uid-1", "oldImage": OLD_IMAGE, "oldPolicy": "IfNotPresent", "oldMarker": None, "targetImage": TARGET_IMAGE, "state": "patch_attempted"})
            changed_policy = deployment()
            changed_policy["spec"]["template"]["spec"]["containers"][0]["imagePullPolicy"] = "Never"
            with mock.patch.object(update, "current_target", return_value=changed_policy):
                with self.assertRaisesRegex(update.UpdateError, "pull policy or update marker differs"):
                    update.rollback_run(root)

    def test_cli_does_not_accept_an_expanded_target(self):
        with mock.patch("sys.argv", [str(MODULE_PATH), "plan", "--repository-path", "/repo", "--expected-commit", COMMIT, "--run-root", "/run", "--target", "operator"]):
            with contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    update.parse_args()

    def test_patch_payload_contains_only_named_portal_fields_and_preconditions(self):
        with mock.patch.dict(update.os.environ, {"KUBECONFIG": "/shared/remote.yaml"}), mock.patch.object(update, "run") as run:
            update.patch_image("de-portal", "digital-employees", "portal", TARGET_IMAGE, "Never", "run-1", "uid-1", "12")
        args = run.call_args.args[0]
        payload = json.loads(run.call_args.kwargs["input_text"])
        self.assertEqual(["k3s", "kubectl", "-n", "digital-employees", "patch", "deployment", "de-portal", "--type=strategic", "--patch-file=/dev/stdin"], args)
        self.assertEqual(update.LOCAL_KUBECONFIG, run.call_args.kwargs["env"]["KUBECONFIG"])
        self.assertEqual({"uid": "uid-1", "resourceVersion": "12"}, payload["metadata"])
        self.assertEqual([{"name": "portal", "image": TARGET_IMAGE, "imagePullPolicy": "Never"}], payload["spec"]["template"]["spec"]["containers"])
        self.assertEqual({update.UPDATE_ANNOTATION: "run-1"}, payload["spec"]["template"]["metadata"]["annotations"])
        self.assertNotIn("env", json.dumps(payload))

    def test_rollout_uses_fixed_local_kubeconfig_and_rejects_other_namespace(self):
        with mock.patch.dict(update.os.environ, {"KUBECONFIG": "/shared/remote.yaml"}), mock.patch.object(update, "run") as run:
            update.rollout("digital-employees")
        self.assertEqual(["k3s", "kubectl", "-n", "digital-employees", "rollout", "status", "deployment/de-portal", "--timeout=180s"], run.call_args.args[0])
        self.assertEqual(update.LOCAL_KUBECONFIG, run.call_args.kwargs["env"]["KUBECONFIG"])
        with mock.patch.object(update, "run") as run:
            with self.assertRaisesRegex(update.UpdateError, "fixed Portal namespace"):
                update.rollout("default")
        run.assert_not_called()

    def test_linux_test_runner_avoids_shell_interpolation_and_cleans_temp(self):
        with tempfile.TemporaryDirectory(prefix="test path ") as temp:
            binary = Path(temp) / "test binary"
            binary.write_bytes(b"test artifact")
            with mock.patch.object(update.subprocess, "run", return_value=mock.Mock(returncode=0)) as run:
                self.assertEqual(0, update.run_linux_tests(binary))
            args = run.call_args.args[0]
            self.assertTrue(Path(args[0]).is_file() is False)
            self.assertEqual("-test.v", args[1])


if __name__ == "__main__":
    unittest.main()
