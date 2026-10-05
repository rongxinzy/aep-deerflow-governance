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
      name used by the governed tools), knowledge_bases (kb id -> display
      name: this employee's knowledge-base whitelist — None keeps the legacy
      unrestricted bridge credential, {} denies every knowledge_search, and
      calls must pass explicit in-list knowledge_base_ids because omitting
      the argument searches everything the credential sees),
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
        im_bot_name: str | None = None,
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
        # Employee knowledge-base whitelist (kb id -> display name). None =
        # legacy unrestricted bridge credential; {} = deny every search.
        self.knowledge_bases = None if knowledge_bases is None else {str(k): str(v) for k, v in knowledge_bases.items()}
        print(f"[aep-governance] construct governed={sorted(self.governed)} kb={sorted(self.knowledge_bases) if self.knowledge_bases is not None else None}", flush=True)
        # Display name of this employee's bot on the IM platform: @-mentions
        # of this name address the bot itself, not a delegation target.
        self.im_bot_name = (im_bot_name or "").strip() or None
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
    _WEKNORA_READ_TOOLS = frozenset({
        "hybrid_search",          # retrieval (kb_id)
        "wiki_search",            # retrieval (kb_id)
        "wiki_read_page",         # retrieval (kb_id)
        "wiki_index_view",        # retrieval (kb_id)
        "list_knowledge",         # list documents in one kb (kb_id)
        "get_knowledge_base",     # describe one kb (kb_id)
        "knowledge_search",       # dsh bridge retrieval (knowledge_base_ids)
    })

    # Full known surface of both bridges (used to attribute bare,
    # unprefixed tool names to the weknora server — default-deny there).
    _WEKNORA_KNOWN_TOOLS = _WEKNORA_READ_TOOLS | frozenset({
        "create_tenant", "list_tenants", "create_knowledge_base", "list_knowledge_bases",
        "delete_knowledge_base", "create_knowledge_from_file", "create_knowledge_from_url",
        "get_knowledge", "delete_knowledge", "list_chunks", "delete_chunk",
        "create_model", "list_models", "get_model",
        "create_session", "get_session", "list_sessions", "delete_session",
        "chat", "agent_chat", "list_agents", "get_agent",
    })

    @staticmethod
    def _strip_server_prefix(name: str) -> str:
        """DeerFlow delivers MCP tools under server-qualified names. The
        model-visible form is ``weknora_<tool>``/``weknora__<tool>``; the
        ToolNode execution name is the LangChain MCP adapter form
        ``mcp__weknora__<tool>``. Normalize every variant to the bare tool
        name (segment after the weknora token)."""
        parts = name.split("__")
        for index, part in enumerate(parts):
            if part == "weknora" and index + 1 < len(parts):
                return parts[-1]
        for prefix in ("weknora__", "weknora_"):
            if name.startswith(prefix):
                return name[len(prefix):]
        return name

    @classmethod
    def _is_weknora_tool(cls, name: str) -> bool:
        if name.startswith("weknora_") or "weknora" in name.split("__"):
            return True
        stripped = cls._strip_server_prefix(name)
        return stripped != name or name in cls._WEKNORA_KNOWN_TOOLS

    def _knowledge_note(self) -> str:
        """Prompt line listing this employee's usable knowledge bases."""
        if self.knowledge_bases is None:
            return ""
        listing = "、".join(f"{kb}（{name}）" for kb, name in sorted(self.knowledge_bases.items())) or "未连接任何知识库"
        return (f"本数字员工可用的知识库：{listing}。"
                f"知识检索只允许使用上述知识库：检索类工具（hybrid_search / wiki_search / knowledge_search 等）"
                f"必须显式传 kb_id 或 knowledge_base_ids，且只使用上述 id；"
                f"知识库的管理类操作（创建、删除、上传等）一律不可用，不要尝试。")

    def _knowledge_whitelist_denial(self, call: Any) -> ToolMessage | None:
        """Denial decision for the employee's knowledge-base whitelist.

        The weknora bridge credential is tenant-wide and, on the deployed
        PyPI bridge, carries admin rights (create/delete tenant, knowledge
        base, model). Under an explicit whitelist policy the whole weknora
        server is therefore default-deny: only kb-scoped read tools pass,
        only with explicit in-whitelist ids (omitting the id would act on
        everything the credential sees). Everything else on the server —
        admin writes, session chat, kb-less listings — is refused. Legacy
        employees (no whitelist configured) keep the unrestricted bridge.
        """
        if self.knowledge_bases is None:
            return None
        name = str(_tool_field(call, "name", "") or "")
        if not self._is_weknora_tool(name):
            return None
        tool = self._strip_server_prefix(name)
        args = dict(_tool_field(call, "args", None) or {})
        listing = "、".join(f"{kb}（{self.knowledge_bases[kb]}）" for kb in sorted(self.knowledge_bases)) or "（未连接任何知识库）"
        deny = lambda reason: ToolMessage(  # noqa: E731 - local readability
            content=f"DENIED {name}: {reason} 可用知识库：{listing}。",
            tool_call_id=str(_tool_field(call, "id", "") or ""),
        )
        if tool not in self._WEKNORA_READ_TOOLS:
            print(f"[aep-governance] DENY weknora tool={name} (not a whitelisted read tool)", flush=True)
            return deny("该知识服务工具不在数字员工可用范围内（仅只读检索类可用）。")
        # kb_id (single) and knowledge_base_ids (list) are the two id shapes
        # the two bridges use; both must be explicit and in-whitelist.
        ids: list[str] = []
        if isinstance(args.get("kb_id"), str):
            ids.append(args["kb_id"])
        requested = args.get("knowledge_base_ids")
        if isinstance(requested, list):
            ids.extend(str(item) for item in requested if str(item))
        if not ids:
            print(f"[aep-governance] DENY weknora tool={name} (no explicit kb id)", flush=True)
            return deny("必须显式传 kb_id（或 knowledge_base_ids）指定知识库，省略该参数会被拒绝。")
        if not all(i in self.knowledge_bases for i in ids):
            print(f"[aep-governance] DENY weknora tool={name} ids={ids} allowed={sorted(self.knowledge_bases)}", flush=True)
            return deny("本次访问的知识库不在本数字员工可用范围内。")
        print(f"[aep-governance] ALLOW weknora tool={name} ids={ids}", flush=True)
        return None

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
        return denied if denied is not None else handler(request)

    async def awrap_tool_call(self, request: Any, handler: Callable[[Any], Any]) -> ToolMessage:
        denied = self._check(request)
        return denied if denied is not None else await handler(request)
