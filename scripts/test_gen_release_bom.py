"""Unit tests for scripts/gen-release-bom.py (no registry access needed)."""
import contextlib
import datetime as dt
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest

import yaml

SCRIPT = Path(__file__).resolve().parent / "gen-release-bom.py"
spec = importlib.util.spec_from_file_location("gen_release_bom", SCRIPT)
bom = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bom)

DIGEST = "sha256:" + "a" * 64
OTHER = "sha256:" + "b" * 64
MANIFEST = f"""
apiVersion: apps/v1
kind: Deployment
metadata: {{name: de-operator, namespace: digital-employees}}
spec:
  template:
    spec:
      initContainers:
        - {{name: wait, image: "harbor.zhiyuan.local:30443/zhiyuan/busybox:1.36"}}
      containers:
        - name: operator
          image: "harbor.zhiyuan.local:30443/zhiyuan/de-operator:0.21.0@{DIGEST}"
          env:
            - {{name: CONVERSATION_INIT_IMAGE, value: "harbor.zhiyuan.local:30443/zhiyuan/postgres:17.6-alpine"}}
            - {{name: OTHER, value: "not-an-image"}}
            - name: SECRET_IMAGE
              valueFrom: {{secretKeyRef: {{name: x, key: y}}}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {{name: pg-backup, namespace: aep-system}}
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - {{name: dump, image: "harbor.zhiyuan.local:30443/zhiyuan/postgres:17.6-alpine"}}
---
apiVersion: v1
kind: ConfigMap
metadata: {{name: ignored}}
data: {{image: "nope:1"}}
"""


def collect(text=MANIFEST):
    return bom.collect(yaml.safe_load_all(text))


class SplitImageTests(unittest.TestCase):
    def test_registry_port_is_not_a_tag(self):
        self.assertEqual(bom.split_image("harbor.local:30443/zhiyuan/app"),
                         {"image": "harbor.local:30443/zhiyuan/app", "repository": "harbor.local:30443/zhiyuan/app",
                          "tag": "", "digest": ""})

    def test_tag_and_digest(self):
        parts = bom.split_image(f"ghcr.io/o/app:1.2.3@{DIGEST}")
        self.assertEqual((parts["repository"], parts["tag"], parts["digest"]), ("ghcr.io/o/app", "1.2.3", DIGEST))
        parts = bom.split_image(f"ghcr.io/o/app@{DIGEST}")
        self.assertEqual((parts["tag"], parts["digest"]), ("", DIGEST))

    def test_invalid_digest(self):
        with self.assertRaises(ValueError):
            bom.split_image("ghcr.io/o/app:1@sha256:xyz")


class CollectTests(unittest.TestCase):
    def test_collects_pods_cronjobs_and_env_images(self):
        entries = collect()
        by_image = {e["image"]: e for e in entries}
        self.assertEqual(len(entries), 3)
        postgres = by_image["harbor.zhiyuan.local:30443/zhiyuan/postgres:17.6-alpine"]
        self.assertEqual(postgres["sources"], [
            "CronJob/aep-system/pg-backup:dump",
            "Deployment/digital-employees/de-operator:operator:env:CONVERSATION_INIT_IMAGE",
        ])
        operator = by_image[f"harbor.zhiyuan.local:30443/zhiyuan/de-operator:0.21.0@{DIGEST}"]
        self.assertEqual((operator["tag"], operator["digest"]), ("0.21.0", DIGEST))
        self.assertNotIn("nope:1", by_image)

    def test_resolve_fills_only_missing_and_reports_errors(self):
        entries = collect()
        calls = []

        def resolver(image):
            calls.append(image)
            if "busybox" in image:
                raise RuntimeError("cannot resolve busybox")
            return OTHER

        errors = bom.resolve(entries, resolver)
        self.assertEqual(errors, ["cannot resolve busybox"])
        self.assertEqual(len(calls), 2)  # the pinned operator image is not looked up
        resolved = [e for e in entries if e.get("resolved")]
        self.assertEqual([e["digest"] for e in resolved], [OTHER])

    def test_resolver_garbage_is_rejected(self):
        entries = collect()
        errors = bom.resolve(entries, lambda image: "latest")
        self.assertEqual(len(errors), 2)

    def test_bom_and_markdown(self):
        doc = bom.build_bom(collect(), {"name": "zhiyuan-stack", "version": "0.1.0", "appVersion": ""}, "abc",
                            dt.datetime(2026, 10, 17, 8, 0, tzinfo=dt.timezone(dt.timedelta(hours=8))))
        self.assertEqual(doc["generatedAt"], "2026-10-17T00:00:00Z")
        self.assertEqual(doc["summary"], {"images": 3, "digestPinned": 1})
        markdown = bom.to_markdown(doc)
        self.assertIn("# 发版 BOM: zhiyuan-stack 0.1.0", markdown)
        self.assertIn(f"`{DIGEST}`", markdown)
        self.assertIn("⚠️ 未固定", markdown)


class MainTests(unittest.TestCase):
    def run_main(self, *args, resolver=None):
        with tempfile.TemporaryDirectory() as tmp:
            manifest = Path(tmp) / "m.yaml"
            manifest.write_text(MANIFEST)
            out, err = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                code = bom.main(["--manifest", str(manifest), *args], resolver=resolver or (lambda i: OTHER))
            return code, out.getvalue(), err.getvalue()

    def test_json_output(self):
        code, out, _ = self.run_main()
        self.assertEqual(code, 0)
        data = json.loads(out)
        self.assertEqual(data["schema"], "zhiyuan-release-bom/v1")
        self.assertEqual(data["chart"]["name"], "zhiyuan-stack")

    def test_require_digest(self):
        code, _, err = self.run_main("--require-digest")
        self.assertEqual(code, 1)
        self.assertIn("busybox:1.36", err)
        code, out, _ = self.run_main("--require-digest", "--resolve")
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out)["summary"]["digestPinned"], 3)


@unittest.skipUnless(shutil.which(os.environ.get("HELM_BIN", "helm")), "helm not installed")
class ChartRenderTests(unittest.TestCase):
    def test_default_chart_render(self):
        hardening = importlib.util.spec_from_file_location("hardening", SCRIPT.parent / "test_hardening_chart.py")
        module = importlib.util.module_from_spec(hardening)
        hardening.loader.exec_module(module)
        BASE = module.BASE  # noqa: N806 - placeholder-free values shared with the chart tests
        text = bom.render_chart(bom.CHART, [], BASE + ["weknora.enabled=false"], os.environ.get("HELM_BIN", "helm"))
        repos = {e["repository"].rsplit("/", 1)[-1] for e in bom.collect(yaml.safe_load_all(text))}
        self.assertIn("de-operator", repos)
        self.assertIn("de-portal", repos)


if __name__ == "__main__":
    unittest.main()
