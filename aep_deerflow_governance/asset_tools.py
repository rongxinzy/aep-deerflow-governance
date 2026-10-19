"""Governed asset tools — the runtime side of the AEP asset catalog (FR-AS-03).

Enabled iff the runtime env carries ``ASSET_TOOLS_ENABLED`` and
``ASSET_SUBJECT_ID``: the operator renders both from the DigitalEmployee CR
(the subject is the employee's own name), and the middleware registers
``asset_list`` / ``asset_ssh_exec`` in ``AgentMiddleware.tools``. Absent env
means no tools — an employee without the asset feature must never see them.

Execution does not happen here. Every call goes through the AEP control
plane, which enforces the deployment-wide kill switch, the asset status,
the binding and its capability allowlist, and writes the execution audit
row. The middleware contributes identity, not enforcement:

- the binding subject comes from ``ASSET_SUBJECT_ID`` (rendered config,
  unforgeable by the model);
- the requester user id is resolved per call from the trusted runtime
  context (``AepGovernanceMiddleware.resolve_aep_user``) and injected as a
  reserved argument in ``wrap_tool_call`` — a model-supplied value is
  overwritten, never trusted;
- an unresolved requester fails closed: no execution without a trusted
  on-behalf-of identity.

Credentials never appear in tool results or errors: AEP returns bounded
command output only.
"""
from __future__ import annotations

import json
import os
from typing import Any, Callable

from langchain_core.tools import BaseTool, tool

ASSET_REQUESTER_ARG = "asset_requester_user_id"
SSH_EXEC_TOOL = "asset_ssh_exec"
DEFAULT_TIMEOUT_S = 30


class _AssetGateway:
    """The AEP surface asset tools call; substituted in tests."""

    def __init__(self, client: Any, subject_id: str) -> None:
        self._client = client
        self.subject_id = subject_id

    def list_assets(self) -> list[dict[str, Any]]:
        return list(self._runtime_assets())

    def execute(self, asset: str, command: str, requester: str, timeout_seconds: int) -> dict[str, Any]:
        match = self._resolve(asset)
        if match is None:
            raise ValueError(_unknown_asset_message(asset, self._runtime_assets()))
        body = {
            "subject": {"type": "de_employee", "id": self.subject_id},
            "requesterUserId": requester,
            "capability": "exec",
            "command": command,
            "timeoutSeconds": timeout_seconds,
        }
        return self._client.asset_execute(match["assetId"], body)

    def _resolve(self, asset: str) -> dict[str, Any] | None:
        assets = self._runtime_assets()
        for record in assets:
            if asset in (record.get("assetId"), record.get("name")):
                return record
        return None

    def _runtime_assets(self) -> list[dict[str, Any]]:
        response = self._client.runtime_assets("de_employee", self.subject_id)
        return list(response.get("assets") or [])


def _unknown_asset_message(asset: str, assets: list[dict[str, Any]]) -> str:
    names = ", ".join(sorted(str(record.get("name") or record.get("assetId")) for record in assets)) or "none"
    return f"unknown asset {asset!r}; assets bound to this employee: {names}"


def _format_execution(result: dict[str, Any]) -> str:
    parts = [f"exit code: {result.get('exitCode')}", str(result.get("output") or "").rstrip()]
    if result.get("truncated"):
        parts.append("(output truncated at 64 KiB)")
    return "\n".join(parts)


def build_asset_tools(environ: dict[str, str] | None = None,
                      client: Any = None,
                      gateway_factory: Callable[[str], _AssetGateway] | None = None) -> list[BaseTool]:
    """Build the asset tools from ``ASSET_TOOLS_ENABLED``/``ASSET_SUBJECT_ID``.

    ``client`` is the middleware's own _AepClient — the single credential
    source (base URL, deployment, service account) the data-scope calls
    already use; the tools never construct their own. Returns ``[]`` when
    the feature env is absent or no client was provided.
    ``gateway_factory`` substitutes the AEP-carrying gateway in tests.
    """
    env = dict(os.environ if environ is None else environ)
    if not env.get("ASSET_TOOLS_ENABLED", "").strip() or not env.get("ASSET_SUBJECT_ID", "").strip():
        return []
    if client is None and gateway_factory is None:
        print("[aep-governance] asset tools disabled: no AEP client provided", flush=True)
        return []
    subject_id = env["ASSET_SUBJECT_ID"].strip()

    def make_gateway() -> _AssetGateway:
        if gateway_factory is not None:
            return gateway_factory(subject_id)
        return _AssetGateway(client, subject_id)

    @tool
    def asset_list() -> str:
        """列出本员工可用的服务器资产及其能力（来自 AEP 资产目录的绑定授权）。"""
        assets = make_gateway().list_assets()
        if not assets:
            return "没有绑定给本员工的可用资产。"
        lines = []
        for record in assets:
            capabilities = ", ".join(
                f"{capability.get('key')}({capability.get('risk')})"
                for capability in record.get("capabilities") or []
            )
            lines.append(f"- {record.get('name')} (id={record.get('assetId')}): {capabilities or '无可用能力'}")
        return "\n".join(lines)

    @tool
    def asset_ssh_exec(asset: str, command: str, timeout_seconds: int = DEFAULT_TIMEOUT_S,
                       asset_requester_user_id: str = "") -> str:
        """在绑定的服务器资产上执行一条 SSH 命令并返回输出。
        asset 用 asset_list 列出的名称或 id；仅限本员工被授权的资产与 exec 能力，
        执行经 AEP 审计。asset_requester_user_id 由平台注入，模型传入值会被覆盖。"""
        requester = asset_requester_user_id.strip()
        if not requester:
            return "DENIED: no trusted requester on this conversation; execution requires a resolved AEP user."
        timeout_seconds = max(1, min(int(timeout_seconds), 120))
        try:
            result = make_gateway().execute(asset, command, str(requester), timeout_seconds)
        except ValueError as error:
            return f"ERROR: {error}"
        except Exception as error:  # AEP problem details surface as HTTPError
            return f"ERROR: asset execution rejected or failed: {error}"
        return _format_execution(result)

    return [asset_list, asset_ssh_exec]
