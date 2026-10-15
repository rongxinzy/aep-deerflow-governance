#!/usr/bin/env python3
"""Audit-shipper contract tests (no cluster, no ES: HTTP doubles only).

Covers the P2-3 review findings: every paginated source must consume
`nextCursor` to the end, bulk responses are inspected item by item with
transient/permanent classification, an unresolved failure must exit non-zero
without printing OK, progress is checkpointed only after the page was
acknowledged, and each data source is mapped with the field names of the
interface that is actually requested.

Run: python -m unittest scripts/test_audit_shipper.py
"""
import contextlib
import importlib.util
import io
import json
import os
import re
import sys
import unittest
import urllib.error
import urllib.parse
from pathlib import Path
from unittest.mock import patch

REPO = Path(__file__).resolve().parents[1]
SCRIPT = REPO / 'charts/zhiyuan-stack/files/ship.py'
TEMPLATE = REPO / 'charts/zhiyuan-stack/templates/observability-audit.yaml'
K3S_MANIFEST = REPO / 'k3s/audit-shipper.yaml'

# Retries must not sleep in tests; the module reads this at import time.
os.environ['AUDIT_SHIPPER_RETRY_SLEEP'] = '0'
os.environ['AUDIT_SHIPPER_RUN_ID'] = 'test-run'


