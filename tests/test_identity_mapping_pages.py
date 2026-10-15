"""Identity resolution must cover all pages without retaining partial results."""

import io
import json
from types import SimpleNamespace
from urllib.parse import parse_qs, urlsplit

import pytest

from aep_deerflow_governance import _IdentityResolver


def resolver():
    client = SimpleNamespace(base_url="https://aep.invalid", token=lambda: "test-token")
    return _IdentityResolver(client, "source/with space", 60)


def mapping(external, local, status="active"):
    return {"externalId": external, "localSubjectId": local, "status": status}


def serve(monkeypatch, bodies):
    requests = []

    def open_page(request, timeout):
        requests.append(request.full_url)
        body = bodies[len(requests) - 1]
        if isinstance(body, Exception):
            raise body
        return io.StringIO(json.dumps(body))

    monkeypatch.setattr("urllib.request.urlopen", open_page)
    return requests


def test_resolves_user_after_first_200_and_encodes_opaque_cursor(monkeypatch):
    first = [mapping(f"ext-{i}", f"user-{i}") for i in range(200)]
    cursor = "next/+?=& value"
    requests = serve(monkeypatch, [
        {"items": first, "nextCursor": cursor},
        {"items": [mapping("last", "user-last"), mapping("disabled", "u", "disabled")]},
    ])
    subject = resolver()
    assert subject.resolve("last") == "user-last"
    assert subject.resolve("disabled") is None
    assert len(requests) == 2
    assert "/source%2Fwith%20space/mappings?" in requests[1]
    assert parse_qs(urlsplit(requests[1]).query)["cursor"] == [cursor]


@pytest.mark.parametrize("second_page", [
    OSError("unavailable"),
    {"items": [], "nextCursor": "repeat"},
    {"items": "invalid"},
    {"items": [], "nextCursor": 7},
])
def test_failed_or_malformed_page_discards_previous_cache(monkeypatch, second_page):
    subject = resolver()
    subject._by_external = {"old": "old-user"}
    serve(monkeypatch, [
        {"items": [mapping("partial", "partial-user")], "nextCursor": "repeat"},
        second_page,
    ])
    assert subject.resolve("old") is None
    assert subject._by_external == {}


def test_invalid_mapping_cannot_create_literal_none_identity(monkeypatch):
    serve(monkeypatch, [{"mappings": [mapping(None, "u"), mapping("ext", None), mapping("", "u")]}])
    assert resolver().resolve("None") is None


def test_unbounded_pagination_is_rejected(monkeypatch):
    requests = serve(monkeypatch, [
        {"items": [], "nextCursor": f"page-{i}"} for i in range(100)
    ])
    assert resolver().resolve("any") is None
    assert len(requests) == 100


def test_pagination_has_one_timeout_budget_and_no_partial_cache(monkeypatch):
    ticks = iter([0, 0, 3, 5, 9])
    monkeypatch.setattr("aep_deerflow_governance.time.monotonic", lambda: next(ticks))
    timeouts = []

    def open_page(request, timeout):
        timeouts.append(timeout)
        return io.StringIO(json.dumps({
            "mappings": [mapping("ext", "u")],
            "nextCursor": "next" if len(timeouts) == 1 else None,
        }))

    monkeypatch.setattr("urllib.request.urlopen", open_page)
    subject = resolver()
    assert subject.resolve("ext") is None
    assert subject._by_external == {}
    assert timeouts == [8, 3]


@pytest.mark.parametrize("body", [{"mappings": []}, OSError("unavailable")])
def test_empty_or_failed_snapshot_is_cached_for_ttl(monkeypatch, body):
    requests = serve(monkeypatch, [body])
    subject = resolver()
    assert subject.resolve("one") is None
    assert subject.resolve("two") is None
    assert len(requests) == 1
