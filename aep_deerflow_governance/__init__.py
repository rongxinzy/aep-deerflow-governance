"""AEP governance middlewares for DeerFlow (digital-employee Phase 1).

Ports the governance assets proven on the earlier aepgate runtime onto
DeerFlow's agent middleware chains:

- identity: which AEP user a conversation acts on behalf of (per-instance
  default, DeerFlow-user map, resolved live per run);
- scope: the requester's data scope, resolved live from the AEP control
  service (``GET /aep/v1/admin/data-scope/context``) with a short cache and
  fail-closed semantics;
- enforcement: governed tools are wrapped with ``wrap_tool_call`` — calls
  naming a department outside the requester's ``orgScope`` never execute and
  return an explicit denial, exactly like the aepgate dept_data tool;
- context: a transient system reminder before each model call tells the
  model whose scope it operates in, so refusals are graceful.
"""

from __future__ import annotations

import json
import copy
import threading
import time
import urllib.parse
import urllib.request
from typing import Any, Callable

from langchain.agents.middleware import AgentMiddleware
from langchain_core.messages import SystemMessage, ToolMessage

from .asset_tools import ASSET_REQUESTER_ARG, SSH_EXEC_TOOL
from .model_tools import MODEL_REQUESTER_ARG, MODEL_TOOL_NAMES

_GOVERNANCE_TAG = "aep_governance_v1"

# Actionable configuration error for knowledge tools that are called while the
# employee has no knowledge-base policy at all (no ``knowledge_bases`` kwarg —
# the DigitalEmployee carries no ``spec.knowledge.bases``). A missing policy is
# a configuration error, never an allow: the weknora bridge credential is
# tenant-wide and carries admin rights, so the middleware default-denies and
# tells the administrator exactly what to configure (and how to inventory and
# roll back). Kept as one constant so the runtime message and the migration
# runbook cannot drift apart.
_KNOWLEDGE_MISSING_POLICY_REASON = (
    "知识库访问策略缺失（配置错误）：该数字员工未配置知识库白名单（spec.knowledge.bases 缺失），"
    "缺策略时知识库工具按默认拒绝处理，原「未配置即不受限」的放行已移除。"
    "管理员修复：① 只读盘点该员工实际需要的 kb id（用原 WeKnora 凭据做只读查询，不要整租户照搬）；"
    "② 在 DigitalEmployee 的 spec.knowledge.bases 显式写入这些库"
    "（bases: [{\"id\": \"kb-1\", \"name\": \"销售知识库\"}]，可多条）；"
    "③ 保存并等 operator reconcile 完成后，重新发起本次调用。"
    "若该员工确实不应访问任何知识库，请显式配置 bases: []（deny-all）。"
    "迁移、盘点与回退步骤见 docs/design/knowledge-whitelist-default-deny.md。"
)