def load_module():
    spec = importlib.util.spec_from_file_location('audit_ship_under_test', SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


SHIP = load_module()


class Response:
    def __init__(self, body, status=200):
        self.status = status
        self._body = body if isinstance(body, bytes) else json.dumps(body).encode()

    def read(self):
        return self._body


# List field of each audited endpoint; an unconfigured route answers with an
# empty page, so a test only declares the source it is about.
LIST_KEYS = {
    '/aep/v1/admin/audit/operations': 'items',
    '/aep/v1/admin/audit/authentication': 'items',
    '/aep/v1/admin/control-events': 'items',
    '/aep/v1/admin/events': 'items',
    '/aep/v1/admin/users': 'items',
    '/aep/v1/admin/models': 'models',
    '/api/v1/employees': 'employees',
    '/api/v1/requests': 'requests',
}


def telemetry_event(event_id, **overrides):
    item = {
        'eventId': event_id,
        'userId': 'actor-1',
        'sessionId': 'session-1',
        'type': 'skill.updated',
        'resourceType': 'skill',
        'resourceId': 'skill-1',
        'result': 'failure',
        'data': {'reason': 'revision conflict'},
        'occurredAt': '2026-10-01T00:00:00Z',
        'receivedAt': '2026-10-01T00:00:05Z',
    }
    item.update(overrides)
    return item


class FakeWorld:
    """AEP/portal/ES doubles: routed by path, with per-document bulk verdicts."""

    def __init__(self, fault=None):
        self.requests = []  # (method, path, query)
        self.batches = []  # [[(index, doc_id, doc), ...], ...] in bulk order
        self.stored = {}  # (index, doc_id) -> doc
        self.checkpoints = {}  # source -> cursor
        self.pages = {}  # path -> [body, body, ...] (last body repeats)
        self.faults = {}  # path -> HTTP status raised for that route
        self.bulk_fault = fault  # (index, doc_id, doc) -> (status, error|None)
        self.checkpoint_status = 201

    # -- transport ---------------------------------------------------------
    def transport(self, request, timeout=None, **kwargs):
        parsed = urllib.parse.urlparse(request.full_url)
        self.requests.append((request.method, parsed.path, parsed.query))
        if parsed.path in self.faults:
            raise urllib.error.HTTPError(request.full_url, self.faults[parsed.path],
                                         'injected', None, None)
        if parsed.path.endswith('/password/login'):
            return Response({'accessToken': 'test-token'})
        if parsed.path == '/_bulk':
            return self.bulk(request)
        if parsed.path.startswith(f'/{SHIP.CHECKPOINT_INDEX}/_doc/'):
            source = urllib.parse.unquote(parsed.path.rsplit('/', 1)[1])
            source = source.split(':', 1)[-1]  # <deployment>:<source>
            if source not in self.checkpoints:
                return Response({'found': False})
            return Response({'found': True, '_source': {'cursor': self.checkpoints[source]}})
        if parsed.path in self.faults:
            raise urllib.error.HTTPError(request.full_url, self.faults[parsed.path],
                                         'injected', None, None)
        if parsed.path in self.pages:
            queue = self.pages[parsed.path]
            return Response(queue.pop(0) if len(queue) > 1 else queue[0])
        if parsed.path in LIST_KEYS:
            return Response({LIST_KEYS[parsed.path]: []})
        raise AssertionError(f'unexpected request: {request.method} {request.full_url}')

    def bulk(self, request):
        lines = request.data.decode().splitlines()
        entries = []
        items = []
        for number in range(0, len(lines), 2):
            meta = json.loads(lines[number])['index']
            doc = json.loads(lines[number + 1])
            index, doc_id = meta['_index'], meta.get('_id')
            entries.append((index, doc_id, doc))
            if index == SHIP.CHECKPOINT_INDEX:
                self.checkpoints[doc['source']] = doc['cursor']
                status, error = self.checkpoint_status, None
            elif self.bulk_fault:
                status, error = self.bulk_fault(index, doc_id, doc)
            else:
                status, error = 201, None
            if error is None and isinstance(status, int) and 200 <= status < 300:
                self.stored[(index, doc_id)] = doc
            item = {'index': {'_id': doc_id, 'status': status}}
            if error is not None:
                item['index']['error'] = error
            items.append(item)
        self.batches.append(entries)
        return Response({'took': 1, 'errors': any('error' in i['index'] for i in items), 'items': items})

    # -- assertions helpers ------------------------------------------------
    def docs_of(self, audit_type):
        return [doc for doc in self.stored.values() if doc.get('audit_type') == audit_type]

    def request_queries(self, path):
        return [query for _, request_path, query in self.requests if request_path == path]

    def audit_batches(self):
        return [[entry for entry in batch if entry[0] != SHIP.CHECKPOINT_INDEX] for batch in self.batches]


class ShipperTestCase(unittest.TestCase):
    def run_shipper(self, world):
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch('urllib.request.urlopen', side_effect=world.transport), \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = SHIP.main()
        return code, stdout.getvalue() + stderr.getvalue()


class Pagination(ShipperTestCase):
    def test_consumes_next_cursor_until_pages_drain(self):
        world = FakeWorld()
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1')], 'nextCursor': 'page-2'},
            {'items': [telemetry_event('event-2')], 'nextCursor': 'page-3'},
            {'items': [telemetry_event('event-3')], 'nextCursor': None},
        ]
        code, output = self.run_shipper(world)
        self.assertEqual(code, 0, output)
        queries = world.request_queries('/aep/v1/admin/events')
        self.assertEqual(len(queries), 3, queries)
        self.assertNotIn('cursor=', queries[0])
        self.assertIn('cursor=page-2', queries[1])
        self.assertIn('cursor=page-3', queries[2])
        self.assertEqual(sorted(doc['event_id'] for doc in world.docs_of('telemetry_event')),
                         ['event-1', 'event-2', 'event-3'])
        self.assertIn('OK: 3/3 audit docs indexed', output)

    def test_every_paginated_source_consumes_its_cursor(self):
        world = FakeWorld()
        world.pages['/aep/v1/admin/users'] = [
            {'items': [{'id': 'user-1', 'username': 'u1', 'displayName': 'U1'}], 'nextCursor': 'user-9'},
            {'items': [{'id': 'user-2', 'username': 'u2', 'displayName': 'U2'}], 'nextCursor': None},
        ]
        world.pages['/aep/v1/admin/models'] = [
            {'models': [{'id': 'model-1', 'displayName': 'M1', 'healthStatus': 'unhealthy'}], 'nextCursor': 'model-9'},
            {'models': [{'id': 'model-2', 'displayName': 'M2'}], 'nextCursor': None},
        ]
        world.pages['/aep/v1/admin/control-events'] = [
            {'items': [{'eventId': 'ce-1', 'type': 'session.control', 'state': 'pending'}], 'nextCursor': 'ce-cursor-2'},
            {'items': [{'eventId': 'ce-2', 'type': 'session.control', 'state': 'succeeded'}], 'nextCursor': None},
        ]
        code, output = self.run_shipper(world)
        self.assertEqual(code, 0, output)
        self.assertIn('cursor=user-9', world.request_queries('/aep/v1/admin/users')[1])
        self.assertIn('cursor=model-9', world.request_queries('/aep/v1/admin/models')[1])
        self.assertIn('cursor=ce-cursor-2', world.request_queries('/aep/v1/admin/control-events')[1])
        self.assertEqual(len(world.docs_of('user_snapshot')), 2)
        self.assertEqual(len(world.docs_of('model_health_snapshot')), 2)
        self.assertEqual(len(world.docs_of('control_event')), 2)

    def test_cursor_that_does_not_advance_fails_instead_of_looping(self):
        world = FakeWorld()
        world.checkpoints['telemetry_events'] = 'page-2'
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-2')], 'nextCursor': 'page-2'}]
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0, output)
        self.assertIn('refusing to re-read the same page', output)
        self.assertEqual(len(world.request_queries('/aep/v1/admin/events')), 1)

    def test_resume_starts_from_the_confirmed_checkpoint(self):
        world = FakeWorld()
        world.checkpoints['telemetry_events'] = 'page-9'
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-9')], 'nextCursor': None}]
        code, output = self.run_shipper(world)
        self.assertEqual(code, 0, output)
        queries = world.request_queries('/aep/v1/admin/events')
        self.assertIn('cursor=page-9', queries[0])
        self.assertEqual(world.checkpoints['telemetry_events'], '')


