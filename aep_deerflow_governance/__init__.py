"""AEP governance middlewares for DeerFlow (digital-employee Phase 1).

Ports the governance assets proven on the picoclaw runtime onto DeerFlow's
agent middleware chains:

- identity: which AEP user a conversation acts on behalf of (per-instance
  default, DeerFlow-user map, resolved live per run);
- scope: the requester's data scope, resolved live from the AEP control
  service (``GET /aep/v1/admin/data-scope/context``) with a short cache and
  fail-closed semantics;
- enforcement: governed tools are wrapped with ``wrap_tool_call`` — calls
  naming a department outside the requester's ``orgScope`` never execute and
  return an explicit denial, exactly like the picoclaw dept_data tool;
- context: a transient system reminder before each model call tells the
  model whose scope it operates in, so refusals are graceful.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.parse
import urllib.request
from typing import Any, Callable

from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import SystemMessage, ToolMessage

_GOVERNANCE_TAG = "aep_governance_v1"


class _AepClient:
    """Minimal sync AEP control-service client: password login + scope."""

    def __init__(self, base_url: str, deployment_id: str, username: str, password: str):
        self.base_url = base_url.rstrip("/")
        self.deployment_id = deployment_id
        self.username = username
        self.password = password
        self._token: str | None = None
        self._token_at = 0.0
        self._lock = threading.Lock()

    def _login(self) -> str:
        body = json.dumps({
            "deploymentId": self.deployment_id,
            "sessionId": f"deerflow-governance-{int(time.time())}",
            "username": self.username,
            "password": self.password,
        }).encode()
        req = urllib.request.Request(
            self.base_url + "/aep/v1/auth/password/login", data=body,
            headers={"Content-Type": "application/json", "X-AEP-Protocol-Version": "1.0"},
            method="POST")
        with urllib.request.urlopen(req, timeout=8) as resp:
            self._token = json.load(resp)["accessToken"]
        self._token_at = time.time()
        return self._token

    def token(self) -> str:
        with self._lock:
            if self._token is None or time.time() - self._token_at > 600:
                return self._login()
            return self._token

    def data_scope(self, user_id: str) -> dict[str, Any]:
        path = "/aep/v1/admin/data-scope/context?userId=" + urllib.parse.quote(user_id)
        for attempt in (1, 2):
            req = urllib.request.Request(
                self.base_url + path,
                headers={"Authorization": f"Bearer {self.token()}",
                         "X-AEP-Protocol-Version": "1.0"})
            try:
                with urllib.request.urlopen(req, timeout=8) as resp:
                    return json.load(resp)
            except urllib.error.HTTPError as e:
                if e.code == 401 and attempt == 1:
                    with self._lock:
                        self._token = None
                    continue
                raise
        raise RuntimeError("unreachable")


class _IdentityResolver:
    """Platform sender -> AEP user through the AEP identity-mappings API.

    Same semantics as the picoclaw aepgate resolver: paged listing of one
    identity source's active user mappings, short cache, fail closed (an
    unresolvable source resolves nobody).
    """

    def __init__(self, client: "_AepClient", source_id: str, cache_seconds: int):
        self.client = client
        self.source_id = source_id
        self.cache_seconds = cache_seconds
        self._by_external: dict[str, str] = {}
        self._loaded_at = 0.0
        self._lock = threading.Lock()

    def _load(self) -> dict[str, str]:
        path = f"/aep/v1/admin/identity-sources/{self.source_id}/mappings?subjectType=user&limit=200"
        req = urllib.request.Request(
            self.client.base_url + path,
            headers={"Authorization": f"Bearer {self.client.token()}",
                     "X-AEP-Protocol-Version": "1.0"})
        with urllib.request.urlopen(req, timeout=8) as resp:
            body = json.load(resp)
        out: dict[str, str] = {}
        for item in body.get("items") or body.get("mappings") or []:
            if item.get("status") != "active":
                continue
            out[str(item.get("externalId"))] = str(item.get("localSubjectId"))
        return out

    def resolve(self, external_id: str) -> str | None:
        now = time.time()
        with self._lock:
            if not self._by_external or now - self._loaded_at > self.cache_seconds:
                try:
                    self._by_external = self._load()
                    self._loaded_at = now
                except Exception:
                    self._by_external = {}  # fail closed on any error
                    self._loaded_at = now
            return self._by_external.get(external_id)


class AepGovernanceMiddleware(AgentMiddleware):
    """DeerFlow extension middleware enforcing AEP data scope per requester.

    Constructor kwargs (JSON-safe, from ``extensions.middlewares`` config):
      aep_base_url, deployment_id, service_username, service_password,
      default_aep_user, user_map (deerflow user id -> AEP user id),
      identity_source_id (optional: enables the IM bridge — platform senders
      from run context ``channel_user_id`` resolve through AEP identity
      mappings, unmapped senders fail closed),
      governed_tools (tool names to wrap), team_names (team id -> department
      name used by the governed tools), scope_cache_seconds.
    """

    def __init__(
        self,
        aep_base_url: str,
        service_username: str,
        service_password: str,
        default_aep_user: str,
        deployment_id: str = "demo",
        user_map: dict[str, str] | None = None,
        identity_source_id: str | None = None,
        governed_tools: list[str] | None = None,
        team_names: dict[str, str] | None = None,
        scope_cache_seconds: int = 60,
        **kwargs: Any,
    ) -> None:
        super().__init__(**kwargs)
        self.client = _AepClient(aep_base_url, deployment_id, service_username, service_password)
        self.default_aep_user = default_aep_user
        self.user_map = user_map or {}
        self.governed = set(governed_tools or ("dept_revenue", "dept_headcount",
                                                "benchdept__dept_revenue", "benchdept__dept_headcount"))
        self.team_names = team_names or {}
        self.name_to_team = {name: tid for tid, name in self.team_names.items()}
        self.cache_seconds = scope_cache_seconds
        self.identities = _IdentityResolver(self.client, identity_source_id, max(scope_cache_seconds, 30)) if identity_source_id else None
        self._scope_cache: dict[str, tuple[float, frozenset[str]]] = {}
        self._aep_user_cache: dict[str, tuple[float, bool]] = {}
        self._cache_lock = threading.Lock()

    # ------------------------------------------------------------ identity
    def resolve_aep_user(self, runtime: Any = None) -> str | None:
        """Identity chain: IM platform sender -> DeerFlow user map -> runtime user -> default.

        Returns None for an unmapped IM sender (fail closed everywhere).

        The runtime-user branch covers the web-chat path: the portal proxies
        browser sessions to the employee runtime with
        ``X-DeerFlow-Owner-User-Id`` set to the caller's AEP user id, so the
        runtime ``user_context`` id IS an AEP user id. It is validated against
        the AEP data-scope API (positive cache, TTL-bounded) before being
        trusted — a stale or fabricated id falls back to ``default_aep_user``
        instead of gaining its (nonexistent) scope.
        """
        context = getattr(runtime, "context", None) if runtime is not None else None
        if isinstance(context, dict) and context.get("channel_user_id"):
            if self.identities is None:
                return None  # IM traffic without a configured identity source: closed
            mapped = self.identities.resolve(str(context["channel_user_id"]))
            return mapped  # None -> unmapped -> fail closed
        try:
            from deerflow.runtime.user_context import get_current_user
            user = get_current_user()
            user_id = str(getattr(user, "id", "") or "") if user is not None else ""
        except Exception:
            user_id = ""
        if user_id:
            if user_id in self.user_map:
                return self.user_map[user_id]
            if self._is_aep_user(user_id):
                return user_id
        return self.default_aep_user

    def _is_aep_user(self, user_id: str) -> bool:
        """Positive-cache check that *user_id* resolves in AEP (data-scope API)."""
        now = time.time()
        with self._cache_lock:
            hit = self._aep_user_cache.get(user_id)
            if hit is not None and now - hit[0] < self.cache_seconds:
                return hit[1]
        try:
            self.client.data_scope(user_id)
            ok = True
        except Exception:
            ok = False
        with self._cache_lock:
            self._aep_user_cache[user_id] = (now, ok)
        return ok

    # ------------------------------------------------------------ scope
    def visible_teams(self, aep_user: str | None) -> frozenset[str]:
        """Team ids visible to the requester; empty on any failure (fail closed)."""
        if aep_user is None:
            return frozenset()
        now = time.time()
        with self._cache_lock:
            hit = self._scope_cache.get(aep_user)
            if hit and now - hit[0] < self.cache_seconds:
                return hit[1]
        try:
            scope = self.client.data_scope(aep_user)
            teams = frozenset(str(t) for t in scope.get("orgScope") or [])
        except Exception:
            teams = frozenset()  # fail closed: unresolvable scope sees nothing
        with self._cache_lock:
            self._scope_cache[aep_user] = (now, teams)
        return teams

    def _denied(self, tool_call_id: str, dept: str, visible: frozenset[str]) -> ToolMessage:
        names = ", ".join(sorted(self.team_names.get(t, t) for t in visible)) or "无可见部门"
        return ToolMessage(
            content=(f"DENIED dept={dept}: 该部门不在当前请求者的数据范围内。"
                     f"可见范围：{names}。如需跨部门数据，请联系管理员申请例外授权。"),
            tool_call_id=tool_call_id,
        )

    # ------------------------------------------------------------ hooks
    def _governance_message(self, aep_user: str | None, visible: frozenset[str]) -> SystemMessage:
        if aep_user is None:
            return SystemMessage(content=(
                f"<{_GOVERNANCE_TAG}>发送者的平台账号尚未绑定企业身份，数据范围解析结果为空。"
                f"所有数据类工具都会被拒绝。请直接告知用户：账号尚未绑定企业身份，"
                f"请联系管理员完成绑定后重试。</{_GOVERNANCE_TAG}>"))
        names = ", ".join(sorted(self.team_names.get(t, t) for t in visible)) or "（范围解析失败，按无可见部门处理）"
        return SystemMessage(content=(
            f"<{_GOVERNANCE_TAG}>当前会话代表 AEP 用户 {aep_user} 执行。"
            f"其数据范围仅覆盖：{names}。访问范围外部门的请求会被工具层拒绝；"
            f"请直接告知用户无权访问，不要尝试绕过。</{_GOVERNANCE_TAG}>"))

    def _decorated(self, request: Any) -> Any:
        aep_user = self.resolve_aep_user(getattr(request, "runtime", None))
        visible = self.visible_teams(aep_user)
        messages = [m for m in (list(getattr(request, "messages", None) or []))
                    if _GOVERNANCE_TAG not in str(getattr(m, "content", ""))]
        if messages:
            messages.insert(len(messages) - 1, self._governance_message(aep_user, visible))
        else:
            messages = [self._governance_message(aep_user, visible)]
        return request.override(messages=messages)

    def wrap_model_call(self, request: Any, handler: Callable[[Any], Any]) -> Any:
        return handler(self._decorated(request))

    async def awrap_model_call(self, request: Any, handler: Callable[[Any], Any]) -> Any:
        return await handler(self._decorated(request))

    def _check(self, request: Any) -> ToolMessage | None:
        """Denial decision for one governed tool call; None means allowed."""
        call = getattr(request, "tool_call", None)
        name = getattr(call, "name", "") or ""
        if name not in self.governed:
            return None
        args = dict(getattr(call, "args", None) or {})
        dept = str(args.get("dept", "")).strip()
        aep_user = self.resolve_aep_user(getattr(request, "runtime", None))
        visible = self.visible_teams(aep_user)
        if aep_user is None:
            print(f"[aep-governance] DENY unmapped-sender tool={name} dept={dept}", flush=True)
            return ToolMessage(
                content="DENIED: 发送者账号尚未绑定企业身份，无法解析数据范围。请联系管理员完成绑定后重试。",
                tool_call_id=getattr(call, "id", "") or "",
            )
        team_id = self.name_to_team.get(dept)
        if team_id is not None and team_id in visible:
            print(f"[aep-governance] ALLOW user={aep_user} tool={name} dept={dept}", flush=True)
            return None
        print(f"[aep-governance] DENY user={aep_user} tool={name} dept={dept}", flush=True)
        return self._denied(getattr(call, "id", "") or "", dept or "?", visible)

    def wrap_tool_call(self, request: Any, handler: Callable[[Any], Any]) -> ToolMessage:
        denied = self._check(request)
        return denied if denied is not None else handler(request)

    async def awrap_tool_call(self, request: Any, handler: Callable[[Any], Any]) -> ToolMessage:
        denied = self._check(request)
        return denied if denied is not None else await handler(request)