def _tool_field(call: Any, key: str, default: Any = None) -> Any:
    """Read one field off a tool call. langgraph hands ToolCallRequest.tool_call
    as a TypedDict (name/args/id keys); older shapes and tests may use objects."""
    if isinstance(call, dict):
        return call.get(key, default)
    return getattr(call, key, default)


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

    def runtime_assets(self, subject_type: str, subject_id: str) -> dict[str, Any]:
        path = ("/aep/v1/admin/assets/runtime?subjectType=" + urllib.parse.quote(subject_type)
                + "&subjectId=" + urllib.parse.quote(subject_id))
        return self._get(path)

    def asset_execute(self, asset_id: str, body: dict[str, Any]) -> dict[str, Any]:
        path = "/aep/v1/admin/assets/" + urllib.parse.quote(asset_id, safe="") + "/execute"
        return self._post(path, body)

    def _get(self, path: str) -> dict[str, Any]:
        for attempt in (1, 2):
            req = urllib.request.Request(
                self.base_url + path,
                headers={"Authorization": f"Bearer {self.token()}",
                         "X-AEP-Protocol-Version": "1.0"})
            try:
                with urllib.request.urlopen(req, timeout=15) as resp:
                    return json.load(resp)
            except urllib.error.HTTPError as e:
                if e.code == 401 and attempt == 1:
                    with self._lock:
                        self._token = None
                    continue
                raise
        raise RuntimeError("unreachable")

    def _post(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        payload = json.dumps(body).encode()
        for attempt in (1, 2):
            req = urllib.request.Request(
                self.base_url + path, data=payload,
                headers={"Authorization": f"Bearer {self.token()}",
                         "Content-Type": "application/json",
                         "X-AEP-Protocol-Version": "1.0"},
                method="POST")
            try:
                with urllib.request.urlopen(req, timeout=180) as resp:
                    return json.load(resp)
            except urllib.error.HTTPError as e:
                if e.code == 401 and attempt == 1:
                    with self._lock:
                        self._token = None
                    continue
                raise
        raise RuntimeError("unreachable")

    def issue_model_token(self, user_id: str) -> dict[str, Any]:
        """M2M data-plane token for a resolved user (AEP #361).

        Scopes are derived server-side from the target user's assignments;
        the returned token never exceeds what the user could obtain by
        logging in themselves.
        """
        return self._post("/aep/v1/admin/model-gateway/tokens", {"userId": user_id})

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

    Same semantics as the aepgate resolver: paged listing of one
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
        out: dict[str, str] = {}
        source = urllib.parse.quote(self.source_id, safe="")
        cursor = None
        seen: set[str] = set()
        deadline = time.monotonic() + 8
        # Build a complete snapshot before publishing it to the cache. A
        # failing/looping page must not leave a partial authorization map.
        for _ in range(100):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("Identity mapping pagination timed out")
            params = {"subjectType": "user", "limit": "200"}
            if cursor is not None:
                params["cursor"] = cursor
            path = f"/aep/v1/admin/identity-sources/{source}/mappings?{urllib.parse.urlencode(params)}"
            req = urllib.request.Request(
                self.client.base_url + path,
                headers={"Authorization": f"Bearer {self.client.token()}",
                         "X-AEP-Protocol-Version": "1.0"})
            with urllib.request.urlopen(req, timeout=remaining) as resp:
                body = json.load(resp)
            if time.monotonic() >= deadline:
                raise TimeoutError("Identity mapping pagination timed out")
            # ``items`` can be present but null while the same payload carries
            # ``mappings`` (the older shape); the or-chain tolerates that
            # instead of rejecting the page and clearing every identity.
            items = body.get("items") or body.get("mappings") or []
            if not isinstance(items, list):
                raise ValueError("Invalid identity mapping page")
            for item in items:
                if not isinstance(item, dict) or item.get("status") != "active":
                    continue
                external, local = item.get("externalId"), item.get("localSubjectId")
                if isinstance(external, str) and external and isinstance(local, str) and local:
                    out[external] = local
            cursor = body.get("nextCursor")
            if cursor is None or cursor == "":
                return out
            if not isinstance(cursor, str) or cursor in seen:
                raise ValueError("Invalid or repeated identity mapping cursor")
            seen.add(cursor)
        raise ValueError("Identity mapping pagination exceeded 100 pages")

    def resolve(self, external_id: str) -> str | None:
        now = time.time()
        with self._lock:
            if self._loaded_at == 0.0 or now - self._loaded_at > self.cache_seconds:
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
      name used by the governed tools), knowledge_bases (kb id -> display
      name: this employee's explicit knowledge-base whitelist — None means the
      operator rendered no policy, which is a configuration error and denies
      every knowledge tool with migration guidance for the administrator;
      {} denies every knowledge call; an explicit dict allows only in-list
      kb ids on read tools, and calls must pass an explicit in-list kb_id /
      knowledge_base_ids because omitting the argument searches everything
      the credential sees),
      scope_cache_seconds.
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
        knowledge_bases: dict[str, str] | None = None,
        knowledge_online_authorization: bool = False,
        im_bot_name: str | None = None,
        scope_cache_seconds: int = 60,
        **kwargs: Any,
    ) -> None:
        # langchain 1.x AgentMiddleware has no __init__ of its own (MRO ends
        # at object): forwarding unknown factory kwargs to super() raised
        # "object.__init__() takes exactly one argument" and took the whole
        # agent factory down. Unknown kwargs are tolerated here instead.
        super().__init__()
        self.client = _AepClient(aep_base_url, deployment_id, service_username, service_password)
        self.default_aep_user = default_aep_user
        self.user_map = user_map or {}
        self.governed = set(governed_tools or ("dept_revenue", "dept_headcount",
                                                "benchdept__dept_revenue", "benchdept__dept_headcount"))
        self.team_names = team_names or {}
        # Employee knowledge-base whitelist (kb id -> display name). None =
        # no policy configured by the operator (configuration error: default
        # deny, see _knowledge_whitelist_denial); {} = deny every call;
        # explicit dict = the only allow list.
        self.knowledge_bases = None if knowledge_bases is None else {str(k): str(v) for k, v in knowledge_bases.items()}
        self.knowledge_online_authorization = bool(knowledge_online_authorization)
        kb_policy = "missing" if self.knowledge_bases is None else (sorted(self.knowledge_bases) or "deny-all")
        print(f"[aep-governance] construct governed={sorted(self.governed)} kb_policy={kb_policy}", flush=True)
        # Display name of this employee's bot on the IM platform: @-mentions
        # of this name address the bot itself, not a delegation target.
        self.im_bot_name = (im_bot_name or "").strip() or None
        self.name_to_team = {name: tid for tid, name in self.team_names.items()}
        self.cache_seconds = scope_cache_seconds
        self.identities = _IdentityResolver(self.client, identity_source_id, max(scope_cache_seconds, 30)) if identity_source_id else None
        self._scope_cache: dict[str, tuple[float, frozenset[str]]] = {}
        self._aep_user_cache: dict[str, tuple[float, bool]] = {}
        self._cache_lock = threading.Lock()
        # Employee mailbox tools: registered iff EMAIL_MAILBOXES env is
        # present (operator envFrom-injects the employee's email Secret).
        # Absent/malformed env = no tools; the misconfiguration is logged,
        # never a crash loop. See email_tools for the security notes.
        from .email_tools import build_email_tools
        self.tools = build_email_tools()
        if self.tools:
            print(f"[aep-governance] email tools registered: {sorted(t.name for t in self.tools)}", flush=True)
        # Governed asset tools: registered iff the operator rendered
        # ASSET_TOOLS_ENABLED + ASSET_SUBJECT_ID from the employee CR. The
        # subject is deployment config (unforgeable); the requester is
        # resolved per call and injected in wrap_tool_call — see
        # asset_tools for the trust notes.
        from .asset_tools import build_asset_tools
        asset_tools = build_asset_tools(client=self.client)
        if asset_tools:
            self.tools = list(self.tools) + list(asset_tools)
            print(f"[aep-governance] asset tools registered: {sorted(t.name for t in asset_tools)}", flush=True)
        # Governed multimodal model tools: registered iff the operator
        # rendered MODEL_TOOLS_ENABLED + MODEL_TOOLS_GATEWAY_URL. Calls carry
        # a per-requester model token (M2M issuance) — never a shared key.
        from .model_tools import build_model_tools
        model_tools = build_model_tools(client=self.client)
        if model_tools:
            self.tools = list(self.tools) + list(model_tools)
            print(f"[aep-governance] model tools registered: {sorted(t.name for t in model_tools)}", flush=True)

    # ------------------------------------------------------------ identity
    def resolve_aep_user(self, runtime: Any = None) -> str | None:
        """Identity chain: IM platform sender -> DeerFlow user map -> runtime user.

        Returns ``None`` when the requester cannot be resolved (unmapped IM
        sender, unknown runtime user, or a user that AEP does not know). The
        configured ``default_aep_user`` is **not** used as an authorization
        source: an unresolved requester must not inherit that identity's scope.
        Callers that legitimately act must carry a trusted identity.

        The runtime-user branch covers the web-chat path: the portal proxies
        browser sessions to the employee runtime with
        ``X-DeerFlow-Owner-User-Id`` set to the caller's AEP user id, so the
        runtime ``user_context`` id IS an AEP user id. It is validated against
        the AEP data-scope API (positive cache, TTL-bounded) before being
        trusted — a stale or fabricated id resolves to ``None`` instead of
        gaining its (nonexistent) scope.
        """
        context = getattr(runtime, "context", None) if runtime is not None else None
        if isinstance(context, dict) and context.get("channel_user_id"):
            if self.identities is None:
                resolved = None  # IM traffic without a configured identity source: closed
            else:
                resolved = self.identities.resolve(str(context["channel_user_id"]))
            self._publish_requester(context, resolved)
            return resolved  # None -> unmapped -> fail closed
        try:
            from deerflow.runtime.user_context import get_current_user
            user = get_current_user()
            user_id = str(getattr(user, "id", "") or "") if user is not None else ""
        except Exception:
            user_id = ""
        resolved = None
        if user_id:
            if user_id in self.user_map:
                resolved = self.user_map[user_id]
            elif self._is_aep_user(user_id):
                resolved = user_id
        self._publish_requester(context, resolved)
        return resolved

    @staticmethod
    def _publish_requester(context: Any, requester_id: str | None) -> None:
        """Publish the trusted requester for downstream consumers (memory).

        The run context is shared by the middlewares in this run; memory reads
        ``requester_id`` from it. ``None`` means "not resolved" and must stay
        distinguishable from "resolved to this id".
        """
        if isinstance(context, dict):
            context["requester_id"] = requester_id
        # Also publish as a task-local ContextVar: memory's read path
        # (prompt assembly) has no runtime handle, and background timers do not
        # inherit contextvars, so the write path captures it while still in-task.
        try:
            from deerflow.runtime.user_context import set_requester_id
        except Exception:  # pragma: no cover - deerflow always present in-runtime
            return
        set_requester_id(requester_id)

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

    # ------------------------------------------------------ knowledge scope
    # The deployed weknora bridges expose two different tool surfaces:
    # - PyPI tencent-weknora-mcp (cicd): weknora_hybrid_search / wiki_search /
    #   list_knowledge / get_knowledge_base with a single `kb_id`, plus a wide
    #   admin surface (create_tenant, delete_knowledge_base, create_model, …).
    # - WeKnora's own MCP (dev): knowledge_search with `knowledge_base_ids`.
    # DeerFlow may deliver either with a server-alias prefix (weknora_ /
    # weknora__). Under an explicit whitelist policy the weknora server is
    # default-deny: only these kb-scoped read tools pass, and only with an
    # in-whitelist kb_id / knowledge_base_ids.
    # One registry is authoritative for attribution, whitelist-read policy,
    # and online Portal authorization. Native names below match every
    # @mcp.tool registration in WeKnora's mcp-server/weknora_mcp_server.py.
    # Tuple fields are (whitelist_read, portal_authorized). MCP wrappers own
    # fixed actions; middleware must not inject schema-unknown arguments.
    _WEKNORA_TOOL_REGISTRY = {
        "create_tenant": (False, False),
        "list_tenants": (False, False),
        "create_knowledge_base": (False, False),
        "list_knowledge_bases": (True, True),
        "list_shared_knowledge_bases": (False, False),
        "get_knowledge_base": (True, True),
        "delete_knowledge_base": (False, False),
        "hybrid_search": (True, True),
        "create_knowledge_from_file": (False, False),
        "create_knowledge_from_url": (False, False),
        "create_knowledge_from_text": (False, False),
        "update_knowledge_from_text": (False, False),
        "list_knowledge": (True, True),
        "get_knowledge": (True, True),
        "delete_knowledge": (False, False),
        "create_model": (False, False),
        "list_models": (False, False),
        "get_model": (False, False),
        "create_session": (False, False),
        "get_session": (False, False),
        "list_sessions": (False, False),
        "delete_session": (False, False),
        "chat": (False, False),
        "agent_chat": (False, False),
        "list_agents": (False, False),
        "get_agent": (False, False),
        "list_chunks": (True, True),
        "delete_chunk": (False, False),
        "wiki_search": (True, True),
        "wiki_read_page": (True, True),
        "wiki_index_view": (True, True),
        # Older bridge names are recognized for default-deny attribution.
        "search_knowledge": (True, False),
        "grep_chunks": (True, False),
        "read_document": (True, False),
        "list_documents": (True, False),
        "knowledge_search": (True, False),
    }
    _WEKNORA_KNOWN_TOOLS = frozenset(_WEKNORA_TOOL_REGISTRY)

    @staticmethod
    def _strip_server_prefix(name: str) -> str:
        """DeerFlow delivers MCP tools under server-qualified names. The
        model-visible form is ``weknora_<tool>``/``weknora__<tool>``; the
        ToolNode execution name is the LangChain MCP adapter form
        ``mcp__weknora__<tool>``. Normalize every variant to the bare tool
        name (segment after the weknora token)."""
        for prefix in ("mcp__weknora__", "weknora__", "weknora_"):
            if name.startswith(prefix):
                return name[len(prefix):]
        parts = name.split("__")
        for index, part in enumerate(parts):
            if part == "weknora" and index + 1 < len(parts):
                return "__".join(parts[index + 1:])
        return name

    @classmethod
    def _is_weknora_tool(cls, name: str) -> bool:
        if name.startswith("weknora_") or "weknora" in name.split("__"):
            return True
        stripped = cls._strip_server_prefix(name)
        return stripped != name or name in cls._WEKNORA_KNOWN_TOOLS

    def _knowledge_note(self) -> str:
        """Prompt line for this employee's knowledge bases (or the missing-policy notice)."""
        if self.knowledge_bases is None:
            # Missing policy: the middleware default-denies every knowledge
            # tool, so the prompt must not advertise a usable set — it tells
            # the model to refuse gracefully and name the missing config.
            return ("本数字员工未配置知识库白名单（缺策略），知识库工具按默认拒绝处理："
                    "不要调用 hybrid_search / wiki_search / knowledge_search 等知识库工具；"
                    "如用户需要企业知识库内容，请说明该员工的知识库授权尚未配置，"
                    "需联系管理员在 spec.knowledge.bases 中显式配置可访问的知识库。")
        listing = "、".join(f"{kb}（{name}）" for kb, name in sorted(self.knowledge_bases.items())) or "未连接任何知识库"
        return (f"本数字员工可用的知识库：{listing}。"
                f"知识检索只允许使用上述知识库：检索类工具（hybrid_search / wiki_search / knowledge_search 等）"
                f"必须显式传 kb_id 或 knowledge_base_ids，且只使用上述 id；"
                f"知识库的管理类操作（创建、删除、上传等）一律不可用，不要尝试。")

    def _knowledge_whitelist_denial(self, call: Any) -> ToolMessage | None:
        """Denial decision for the employee's knowledge-base whitelist.

        The weknora bridge credential is tenant-wide and, on the deployed
        PyPI bridge, carries admin rights (create/delete tenant, knowledge
        base, model). The whole weknora server is therefore default-deny:
        only kb-scoped read tools pass, only with explicit in-whitelist ids
        (omitting the id would act on everything the credential sees).
        Everything else on the server — admin writes, session chat, kb-less
        listings — is refused.

        Policy states, in the order decided here:
        - no policy (``knowledge_bases is None``): a configuration error —
          every weknora tool is denied with an actionable message naming
          ``spec.knowledge.bases`` (never a silent allow, and never an
          automatic grant of unknown legacy bases to the employee);
        - ``{}``: deny every knowledge call;
        - explicit dict: the only allow list — read tools, in-list ids only.
        """
        name = str(_tool_field(call, "name", "") or "")
        if not self._is_weknora_tool(name):
            return None
        tool = self._strip_server_prefix(name)
        if self.knowledge_bases is None:
            print(f"[aep-governance] DENY weknora tool={name} (no knowledge policy configured)", flush=True)
            return ToolMessage(content=f"DENIED {name}: {_KNOWLEDGE_MISSING_POLICY_REASON}",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        if self.knowledge_bases == {}:
            return ToolMessage(content=f"DENIED {name}: 未连接任何知识库（deny-all whitelist）。",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        policy = self._WEKNORA_TOOL_REGISTRY.get(tool)
        if self.knowledge_online_authorization and (policy is None or not policy[1]):
            return ToolMessage(content=f"DENIED {name}: tool is not protected by online knowledge authorization.",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        args = dict(_tool_field(call, "args", None) or {})
        listing = "、".join(f"{kb}（{self.knowledge_bases[kb]}）" for kb in sorted(self.knowledge_bases)) or "（未连接任何知识库）"
        deny = lambda reason: ToolMessage(  # noqa: E731 - local readability
            content=f"DENIED {name}: {reason} 可用知识库：{listing}。",
            tool_call_id=str(_tool_field(call, "id", "") or ""),
        )
        if policy is None or not policy[0]:
            print(f"[aep-governance] DENY weknora tool={name} (not a whitelisted read tool)", flush=True)
            return deny("该知识服务工具不在数字员工可用范围内（仅只读检索类可用）。")
        # kb_id (single) and knowledge_base_ids (list) are the two id shapes
        # the two bridges use; both must be explicit and in-whitelist.
        ids: list[str] = []
        if isinstance(args.get("kb_id"), str):
            ids.append(args["kb_id"])
        for key in ("knowledge_base_id",):
            if isinstance(args.get(key), str):
                ids.append(args[key])
        requested = args.get("knowledge_base_ids")
        if isinstance(requested, list):
            ids.extend(str(item) for item in requested if str(item))
        if tool == "list_knowledge_bases" and not ids and not self.knowledge_online_authorization:
            # Without the online Portal filter, the bridge's tenant-wide list
            # call cannot be made safe by a middleware-only whitelist check.
            return deny("explicit knowledge-base scope is required; tenant-wide catalog reads are denied")
        if tool in {"read_document", "get_knowledge", "list_chunks"}:
            if not self.knowledge_online_authorization:
                return deny("opaque document handles require online Portal authorization")
            # Document handles are opaque; the MCP server resolves their KB
            # from trusted metadata and performs the live Portal check there.
            ids = []
        elif tool == "list_knowledge_bases":
            ids = list(self.knowledge_bases)
        elif tool in {"search_knowledge", "grep_chunks", "wiki_search"} and not ids:
            # Omitted scope means the employee's explicitly bound set, never
            # the whole WeKnora tenant. The MCP server will intersect it with
            # current user/team grants before querying.
            ids = list(self.knowledge_bases)
        if not ids and tool not in {"read_document", "get_knowledge", "list_chunks"}:
            print(f"[aep-governance] DENY weknora tool={name} (no explicit kb id)", flush=True)
            return deny("必须显式传 kb_id（或 knowledge_base_ids）指定知识库，省略该参数会被拒绝。")
        if not all(i in self.knowledge_bases for i in ids):
            print(f"[aep-governance] DENY weknora tool={name} ids={ids} allowed={sorted(self.knowledge_bases)}", flush=True)
            return deny("本次访问的知识库不在本数字员工可用范围内。")
        print(f"[aep-governance] ALLOW weknora tool={name} ids={ids}", flush=True)
        return None

    def _knowledge_context_request(self, request: Any) -> Any | ToolMessage:
        """Overwrite model-supplied identity fields with Portal run context."""
        call = getattr(request, "tool_call", None) or {}
        runtime = getattr(request, "runtime", None)
        context = getattr(runtime, "context", None) if runtime is not None else None
        if not isinstance(context, dict):
            return ToolMessage(content="DENIED: knowledge session context unavailable; start a new Portal chat request.",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        handle = context.get("zhiyuan_knowledge_context")
        employee = context.get("zhiyuan_employee_name")
        if not isinstance(handle, str) or len(handle) != 43 or not isinstance(employee, str) or not employee:
            return ToolMessage(content="DENIED: knowledge session context unavailable; start a new Portal chat request.",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        args = dict(_tool_field(call, "args", None) or {})
        # These reserved fields are injected after parsing the model call and
        # cannot be forged through tool arguments.
        args["knowledge_context_handle"] = handle
        args["knowledge_employee_name"] = employee
        # Read actions are fixed inside the corresponding MCP wrapper. Never
        # forward a model-selected action or inject an argument absent from
        # the tool schema.
        args.pop("knowledge_action", None)
        tool_name = self._strip_server_prefix(str(_tool_field(call, "name", "") or ""))
        if tool_name in {"list_knowledge_bases", "search_knowledge", "grep_chunks", "wiki_search"} \
                and not args.get("knowledge_base_ids"):
            args["knowledge_base_ids"] = list(self.knowledge_bases or {})
        normalized = dict(call) if isinstance(call, dict) else {
            "name": _tool_field(call, "name", ""),
            "id": _tool_field(call, "id", ""),
        }
        normalized["args"] = args
        override = getattr(request, "override", None)
        if callable(override):
            return override(tool_call=normalized)
        # Compatibility for the older middleware request shape used by the
        # unit tests and older LangChain versions.
        cloned = copy.copy(request)
        cloned.tool_call = normalized
        return cloned

    def _model_requester_request(self, request: Any) -> Any | ToolMessage:
        """Inject the trusted requester into multimodal model tool calls.

        Same trust model as the asset tools: the requester comes from the
        run context, a model-supplied ``model_requester_user_id`` is
        overwritten, and an unresolved requester fails closed — no gateway
        call without an on-behalf-of identity.
        """
        call = getattr(request, "tool_call", None) or {}
        runtime = getattr(request, "runtime", None)
        requester = self.resolve_aep_user(runtime)
        if not requester:
            return ToolMessage(content="DENIED: model tools require a requester bound to an AEP account on this conversation.",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        args = dict(_tool_field(call, "args", None) or {})
        args[MODEL_REQUESTER_ARG] = str(requester)
        normalized = dict(call) if isinstance(call, dict) else {
            "name": _tool_field(call, "name", ""),
            "id": _tool_field(call, "id", ""),
        }
        normalized["args"] = args
        override = getattr(request, "override", None)
        if callable(override):
            return override(tool_call=normalized)
        cloned = copy.copy(request)
        cloned.tool_call = normalized
        return cloned

    def _asset_context_request(self, request: Any) -> Any | ToolMessage:
        """Inject the trusted requester into asset tool calls.

        The requester is resolved from the run context exactly as the data
        scope chain resolves it; an unresolved requester denies the call
        (fail closed). A model-supplied ``asset_requester_user_id`` is
        overwritten here, never trusted.
        """
        call = getattr(request, "tool_call", None) or {}
        runtime = getattr(request, "runtime", None)
        requester = self.resolve_aep_user(runtime)
        if not requester:
            return ToolMessage(content="DENIED: asset execution requires a requester bound to an AEP account on this conversation.",
                               tool_call_id=str(_tool_field(call, "id", "") or ""))
        args = dict(_tool_field(call, "args", None) or {})
        args[ASSET_REQUESTER_ARG] = str(requester)
        normalized = dict(call) if isinstance(call, dict) else {
            "name": _tool_field(call, "name", ""),
            "id": _tool_field(call, "id", ""),
        }
        normalized["args"] = args
        override = getattr(request, "override", None)
        if callable(override):
            return override(tool_call=normalized)
        cloned = copy.copy(request)
        cloned.tool_call = normalized
        return cloned

    # ------------------------------------------------------------ hooks
    def _governance_message(self, aep_user: str | None, visible: frozenset[str]) -> SystemMessage:
        note = self._knowledge_note()
        if aep_user is None:
            # Guest mode: the sender is already inside the corporate IM
            # boundary (authenticated by the platform), just not linked to an
            # AEP account yet. Chat normally; only governed data tools are
            # denied — and the agent should mention binding only when the
            # user actually asks for enterprise data, not on every turn.
            return SystemMessage(content=(
                f"<{_GOVERNANCE_TAG}>发送者来自企业 IM，但尚未关联企业账号：权限层级未知（访客）。"
                f"请正常与其对话——通用问题、闲聊、知识问答照常回答，不要主动、反复提及绑定事宜。"
                f"仅企业数据类工具的调用会被工具层拒绝；当用户询问企业数据而被拒绝时，"
                f"自然说明：当前账号未绑定企业身份，绑定后即可按其权限查询，请联系管理员完成绑定。{note}</{_GOVERNANCE_TAG}>"))
        names = ", ".join(sorted(self.team_names.get(t, t) for t in visible)) or "（范围解析失败，按无可见部门处理）"
        return SystemMessage(content=(
            f"<{_GOVERNANCE_TAG}>当前会话代表 AEP 用户 {aep_user} 执行。"
            f"其数据范围仅覆盖：{names}。访问范围外部门的请求会被工具层拒绝；"
            f"请直接告知用户无权访问，不要尝试绕过。{note}</{_GOVERNANCE_TAG}>"))

    def _im_self_note(self, context: Any) -> str | None:
        """Prompt note for IM turns when the bot's display name is known.

        @-mentions of the bot name are users addressing the bot itself —
        mid-message or leading. Without this note the model mistakes its own
        display name for a delegation target and reports "无法接入".
        """
        if not self.im_bot_name:
            return None
        if not (isinstance(context, dict) and context.get("channel_user_id")):
            return None  # IM traffic only
        return (
            f"你在企业 IM 中的机器人名称是「{self.im_bot_name}」。消息中出现"
            f"「@{self.im_bot_name}」或提到「{self.im_bot_name}」时，都是用户在"
            f"称呼你本人：直接以自己的身份回答，不要把自己当作需要连接或委托"
            f"的其他智能体。")

    def _decorated(self, request: Any) -> Any:
        aep_user = self.resolve_aep_user(getattr(request, "runtime", None))
        visible = self.visible_teams(aep_user)
        messages = [m for m in (list(getattr(request, "messages", None) or []))
                    if _GOVERNANCE_TAG not in str(getattr(m, "content", ""))]
        note = self._im_self_note(getattr(request, "runtime", None) and getattr(request.runtime, "context", None))
        governance = self._governance_message(aep_user, visible)
        if note:
            governance = SystemMessage(content=str(governance.content) + note)
        if messages:
            # Anthropic-protocol providers require system messages to be a
            # single consecutive block at the head of the conversation;
            # inserting mid-list (as the OpenAI path tolerated) is rejected
            # with "multiple non-consecutive system messages".
            head = 0
            while head < len(messages) and isinstance(messages[head], SystemMessage):
                head += 1
            messages.insert(head, governance)
        else:
            messages = [governance]
        return request.override(messages=messages)

    def before_agent(self, state: Any, runtime: Any) -> dict | None:
        """Resolve and publish the trusted requester before any memory recall.

        Memory's first recall runs inside another middleware's ``before_agent``,
        so resolving only at model-call time is too late — the recall would see
        an unresolved requester. Publishing here (idempotent) makes the identity
        available for the memory read path as well as the model call.
        """
        self.resolve_aep_user(runtime)
        return None

    async def abefore_agent(self, state: Any, runtime: Any) -> dict | None:
        self.resolve_aep_user(runtime)
        return None

    def wrap_model_call(self, request: Any, handler: Callable[[Any], Any]) -> Any:
        return handler(self._decorated(request))

    async def awrap_model_call(self, request: Any, handler: Callable[[Any], Any]) -> Any:
        return await handler(self._decorated(request))

    def _check(self, request: Any) -> ToolMessage | None:
        """Denial decision for one governed tool call; None means allowed."""
        call = getattr(request, "tool_call", None) or {}
        # Knowledge-base whitelist first: it is per-employee static policy
        # (no AEP round-trip) and independent of the dept-governed set.
        denial = self._knowledge_whitelist_denial(call)
        if denial is not None:
            return denial
        name = str(_tool_field(call, "name", "") or "")
        # Mailbox credentials belong to the employee, not to every sender who
        # can reach its bot. Identity is the minimum gate for all three tools;
        # per-user mailbox grants and send approval remain separate policy.
        if name in {"email_list", "email_read", "email_send"}:
            if self.resolve_aep_user(getattr(request, "runtime", None)) is None:
                return ToolMessage(
                    content="DENIED: 当前为访客身份（未绑定企业账号），无法使用员工邮箱。请联系管理员绑定企业账号。",
                    tool_call_id=str(_tool_field(call, "id", "") or ""),
                )
        if name not in self.governed:
            return None
        args = dict(_tool_field(call, "args", None) or {})
        dept = str(args.get("dept", "")).strip()
        aep_user = self.resolve_aep_user(getattr(request, "runtime", None))
        visible = self.visible_teams(aep_user)
        if aep_user is None:
            print(f"[aep-governance] DENY guest tool={name} dept={dept}", flush=True)
            return ToolMessage(
                content="DENIED: 当前为访客身份（未绑定企业账号），无法查询企业数据。请联系管理员绑定企业账号后，按你的权限查询。",
                tool_call_id=str(_tool_field(call, "id", "") or ""),
            )
        team_id = self.name_to_team.get(dept)
        if team_id is not None and team_id in visible:
            print(f"[aep-governance] ALLOW user={aep_user} tool={name} dept={dept}", flush=True)
            return None
        print(f"[aep-governance] DENY user={aep_user} tool={name} dept={dept}", flush=True)
        return self._denied(str(_tool_field(call, "id", "") or ""), dept or "?", visible)

    def wrap_tool_call(self, request: Any, handler: Callable[[Any], Any]) -> ToolMessage:
        denied = self._check(request)
        if denied is not None:
            return denied
        name = str(_tool_field(getattr(request, "tool_call", None) or {}, "name", ""))
        if self.knowledge_online_authorization and self._is_weknora_tool(name):
            enriched = self._knowledge_context_request(request)
            if isinstance(enriched, ToolMessage):
                return enriched
            request = enriched
        if name == SSH_EXEC_TOOL:
            enriched = self._asset_context_request(request)
            if isinstance(enriched, ToolMessage):
                return enriched
            request = enriched
        if name in MODEL_TOOL_NAMES:
            enriched = self._model_requester_request(request)
            if isinstance(enriched, ToolMessage):
                return enriched
            request = enriched
        return handler(request)

    async def awrap_tool_call(self, request: Any, handler: Callable[[Any], Any]) -> ToolMessage:
        denied = self._check(request)
        if denied is not None:
            return denied
        name = str(_tool_field(getattr(request, "tool_call", None) or {}, "name", ""))
        if self.knowledge_online_authorization and self._is_weknora_tool(name):
            enriched = self._knowledge_context_request(request)
            if isinstance(enriched, ToolMessage):
                return enriched
            request = enriched
        if name == SSH_EXEC_TOOL:
            enriched = self._asset_context_request(request)
            if isinstance(enriched, ToolMessage):
                return enriched
            request = enriched
        if name in MODEL_TOOL_NAMES:
            enriched = self._model_requester_request(request)
            if isinstance(enriched, ToolMessage):
                return enriched
            request = enriched
        return await handler(request)


# Positioning: the dynamic-context middleware's ``before_agent`` performs the
# first memory recall, so governance must run before it for the trusted
# requester to be published in time. Harmless when the host lacks that module.
try:  # pragma: no cover - depends on the host providing deerflow
    from deerflow.agents.middlewares.dynamic_context_middleware import (
        DynamicContextMiddleware as _DynamicContextMiddleware,
    )

    AepGovernanceMiddleware._prev_anchor = _DynamicContextMiddleware  # type: ignore[attr-defined]
except Exception:  # pragma: no cover
    pass