class BulkHandling(ShipperTestCase):
    def test_bulk_rejecting_every_item_exits_nonzero_without_ok(self):
        world = FakeWorld(fault=lambda index, doc_id, doc: (
            429, {'type': 'es_rejected_execution_exception', 'reason': 'queue full'}))
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1')], 'nextCursor': None}]
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0)
        self.assertNotIn('OK:', output)
        self.assertIn('FAILED: 0/1 audit docs indexed', output)
        self.assertIn('es_rejected_execution_exception', output)
        self.assertIn('telemetry_event:demo:event-1', output)
        self.assertEqual(world.docs_of('telemetry_event'), [])
        self.assertEqual(world.checkpoints.get('telemetry_events'), None)

    def test_partial_rejection_retries_only_the_rejected_documents(self):
        world = FakeWorld()
        flaky = {'event-2'}

        def policy(index, doc_id, doc):
            if doc.get('event_id') in flaky:
                flaky.discard(doc['event_id'])
                return 429, {'type': 'es_rejected_execution_exception', 'reason': 'queue full'}
            return 201, None

        world.bulk_fault = policy
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1'), telemetry_event('event-2')], 'nextCursor': None}]
        code, output = self.run_shipper(world)
        self.assertEqual(code, 0, output)
        self.assertIn('OK: 2/2 audit docs indexed', output)
        self.assertIn('retrying those only', output)
        batches = world.audit_batches()
        self.assertEqual([len(batch) for batch in batches], [2, 1])
        self.assertEqual([doc['event_id'] for _, _, doc in batches[1]], ['event-2'])
        self.assertEqual(len(world.docs_of('telemetry_event')), 2)

    def test_permanent_rejection_names_the_document_and_holds_progress(self):
        world = FakeWorld(fault=lambda index, doc_id, doc: (
            (400, {'type': 'mapper_parsing_exception', 'reason': 'failed to parse field'})
            if doc.get('event_id') == 'bad-event' else (201, None)))
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1')], 'nextCursor': 'page-2'},
            {'items': [telemetry_event('bad-event')], 'nextCursor': 'page-3'},
        ]
        world.pages['/aep/v1/admin/control-events'] = [
            {'items': [{'eventId': 'ce-1', 'type': 'session.control'}], 'nextCursor': None}]
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0)
        self.assertNotIn('OK:', output)
        self.assertIn('mapper_parsing_exception', output)
        self.assertIn('telemetry_event:demo:bad-event', output)
        # The rejected page never became progress, and it was not retried blindly.
        self.assertEqual(world.checkpoints.get('telemetry_events'), 'page-2')
        self.assertEqual(len([batch for batch in world.audit_batches()
                              if any(doc.get('event_id') == 'bad-event' for _, _, doc in batch)]), 1)
        # Other sources keep being archived; the run still fails loudly.
        self.assertEqual(len(world.docs_of('control_event')), 1)

    def test_bulk_response_that_does_not_match_the_request_fails(self):
        world = FakeWorld()
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1'), telemetry_event('event-2')], 'nextCursor': None}]
        world.bulk_fault = lambda index, doc_id, doc: (201, None)
        original = world.bulk

        def truncated(request):
            response = original(request)
            body = json.loads(response.read())
            body['items'] = body['items'][:1]
            return Response(body)

        world.bulk = truncated
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0)
        self.assertIn('cannot tell which documents were indexed', output)

    def test_unpersistable_checkpoint_fails_the_run(self):
        world = FakeWorld()
        world.checkpoint_status = 503
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1')], 'nextCursor': 'page-2'},
            {'items': [], 'nextCursor': None},
        ]
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0)
        self.assertNotIn('OK:', output)
        self.assertIn(SHIP.CHECKPOINT_INDEX, output)


