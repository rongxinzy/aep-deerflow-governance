"""Exercise Helm's real rendering and fail-closed managed configuration."""
import os
from pathlib import Path
import subprocess
import unittest


class ManagedKnowledgeChartTests(unittest.TestCase):
    def render(self, *settings):
        command = [os.environ.get("HELM_BIN", "helm"), "template", "acceptance",
                   str(Path(__file__).resolve().parents[1] / "charts/zhiyuan-stack"),
                   "--set", "weknora.enabled=false", "--set", "aepStack.enabled=false",
                   "--show-only", "templates/portal.yaml"]
        for setting in settings:
            command += ["--set" if setting.startswith("portal.managedKnowledge.enabled=") else "--set-string", setting]
        return subprocess.run(command, text=True, capture_output=True, check=False)

    def test_disabled_feature_does_not_invent_tenant(self):
        rendered = self.render()
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        self.assertNotIn("PORTAL_KNOWLEDGE_TENANT_ID", rendered.stdout)

    def test_enabled_feature_requires_tenant_and_embedding_model(self):
        # enabled must be a boolean; --set-string applies to the identifiers.
        for settings, error in [((), "tenantID is required"),
                                (("portal.managedKnowledge.tenantID=10000",), "embeddingModelID is required")]:
            result = self.render("portal.managedKnowledge.enabled=true", *settings)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(error, result.stderr)

    def test_rendered_selector_and_managed_identifiers_match_values(self):
        result = self.render("portal.managedKnowledge.enabled=true",
                             "portal.managedKnowledge.tenantID=10000",
                             "portal.managedKnowledge.embeddingModelID=embedding-fixture",
                             "secrets.platformCreds=acceptance-creds")
        self.assertEqual(result.returncode, 0, result.stderr)
        for entry in ['PORTAL_KNOWLEDGE_TENANT_ID, value: "10000"',
                      'PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID, value: "embedding-fixture"',
                      'PORTAL_CREDENTIALS_SECRET, value: "acceptance-creds"',
                      'PORTAL_KNOWLEDGE_API_KEY_SECRET_FIELD, value: "WEKNORA_KNOWLEDGE_API_KEY"']:
            self.assertIn(entry, result.stdout)
        self.assertNotIn("secretKeyRef", result.stdout, "Portal resolves the key server-side through Kubernetes")


if __name__ == "__main__":
    unittest.main()
