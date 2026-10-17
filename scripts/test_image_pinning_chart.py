"""Render the zhiyuan-stack chart and assert the image pinning contract.

Covers: optional repo:tag@digest pins through the image helpers, digest
format validation, the Harbor-sourced db-init image handed to the operator,
and the ban on floating WeKnora frontend/docreader tags.
"""
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_hardening_chart import find, objects, render  # noqa: E402

DIGEST = "sha256:" + "0" * 63 + "1"
WEKNORA = [
    "weknora.enabled=true",
    "weknora.secrets.dbPassword=a",
    "weknora.secrets.redisPassword=b",
    "weknora.secrets.jwtSecret=c",
    "weknora.secrets.systemAesKey=d",
    "weknora.mcp.apiKey=k",
    "weknora.mcp.serverAuthToken=s",
]
PINNED = ["weknora.frontend.image.tag=v0.8.0", "weknora.docreader.image.tag=v0.8.0"]


def images(docs):
    found = []
    for doc in docs:
        spec = doc.get("spec", {})
        pod = spec.get("jobTemplate", {}).get("spec", {}).get("template", {}).get("spec") \
            or spec.get("template", {}).get("spec", {})
        for key in ("containers", "initContainers"):
            found += [c["image"] for c in pod.get(key, [])]
    return found


class ImagePinningTests(unittest.TestCase):
    def ok(self, *settings, sets=()):
        result = render(*settings, sets=sets)
        self.assertEqual(result.returncode, 0, result.stderr)
        return objects(result)

    def fails(self, *settings, sets=(), message=""):
        result = render(*settings, sets=sets)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(message, result.stderr)

    def operator_env(self, docs):
        container = find(docs, "Deployment", "de-operator")[0]["spec"]["template"]["spec"]["containers"][0]
        return {e["name"]: e.get("value") for e in container["env"]}

    def test_default_render_has_no_digest(self):
        self.assertFalse([i for i in images(self.ok()) if "@" in i])

    def test_digest_pins_harbor_image(self):
        docs = self.ok(f"images.operator.digest={DIGEST}")
        self.assertIn(f"harbor.zhiyuan.local:30443/zhiyuan/de-operator:0.21.0@{DIGEST}", images(docs))

    def test_digest_pins_raw_image(self):
        docs = self.ok(f"channelGateway.image.digest={DIGEST}", sets=["channelGateway.enabled=true"])
        self.assertTrue([i for i in images(docs) if i.endswith("@" + DIGEST)])

    def test_invalid_digest_fails(self):
        self.fails("images.portal.digest=sha256:abc", message="must look like sha256:<64 hex chars>")

    def test_operator_gets_harbor_db_init_image(self):
        env = self.operator_env(self.ok())
        self.assertEqual(env["CONVERSATION_INIT_IMAGE"], "harbor.zhiyuan.local:30443/zhiyuan/postgres:17.6-alpine")
        env = self.operator_env(self.ok(f"images.conversationInit.digest={DIGEST}"))
        self.assertTrue(env["CONVERSATION_INIT_IMAGE"].endswith("17.6-alpine@" + DIGEST))

    def test_weknora_tags_are_required(self):
        self.fails(*WEKNORA, message="weknora.frontend.image.tag must be pinned")
        self.fails(*WEKNORA, PINNED[1], "weknora.frontend.image.tag=latest", message='got "latest"')
        self.fails(*WEKNORA, PINNED[0], "weknora.docreader.image.tag=<deployed-weknora-docreader-tag>",
                   message="placeholder")

    def test_weknora_pinned_tag_or_digest(self):
        rendered = images(self.ok(*WEKNORA, *PINNED))
        self.assertIn("harbor.zhiyuan.local:30443/zhiyuan/weknora-ui:v0.8.0", rendered)
        self.assertIn("harbor.zhiyuan.local:30443/zhiyuan/weknora-docreader:v0.8.0", rendered)
        rendered = images(self.ok(*WEKNORA, PINNED[1], f"weknora.frontend.image.digest={DIGEST}"))
        self.assertIn(f"harbor.zhiyuan.local:30443/zhiyuan/weknora-ui@{DIGEST}", rendered)


if __name__ == "__main__":
    unittest.main()