class SourceFailures(ShipperTestCase):
    def test_unreadable_source_exits_nonzero_and_other_sources_still_ship(self):
        world = FakeWorld()
        world.faults['/aep/v1/admin/audit/operations'] = 500
        world.pages['/aep/v1/admin/events'] = [
            {'items': [telemetry_event('event-1')], 'nextCursor': None}]
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0)
        self.assertNotIn('OK:', output)
        self.assertIn('admin_audit', output)
        self.assertIn('HTTP 500', output)
        self.assertIn('after 3 attempts', output)
        self.assertEqual(len(world.request_queries('/aep/v1/admin/audit/operations')), 3)
        self.assertEqual(len(world.docs_of('telemetry_event')), 1)

    def test_missing_list_field_is_a_failure_not_an_empty_result(self):
        world = FakeWorld()
        world.pages['/aep/v1/admin/events'] = [{'unexpected': 'shape'}]
        code, output = self.run_shipper(world)
        self.assertNotEqual(code, 0)
        self.assertIn("without a 'items' field", output)

    def test_failed_login_exits_nonzero(self):
        world = FakeWorld()
        world.faults['/aep/v1/auth/password/login'] = 401
        code, output = self.run_shipper(world)
        self.assertEqual(code, 1)
        self.assertIn('AEP login failed', output)
        self.assertNotIn('OK:', output)


