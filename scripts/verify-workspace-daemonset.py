#!/usr/bin/env python3
"""Read-only evidence for a real, isolated workspace-agent DaemonSet rollout."""
import argparse
import json
import pathlib
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument('--namespace', required=True)
parser.add_argument('--node', required=True)
parser.add_argument('--registry-prefix', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--registry-digests', required=True, help='JSON image -> allowed manifest digests, obtained independently from Harbor')
args = parser.parse_args()
registry_digests = json.loads(pathlib.Path(args.registry_digests).read_text())
assert args.namespace.startswith('cloud-qa-'), 'acceptance namespace required'


def get(*parts):
    return json.loads(subprocess.check_output(['kubectl', '-n', args.namespace, 'get', *parts, '-o', 'json']))


ds = get('daemonset', 'pw-node-agent')
assert ds['status']['observedGeneration'] >= ds['metadata']['generation']
assert ds['status']['desiredNumberScheduled'] == ds['status']['numberReady'] == 1
pods = get('pods', '-l', 'app=pw-node-agent')['items']
pods = [p for p in pods if not p['metadata'].get('deletionTimestamp')]
assert len(pods) == 1
pod = pods[0]
assert pod['spec']['nodeName'] == args.node
assert any(o['uid'] == ds['metadata']['uid'] for o in pod['metadata']['ownerReferences'])
assert pod['spec']['imagePullSecrets'], 'private registry credentials required'
containers = pod['spec']['initContainers'] + pod['spec']['containers']
statuses = {s['name']: s for s in pod['status']['initContainerStatuses'] + pod['status']['containerStatuses']}
images = []
for container in containers:
    assert container['image'].startswith(args.registry_prefix + '/')
    assert container['imagePullPolicy'] == 'Always'
    status = statuses[container['name']]
    assert '@sha256:' in status['imageID'], 'resolved registry digest required'
    assert status['imageID'].rsplit('@', 1)[1] in registry_digests[container['image']], 'runtime digest differs from Harbor'
    if container['name'].startswith('template-materialize-'):
        assert status['state']['terminated']['exitCode'] == 0
    else:
        assert status['ready'] and 'running' in status['state']
    images.append({'container': container['name'], 'image': container['image'], 'imageID': status['imageID']})
assert len(pod['spec']['initContainers']) >= 2, 'both pinned templates must materialize'
events = get('events')['items']
events = [e for e in events if e.get('involvedObject', {}).get('uid') == pod['metadata']['uid']]
for image in images:
    assert any(e['reason'] == 'Pulled' and image['image'] in e.get('message', '') for e in events), 'successful pull event required'
result = {'passed': True, 'namespace': args.namespace, 'node': args.node, 'daemonsetGeneration': ds['metadata']['generation'], 'pod': pod['metadata']['name'], 'images': images, 'events': [{'reason': e['reason'], 'message': e.get('message', '')} for e in events]}
pathlib.Path(args.output).write_text(json.dumps(result, indent=2))
print(json.dumps({'passed': True, 'pod': result['pod'], 'images': images}))
