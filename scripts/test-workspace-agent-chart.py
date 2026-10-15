#!/usr/bin/env python3
"""Helm/PyYAML contract checks for private template pulls and namespace isolation."""
import pathlib
import subprocess
import unittest
import yaml

CHART = pathlib.Path(__file__).resolve().parents[1] / 'charts/zhiyuan-stack'


def render(namespace, *overrides):
    args = ['helm', 'template', 'acceptance', str(CHART), '--namespace', namespace,
            '--set', 'aepStack.enabled=false', '--set', 'weknora.enabled=false',
            '--set', 'workspaceAgent.enabled=true', '--set', f'global.namespaces.main={namespace}',
            '--show-only', 'templates/workspace-agent.yaml',
            '--show-only', 'templates/workspace-agent-rbac.yaml']
    for item in overrides:
        args += ['--set', item]
    return list(yaml.safe_load_all(subprocess.check_output(args)))


class WorkspaceAgentChartTest(unittest.TestCase):
    def test_private_templates_share_pod_credentials(self):
        docs = render('cloud-qa-chart', 'workspaceAgent.imagePullSecrets[0].name=harbor-pull',
                      'workspaceAgent.imagePullPolicy=Always',
                      'workspaceAgent.resources.limits.memory=12Gi',
                      'workspaceAgent.additionalTemplates[0].repository=qa/rootfs',
                      'workspaceAgent.additionalTemplates[0].tag=qa-v2')
        pod = next(d for d in docs if d['kind'] == 'DaemonSet')['spec']['template']['spec']
        self.assertEqual(pod['imagePullSecrets'], [{'name': 'harbor-pull'}])
        self.assertEqual(len(pod['initContainers']), 2)
        self.assertEqual(pod['containers'][0]['resources']['limits']['memory'], '12Gi')
        for container in pod['initContainers'] + pod['containers']:
            self.assertEqual(container['imagePullPolicy'], 'Always')

    def test_default_pull_behavior(self):
        pod = next(d for d in render('cloud-qa-chart') if d['kind'] == 'DaemonSet')['spec']['template']['spec']
        self.assertNotIn('imagePullSecrets', pod)
        self.assertNotIn('limits', pod['containers'][0]['resources'])
        for container in pod['initContainers'] + pod['containers']:
            self.assertEqual(container['imagePullPolicy'], 'IfNotPresent')

    def test_cluster_bindings_do_not_replace_another_namespace(self):
        names = []
        for namespace in ['cloud-qa-chart', 'digital-employees']:
            docs = render(namespace)
            role = next(d for d in docs if d['kind'] == 'ClusterRole')
            binding = next(d for d in docs if d['kind'] == 'ClusterRoleBinding')
            self.assertEqual(binding['roleRef']['name'], role['metadata']['name'])
            self.assertEqual(binding['subjects'][0]['namespace'], namespace)
            names.append(binding['metadata']['name'])
        self.assertNotEqual(*names)


if __name__ == '__main__':
    unittest.main()