class FieldMapping(ShipperTestCase):
    def setUp(self):
        self.world = FakeWorld()
        self.world.pages.update({
            '/aep/v1/admin/audit/operations': [{'items': [{
                'cursor': '42', 'actorUserId': 'admin-1', 'action': 'model.disable',
                'resourceType': 'model', 'resourceId': 'model-1', 'result': 'success',
                'reason': None, 'changes': {'enabled': True}, 'createdAt': '2026-10-02T01:02:03Z',
            }], 'nextCursor': None}],
            '/aep/v1/admin/audit/authentication': [{'items': [{
                'cursor': '7', 'userId': 'user-1', 'eventType': 'login.failed',
                'outcome': 'failure', 'reason': 'bad password', 'sourceHash': 'abc123',
                'createdAt': '2026-10-02T04:05:06Z',
            }], 'nextCursor': None}],
            '/aep/v1/admin/control-events': [{'items': [{
                'eventId': 'ce-1', 'type': 'session.control', 'state': 'pending',
                'createdBy': 'admin-1', 'createdAt': '2026-10-02T07:08:09Z',
                'scope': {'type': 'session', 'id': 's-1'}, 'task': {'type': 'restart'},
            }], 'nextCursor': None}],
            '/aep/v1/admin/events': [{'items': [telemetry_event('event-1')], 'nextCursor': None}],
            '/api/v1/requests': [{'requests': [{
                'id': 'req-1', 'employeeName': 'sales-helper', 'owner': 'user-1',
                'displayName': '销售助手', 'state': 'rejected', 'reason': 'not needed',
                'createdAt': '2026-10-02T10:11:12Z',
            }]}],
        })
        self.code, self.output = self.run_shipper(self.world)
        self.assertEqual(self.code, 0, self.output)

    def test_telemetry_maps_the_telemetry_interface(self):
        doc = self.world.docs_of('telemetry_event')[0]
        self.assertEqual(doc['audit_source'], '/aep/v1/admin/events')
        self.assertEqual(doc['actor_user_id'], 'actor-1')
        self.assertEqual(doc['@timestamp'], '2026-10-01T00:00:00Z')
        self.assertEqual(doc['event_type'], 'skill.updated')
        self.assertEqual(doc['result'], 'failure')
        self.assertEqual(doc['resource_type'], 'skill')
        self.assertEqual(doc['data'], {'reason': 'revision conflict'})
        # Control-event names must not be invented for a telemetry response.
        self.assertNotIn('created_by', doc)
        self.assertNotIn('state', doc)

    def test_admin_audit_maps_the_operations_interface(self):
        doc = self.world.docs_of('admin_audit')[0]
        self.assertEqual(doc['audit_source'], '/aep/v1/admin/audit/operations')
        self.assertEqual(doc['actor_user_id'], 'admin-1')
        self.assertEqual(doc['action'], 'model.disable')
        self.assertEqual(doc['resource_type'], 'model')
        self.assertEqual(doc['result'], 'success')
        self.assertEqual(doc['changes'], {'enabled': True})
        self.assertEqual(doc['@timestamp'], '2026-10-02T01:02:03Z')
        self.assertTrue(any(index.startswith('zhiyuan-audit-') and doc_id == 'admin_audit:demo:42'
                            for index, doc_id in self.world.stored))

    def test_authentication_audit_maps_the_authentication_interface(self):
        doc = self.world.docs_of('authentication_audit')[0]
        self.assertEqual(doc['audit_source'], '/aep/v1/admin/audit/authentication')
        self.assertEqual(doc['actor_user_id'], 'user-1')
        self.assertEqual(doc['event_type'], 'login.failed')
        self.assertEqual(doc['outcome'], 'failure')
        self.assertEqual(doc['source_hash'], 'abc123')
        self.assertEqual(doc['@timestamp'], '2026-10-02T04:05:06Z')

    def test_control_event_maps_the_control_event_interface(self):
        doc = self.world.docs_of('control_event')[0]
        self.assertEqual(doc['audit_source'], '/aep/v1/admin/control-events')
        self.assertEqual(doc['created_by'], 'admin-1')
        self.assertEqual(doc['state'], 'pending')
        self.assertEqual(doc['event_type'], 'session.control')
        self.assertEqual(doc['@timestamp'], '2026-10-02T07:08:09Z')

    def test_employee_request_maps_the_portal_response(self):
        doc = self.world.docs_of('employee_request')[0]
        self.assertEqual(doc['request_id'], 'req-1')
        self.assertEqual(doc['employee_name'], 'sales-helper')
        self.assertEqual(doc['owner'], 'user-1')
        self.assertEqual(doc['state'], 'rejected')
        self.assertEqual(doc['reason'], 'not needed')
        self.assertEqual(doc['@timestamp'], '2026-10-02T10:11:12Z')
        # The portal has no decidedBy/decisionReason fields; do not invent them.
        self.assertNotIn('decided_by', doc)
        self.assertNotIn('decision_reason', doc)

    def test_each_document_carries_a_stable_dedup_id(self):
        ids = {doc_id for _, doc_id in self.world.stored
               if doc_id and 'test-run' not in doc_id}
        self.assertIn('telemetry_event:demo:event-1', ids)
        self.assertIn('control_event:demo:ce-1', ids)
        self.assertIn('admin_audit:demo:42', ids)
        self.assertIn('authentication_audit:demo:7', ids)


class ChartWiring(unittest.TestCase):
    def test_template_includes_the_single_script_file(self):
        template = TEMPLATE.read_text(encoding='utf-8')
        self.assertIn('.Files.Get "files/ship.py"', template)
        self.assertNotIn('"""Poll AEP + portal admin APIs', template)
        self.assertIn('command: ["python3", "/script/ship.py"]', template)
        # The include path must resolve inside the chart, or the ConfigMap
        # renders empty and the CronJob fails at run time instead of here.
        included = re.search(r'\.Files\.Get "([^"]+)"', template).group(1)
        self.assertTrue((TEMPLATE.parents[1] / included).is_file(), included)

    def test_k3s_reference_manifest_holds_the_same_script(self):
        # k3s/audit-shipper.yaml is a plain manifest (it cannot use .Files.Get),
        # so its copy must be regenerated by hand whenever ship.py changes:
        # re-indent charts/zhiyuan-stack/files/ship.py by four spaces into the
        # `ship.py: |` block. This assertion is what stops a stale copy from
        # being merged.
        canonical = SCRIPT.read_text(encoding='utf-8')
        lines = K3S_MANIFEST.read_text(encoding='utf-8').splitlines()
        start = lines.index('  ship.py: |') + 1
        body = []
        for line in lines[start:]:
            if line and not line.startswith('    '):
                break
            body.append(line[4:])
        self.assertEqual('\n'.join(body) + '\n', canonical)


if __name__ == '__main__':
    sys.exit(unittest.main(verbosity=2))
