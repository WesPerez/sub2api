#!/usr/bin/env python3
from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import re
import signal
import sys
import time
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, Callable, Protocol
from zoneinfo import ZoneInfo


MAX_RESPONSE_BYTES = 1024 * 1024
ACCOUNT_PAGE_SIZE = 1000
AGENTROUTER_RECOVERY_HOURS = tuple(range(24))
AGENTROUTER_RECOVERY_MINUTE = 0
AGENTROUTER_URL_MARKER = "agentrouter.org"
AGENTROUTER_GROUP_SUFFIXES = ("gpt", "claude", "glm", "deepseek")
AGENTROUTER_GROUP_LIMIT = 3
DEFAULT_POLICY_PATH = Path("/etc/server-scheduled-tasks/agentrouter-recovery.json")
AGENTROUTER_BALANCE_RE = re.compile(r"余额\s*[:：]?\s*\$?(-?\d+(?:\.\d+)?)", re.IGNORECASE)
LOCAL_TIMEZONE = ZoneInfo("Asia/Shanghai")


class RecoveryError(RuntimeError):
    pass


class RecoveryClient(Protocol):
    def list_accounts(self) -> list[dict[str, Any]]: ...

    def get_account(self, account_id: int) -> dict[str, Any]: ...

    def account_base_url(self, account: dict[str, Any]) -> str: ...

    def recover_state(self, account_id: int) -> None: ...

    def set_schedulable(self, account_id: int, schedulable: bool) -> None: ...


class Sub2APIClient:
    def __init__(
        self,
        base_url: str,
        admin_key: str,
        timeout: float,
    ) -> None:
        self.base_url = validate_base_url(base_url)
        self.admin_key = admin_key
        self.timeout = timeout
        self.credential_identities: dict[int, str] = {}
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def _request(
        self,
        method: str,
        path: str,
        payload: dict[str, Any] | None = None,
    ) -> tuple[int, str]:
        body = None
        headers = {"x-api-key": self.admin_key}
        if payload is not None:
            body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self.base_url + path,
            data=body,
            headers=headers,
            method=method,
        )
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                raw = response.read(MAX_RESPONSE_BYTES).decode("utf-8", "replace")
                return int(response.status), raw
        except urllib.error.HTTPError as exc:
            raw = exc.read(MAX_RESPONSE_BYTES).decode("utf-8", "replace")
            return int(exc.code or 0), raw

    @staticmethod
    def _unwrap_json(status: int, body: str, operation: str) -> dict[str, Any]:
        if status >= 400:
            raise RecoveryError(f"{operation} failed: HTTP {status}")
        try:
            payload = json.loads(body)
        except json.JSONDecodeError as exc:
            raise RecoveryError(f"{operation} returned invalid JSON") from exc
        if not isinstance(payload, dict):
            raise RecoveryError(f"{operation} returned an invalid payload")
        if payload.get("code") not in (None, 0, "0"):
            raise RecoveryError(f"{operation} was rejected")
        data = payload.get("data", payload)
        if not isinstance(data, dict):
            raise RecoveryError(f"{operation} returned invalid data")
        return data

    def get_account(self, account_id: int) -> dict[str, Any]:
        status, body = self._request("GET", f"/api/v1/admin/accounts/{account_id}")
        return self._unwrap_json(status, body, f"get account {account_id}")

    def list_accounts(self) -> list[dict[str, Any]]:
        page = 1
        result: list[dict[str, Any]] = []
        seen: set[int] = set()
        while True:
            params: dict[str, Any] = {
                "page": page,
                "page_size": ACCOUNT_PAGE_SIZE,
                "sort_by": "id",
                "sort_order": "asc",
                "lite": "true",
            }
            query = urllib.parse.urlencode(params)
            status, body = self._request("GET", f"/api/v1/admin/accounts?{query}")
            data = self._unwrap_json(status, body, "list accounts")
            items = data.get("items")
            if not isinstance(items, list):
                raise RecoveryError("list accounts returned invalid items")
            for item in items:
                if not isinstance(item, dict):
                    continue
                try:
                    account_id = int(item.get("id"))
                except (TypeError, ValueError):
                    raise RecoveryError("list accounts returned an invalid account ID")
                if account_id in seen:
                    raise RecoveryError(f"list accounts returned duplicate account {account_id}")
                seen.add(account_id)
                result.append(item)

            try:
                pages = max(1, int(data.get("pages") or 1))
            except (TypeError, ValueError) as exc:
                raise RecoveryError("list accounts returned invalid pagination") from exc
            if page >= pages or not items:
                break
            page += 1
            if page > 10000:
                raise RecoveryError("list accounts exceeded pagination safety limit")
        return result

    def _export_account(self, account: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
        try:
            account_id = int(account.get("id"))
        except (TypeError, ValueError) as exc:
            raise RecoveryError("cannot export an account without a valid ID") from exc
        query = urllib.parse.urlencode({"ids": account_id, "include_proxies": "false"})
        status, body = self._request("GET", f"/api/v1/admin/accounts/data?{query}")
        data = self._unwrap_json(status, body, f"export account {account_id}")
        exported = data.get("accounts")
        if not isinstance(exported, list) or len(exported) != 1 or not isinstance(exported[0], dict):
            raise RecoveryError(f"export account {account_id} returned an invalid account set")
        exported_account = exported[0]
        if not same_exported_identity(account, exported_account):
            raise RecoveryError(f"account {account_id} changed identity before URL check")
        credentials = exported_account.get("credentials")
        if not isinstance(credentials, dict):
            raise RecoveryError(f"account {account_id} export is missing credentials")
        return exported_account, credentials

    def account_base_url(self, account: dict[str, Any]) -> str:
        _exported, credentials = self._export_account(account)
        fingerprint = hashlib.sha256(json.dumps(credentials, sort_keys=True).encode()).hexdigest()
        account_id = int(account['id'])
        previous = self.credential_identities.setdefault(account_id, fingerprint)
        if previous != fingerprint:
            raise RecoveryError(f'account {account_id} credentials changed during this run')
        return str(credentials.get("base_url") or "")

    def recover_state(self, account_id: int) -> None:
        status, body = self._request(
            "POST", f"/api/v1/admin/accounts/{account_id}/recover-state", {}
        )
        self._unwrap_json(status, body, f"recover account {account_id}")

    def set_schedulable(self, account_id: int, schedulable: bool) -> None:
        status, body = self._request(
            "POST",
            f"/api/v1/admin/accounts/{account_id}/schedulable",
            {"schedulable": schedulable},
        )
        self._unwrap_json(status, body, f"set schedulable for account {account_id}")


def same_exported_identity(account: dict[str, Any], exported: dict[str, Any]) -> bool:
    return (
        str(account.get("name") or "") == str(exported.get("name") or "")
        and str(account.get("platform") or "") == str(exported.get("platform") or "")
        and str(account.get("type") or "") == str(exported.get("type") or "")
    )


def same_account_identity(account: dict[str, Any], current: dict[str, Any]) -> bool:
    try:
        return int(account.get("id")) == int(current.get("id")) and same_exported_identity(
            account, current
        )
    except (TypeError, ValueError):
        return False


def validate_base_url(base_url: str) -> str:
    normalized = base_url.strip().rstrip("/")
    parsed = urllib.parse.urlparse(normalized)
    if parsed.scheme not in {"http", "https"} or parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        raise RecoveryError("Sub2API base URL must use HTTP(S) on loopback")
    if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in {"", "/"}:
        raise RecoveryError("Sub2API base URL must not contain credentials, path, query, or fragment")
    return normalized


def is_soft_deleted(account: dict[str, Any]) -> bool:
    deleted_at = account.get("deleted_at")
    return deleted_at is not None and str(deleted_at).strip() != ""


def default_policy() -> dict[str, Any]:
    return {"version": 1, "site_host": "agentrouter.org", "groups": [
        {"id": group_id, "label": label, "aliases": [group_id], "top_n": 3}
        for group_id, label in (("gpt", "GPT"), ("claude", "Claude"), ("glm", "GLM"), ("deepseek", "DeepSeek"))
    ]}


def validate_policy(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != {"version", "site_host", "groups"}:
        raise RecoveryError("恢复规则只接受 version、site_host 和 groups")
    if type(value["version"]) is not int or value["version"] != 1 or value["site_host"] != "agentrouter.org":
        raise RecoveryError("目前只支持已核实的 AgentRouter 站点")
    if not isinstance(value["groups"], list) or not 1 <= len(value["groups"]) <= 12:
        raise RecoveryError("请配置 1 至 12 种账号类型")
    ids: set[str] = set()
    aliases: set[str] = set()
    groups = []
    for group in value["groups"]:
        if not isinstance(group, dict) or set(group) != {"id", "label", "aliases", "top_n"}:
            raise RecoveryError("类型字段只接受 id、label、aliases、top_n")
        group_id, label = group["id"], group["label"]
        if not isinstance(group_id, str) or not re.fullmatch(r"[a-z][a-z0-9_]{0,31}", group_id) or group_id in ids:
            raise RecoveryError("类型 ID 必须唯一，且由小写字母、数字或下划线组成")
        if not isinstance(label, str) or not 1 <= len(label.strip()) <= 40 or any(ord(c) < 32 for c in label):
            raise RecoveryError("类型名称应为 1 至 40 个可见字符")
        if type(group["top_n"]) is not int or not 1 <= group["top_n"] <= 20:
            raise RecoveryError("每种类型开放数量应为 1 至 20")
        names = group["aliases"]
        if not isinstance(names, list) or not 1 <= len(names) <= 10:
            raise RecoveryError("每种类型需有 1 至 10 个后缀别名")
        normalized = []
        for name in names:
            if not isinstance(name, str) or not re.fullmatch(r"[a-zA-Z0-9_]{1,32}", name):
                raise RecoveryError("后缀只接受字母、数字和下划线，不包含前面的连字符")
            name = name.casefold()
            if name in aliases:
                raise RecoveryError(f"后缀 {name} 重复，不能同时归入多个类型")
            aliases.add(name)
            normalized.append(name)
        ids.add(group_id)
        groups.append({"id": group_id, "label": label.strip(), "aliases": normalized, "top_n": group["top_n"]})
    return {"version": 1, "site_host": "agentrouter.org", "groups": groups}


def load_policy(path: Path = DEFAULT_POLICY_PATH) -> dict[str, Any]:
    # Missing production policy is a configuration error; never silently fall
    # back to code defaults after an operator saved a policy.
    try:
        st = path.lstat()
        if path.is_symlink() or not path.is_file() or st.st_uid != os.geteuid() or st.st_mode & 0o077:
            raise RecoveryError("恢复规则文件必须由服务用户持有，权限为 0600")
        if st.st_size > 32768:
            raise RecoveryError("恢复规则文件过大")
        return validate_policy(json.loads(path.read_text(encoding="utf-8")))
    except (OSError, ValueError) as exc:
        raise RecoveryError("无法读取恢复规则文件，请检查配置") from exc


def policy_revision(policy: dict[str, Any]) -> str:
    return hashlib.sha256(json.dumps(policy, sort_keys=True).encode()).hexdigest()[:16]


def matches_site(base_url: str, host: str = "agentrouter.org") -> bool:
    try:
        parsed = urllib.parse.urlsplit(base_url)
        return (parsed.scheme == "https" and parsed.hostname == host
                and not parsed.username and not parsed.password and not parsed.query and not parsed.fragment)
    except ValueError:
        return False


def agentrouter_group_suffix(account_name: str, policy: dict[str, Any] | None = None) -> str | None:
    suffix = str(account_name or "").casefold().rsplit("-", 1)[-1]
    return next((g["id"] for g in (policy or default_policy())["groups"] if suffix in g["aliases"]), None)


def parse_agentrouter_balance(notes: Any) -> Decimal | None:
    matches = AGENTROUTER_BALANCE_RE.findall(str(notes or ""))
    if not matches:
        return None
    try:
        return Decimal(matches[-1])
    except InvalidOperation:
        return None


def record_account_failure(
    summary: dict[str, Any],
    stage: str,
    account_id: int,
    exc: Exception,
) -> None:
    summary[f"{stage}_failed"].append(account_id)
    if account_id not in summary["failed"]:
        summary["failed"].append(account_id)
    key = str(account_id)
    diagnostic = f"{stage}: {type(exc).__name__}: {exc}"
    previous = summary["account_errors"].get(key)
    summary["account_errors"][key] = (
        f"{previous}; {diagnostic}" if previous else diagnostic
    )


def run_agentrouter_recovery_cycle(
    client: RecoveryClient,
    checkpoint: Callable[[], None] | None = None,
    *,
    policy: dict[str, Any] | None = None,
    dry_run: bool = False,
) -> dict[str, Any]:
    policy = validate_policy(policy or default_policy())
    summary: dict[str, Any] = {
        "targets": [],
        "skipped_deleted": [],
        "skipped_url_mismatch": [],
        "skipped_unknown_suffix": [],
        "skipped_invalid_balance": [],
        "skipped_inactive": [],
        "skipped_recover_failed": [],
        "schedulable_disabled": [],
        "groups": {},
        "selected": [],
        "recovered": [],
        "schedulable_enabled": [],
        "discover_failed": [],
        "recover_failed": [],
        "disable_failed": [],
        "enable_failed": [],
        "failed": [],
        "account_errors": {},
        "policy_revision": policy_revision(policy),
        "policy_errors": [],
        "site_targets": [],
        "unchanged_accounts": [],
        "preview_groups": [],
        "dry_run": dry_run,
    }
    discovered_accounts = client.list_accounts()
    discovered_by_id: dict[int, dict[str, Any]] = {}
    for account in discovered_accounts:
        try:
            account_id = int(account["id"])
        except (TypeError, ValueError) as exc:
            raise RecoveryError("agentrouter recovery discovery returned an invalid account ID") from exc
        if account_id in discovered_by_id:
            raise RecoveryError(f"agentrouter recovery discovery returned duplicate account {account_id}")
        discovered_by_id[account_id] = account

    verified_accounts: dict[int, dict[str, Any]] = {}
    for account_id in sorted(discovered_by_id):
        try:
            discovered = discovered_by_id[account_id]
            current = client.get_account(account_id)
            if not same_account_identity(discovered, current):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} changed identity before processing"
                )
            if is_soft_deleted(current):
                summary["skipped_deleted"].append(account_id)
                continue

            base_url = client.account_base_url(current)
            if not matches_site(base_url, policy["site_host"]):
                summary["skipped_url_mismatch"].append(account_id)
                continue
            summary["site_targets"].append(account_id)
            if agentrouter_group_suffix(current.get("name"), policy) is None:
                summary["skipped_unknown_suffix"].append(account_id)
                summary["unchanged_accounts"].append({"id": account_id, "name": current.get("name"),
                    "schedulable": bool(current.get("schedulable")), "reason": "未匹配类型后缀"})
                continue
            verified_accounts[account_id] = current
        except Exception as exc:
            record_account_failure(summary, "discover", account_id, exc)

        if checkpoint is not None:
            checkpoint()

    # Classify before any mutation. A renamed/empty group or missing balances
    # must not turn every account off and then report a successful empty result.
    for group in policy["groups"]:
        matched = [a for a in verified_accounts.values() if agentrouter_group_suffix(a.get("name"), policy) == group["id"]]
        ranked = []
        for a in matched:
            balance = parse_agentrouter_balance(a.get('notes'))
            if balance is None:
                summary["skipped_invalid_balance"].append(int(a["id"]))
            else:
                ranked.append((balance, int(a['id']), a))
        ranked.sort(key=lambda row: (-row[0], row[1]))
        if not ranked:
            summary["policy_errors"].append(f"{group['label']}：未匹配账号或没有可用余额，已保留原账号开关")
            for a in matched:
                verified_accounts.pop(int(a["id"]), None)
        summary["preview_groups"].append({
            "id": group["id"], "label": group["label"], "top_n": group["top_n"],
            "matched": len(matched), "eligible": len(ranked),
            "accounts": [{"id": i, "name": a.get("name"), "balance": str(b),
                          "schedulable": bool(a.get("schedulable"))} for b, i, a in ranked] + [
                {"id": int(a['id']), "name": a.get('name'), "balance": None,
                 "schedulable": bool(a.get('schedulable')), "preserved": not bool(ranked)}
                for a in matched if int(a['id']) in summary['skipped_invalid_balance']],
            "selected": [i for _, i, _ in ranked[:group["top_n"]]],
        })
    target_ids = sorted(verified_accounts)
    summary["targets"] = target_ids
    if dry_run:
        summary["selected"] = [i for group in summary["preview_groups"] for i in group["selected"]]
        return summary

    def check_target(account_id: int) -> dict[str, Any]:
        current = client.get_account(account_id)
        if not same_account_identity(discovered_by_id[account_id], current) or is_soft_deleted(current):
            raise RecoveryError(f'account {account_id} identity changed before mutation')
        if not matches_site(client.account_base_url(current), policy['site_host']):
            raise RecoveryError(f'account {account_id} changed upstream before mutation')
        return current

    # The UI recovery endpoint also clears runtime blocks on active accounts.
    # Finish recovery for every target before starting the global disable phase.
    recover_failed_ids: set[int] = set()
    for account_id in target_ids:
        if account_id not in verified_accounts:
            continue
        try:
            check_target(account_id)
            client.recover_state(account_id)
            current = client.get_account(account_id)
            if not same_account_identity(discovered_by_id[account_id], current):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} changed identity during recovery"
                )
            if is_soft_deleted(current):
                raise RecoveryError(f"agentrouter recovery account {account_id} was deleted")
            if str(current.get("status") or "").casefold() != "active":
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} did not become active"
                )
            verified_accounts[account_id] = current
            summary["recovered"].append(account_id)
        except Exception as exc:
            recover_failed_ids.add(account_id)
            record_account_failure(summary, "recover", account_id, exc)

        if checkpoint is not None:
            checkpoint()

    # Disable is also a complete global phase. A failed recovery does not stop
    # the account from being disabled, and only verified false states advance.
    disabled_accounts: dict[int, dict[str, Any]] = {}
    for account_id in target_ids:
        if account_id not in verified_accounts:
            continue
        try:
            check_target(account_id)
            client.set_schedulable(account_id, False)
            current = client.get_account(account_id)
            if not same_account_identity(discovered_by_id[account_id], current):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} changed identity while disabling"
                )
            if is_soft_deleted(current):
                raise RecoveryError(f"agentrouter recovery account {account_id} was deleted")
            if bool(current.get("schedulable")):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} did not become non-schedulable"
                )
            disabled_accounts[account_id] = current
            summary["schedulable_disabled"].append(account_id)
        except Exception as exc:
            record_account_failure(summary, "disable", account_id, exc)

        if checkpoint is not None:
            checkpoint()

    group_candidates: dict[str, list[tuple[Decimal, int]]] = {
        group["id"]: [] for group in policy["groups"]
    }
    for account_id in summary["schedulable_disabled"]:
        current = disabled_accounts[account_id]
        if account_id in recover_failed_ids:
            summary["skipped_recover_failed"].append(account_id)
            continue
        if str(current.get("status") or "").casefold() != "active":
            summary["skipped_inactive"].append(account_id)
            continue
        suffix = agentrouter_group_suffix(current.get("name"), policy)
        if suffix is None:
            summary["skipped_unknown_suffix"].append(account_id)
            continue
        balance = parse_agentrouter_balance(current.get("notes"))
        if balance is None:
            if account_id not in summary["skipped_invalid_balance"]:
                summary["skipped_invalid_balance"].append(account_id)
            continue
        group_candidates[suffix].append((balance, account_id))

    selected_ids: list[int] = []
    for group in policy["groups"]:
        suffix = group["id"]
        candidates = sorted(
            group_candidates[suffix],
            key=lambda item: (-item[0], item[1]),
        )
        summary["groups"][suffix] = [
            {"id": account_id, "balance": str(balance)}
            for balance, account_id in candidates
        ]
        selected_ids.extend(
            account_id
            for _balance, account_id in candidates[:group["top_n"]]
        )
    summary["selected"] = selected_ids

    for account_id in selected_ids:
        try:
            current = check_target(account_id)
            if not same_account_identity(discovered_by_id[account_id], current):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} changed identity before enabling"
                )
            if is_soft_deleted(current):
                raise RecoveryError(f"agentrouter recovery account {account_id} was deleted")

            current_status = str(current.get("status") or "").casefold()
            if current_status != "active":
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} is not active before enabling"
                )

            client.set_schedulable(account_id, True)
            current = client.get_account(account_id)
            if not same_account_identity(discovered_by_id[account_id], current):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} changed identity while enabling scheduling"
                )
            if (
                str(current.get("status") or "").casefold() != "active"
                or not bool(current.get("schedulable"))
            ):
                raise RecoveryError(
                    f"agentrouter recovery account {account_id} did not become active and schedulable"
                )
            summary["schedulable_enabled"].append(account_id)
        except Exception as exc:
            record_account_failure(summary, "enable", account_id, exc)

        if checkpoint is not None:
            checkpoint()
    return summary


def atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix='.recovery-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as stream:
            stream.write(json.dumps(payload, sort_keys=True, indent=2) + '\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def load_state(path: Path) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {"version": 1}
    except (OSError, json.JSONDecodeError) as exc:
        raise RecoveryError(f"invalid state file: {path}") from exc
    if not isinstance(payload, dict):
        raise RecoveryError(f"invalid state file: {path}")
    payload["version"] = 1
    return payload


def next_agentrouter_recovery_run_at(now: int) -> int:
    local_now = datetime.fromtimestamp(now, LOCAL_TIMEZONE)
    for hour in AGENTROUTER_RECOVERY_HOURS:
        candidate = local_now.replace(
            hour=hour,
            minute=AGENTROUTER_RECOVERY_MINUTE,
            second=0,
            microsecond=0,
        )
        if candidate.timestamp() > now:
            return int(candidate.timestamp())
    next_day = (local_now + timedelta(days=1)).replace(
        hour=AGENTROUTER_RECOVERY_HOURS[0],
        minute=AGENTROUTER_RECOVERY_MINUTE,
        second=0,
        microsecond=0,
    )
    return int(next_day.timestamp())


def reconcile_agentrouter_recovery_schedule(state: dict[str, Any], now: int) -> bool:
    next_run_at = next_agentrouter_recovery_run_at(now)
    if int(state.get("next_run_at") or 0) == next_run_at:
        return False
    state["last_interval_seconds"] = next_run_at - now
    state["next_run_at"] = next_run_at
    return True


def read_admin_key(path: Path) -> str:
    resolved = path.expanduser().resolve()
    if not resolved.is_file():
        raise RecoveryError(f"admin key file does not exist: {resolved}")
    if os.name != "nt" and resolved.stat().st_mode & 0o077:
        raise RecoveryError("admin key file must not be group/world accessible")
    key = resolved.read_text(encoding="utf-8").strip()
    if not key:
        raise RecoveryError("admin key file is empty")
    return key


def env(name: str, default: str) -> str:
    return os.environ.get(name, default)


def parse_branch_enabled(value: str, field: str, *, default: bool) -> bool:
    normalized = value.strip().casefold()
    if not normalized:
        return default
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise RecoveryError(f"{field} must be true or false")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Scheduled Sub2API AgentRouter account recovery")
    parser.add_argument("--base-url", default=env("SUB2API_RECOVERY_BASE_URL", "http://127.0.0.1:13080"))
    parser.add_argument("--admin-key-file", default=env("SUB2API_RECOVERY_ADMIN_KEY_FILE", ""))
    parser.add_argument(
        "--agentrouter-enabled",
        default=env("SUB2API_RECOVERY_AGENTROUTER_ENABLED", ""),
    )
    parser.add_argument(
        "--agentrouter-state-file",
        default=env(
            "SUB2API_RECOVERY_AGENTROUTER_STATE_FILE",
            "/var/lib/server-scheduled-tasks/sub2api-agentrouter-recovery-state.json",
        ),
    )
    parser.add_argument(
        "--request-timeout",
        type=float,
        default=float(env("SUB2API_RECOVERY_REQUEST_TIMEOUT", "150")),
    )
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--preview", action="store_true", help="Read account selection without mutations")
    parser.add_argument("--policy-file", type=Path, default=DEFAULT_POLICY_PATH)
    return parser.parse_args()


def log_event(event: str, **fields: Any) -> None:
    safe = {"event": event, **fields}
    print(json.dumps(safe, sort_keys=True, separators=(",", ":")), flush=True)


def main() -> int:
    args = parse_args()
    agentrouter_enabled = parse_branch_enabled(
        args.agentrouter_enabled,
        "SUB2API_RECOVERY_AGENTROUTER_ENABLED",
        default=False,
    )
    if not agentrouter_enabled:
        log_event("disabled", agentrouter_enabled=False)
        return 0
    if not args.admin_key_file:
        raise RecoveryError("SUB2API_RECOVERY_ADMIN_KEY_FILE is required")
    policy = load_policy(args.policy_file)
    if args.preview:
        client = Sub2APIClient(args.base_url, read_admin_key(Path(args.admin_key_file)), args.request_timeout)
        print(json.dumps(run_agentrouter_recovery_cycle(client, policy=policy, dry_run=True), ensure_ascii=False))
        return 0
    lock_path = Path("/run/server-scheduled-tasks/sub2api-recovery.lock")
    lock_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock_file = lock_path.open("a+", encoding="utf-8")
    os.chmod(lock_path, 0o600)
    try:
        fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as exc:
        raise RecoveryError("another recovery worker already holds the state lock") from exc

    client = Sub2APIClient(
        args.base_url,
        read_admin_key(Path(args.admin_key_file)),
        args.request_timeout,
    )
    state_path = Path(args.agentrouter_state_file).expanduser().resolve()
    state = load_state(state_path)

    def run_agentrouter_branch() -> dict[str, Any]:
        return run_agentrouter_recovery_cycle(
            client,
            checkpoint=lambda: atomic_write_json(state_path, state),
            policy=policy,
        )

    startup_now = int(time.time())
    if args.once:
        state['schedule_source'] = 'systemd'
        state.pop('next_run_at', None)
        state.pop('last_interval_seconds', None)
    elif reconcile_agentrouter_recovery_schedule(state, startup_now):
        atomic_write_json(state_path, state)
        log_event(
            "schedule_adjusted",
            branch="agentrouter_recovery",
            interval_seconds=state["last_interval_seconds"],
            next_run_at=state["next_run_at"],
        )

    stop_requested = False

    def request_stop(_signum: int, _frame: Any) -> None:
        nonlocal stop_requested
        stop_requested = True

    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)

    while not stop_requested:
        now = int(time.time())
        if args.once:
            started_at = time.time()
            state["running"] = True
            state["started_at"] = started_at
            state['invocation_id'] = os.environ.get('INVOCATION_ID')
            atomic_write_json(state_path, state)
            try:
                summary = run_agentrouter_branch()
            except Exception as exc:
                summary = {"error_type": type(exc).__name__, "failed": [], "policy_errors": ["恢复执行失败，请查看服务日志"]}
            state["running"] = False
            state["finished_at"] = time.time()
            state["duration_seconds"] = round(state["finished_at"] - started_at, 3)
            log_event("cycle", branch="agentrouter_recovery", **summary)
            state["last_cycle_at"] = int(time.time())
            state["last_cycle"] = summary
            atomic_write_json(state_path, state)
            return 1 if summary.get("failed") or summary.get("policy_errors") or summary.get("error_type") else 0

        if int(state.get("next_run_at") or 0) <= 0:
            state["next_run_at"] = next_agentrouter_recovery_run_at(now)
            state["last_interval_seconds"] = state["next_run_at"] - now
            atomic_write_json(state_path, state)
            log_event(
                "scheduled",
                branch="agentrouter_recovery",
                interval_seconds=state["last_interval_seconds"],
                next_run_at=state["next_run_at"],
            )

        if now < int(state.get("next_run_at") or 0):
            time.sleep(min(30, max(1, int(state["next_run_at"]) - now)))
            continue

        try:
            summary = run_agentrouter_branch()
            log_event("cycle", branch="agentrouter_recovery", **summary)
            state["last_cycle_at"] = int(time.time())
            state["last_cycle"] = summary
        except Exception as exc:
            log_event(
                "cycle_error",
                branch="agentrouter_recovery",
                error_type=type(exc).__name__,
            )
            state["last_cycle_at"] = int(time.time())
            state["last_cycle"] = {"error_type": type(exc).__name__}

        scheduled_at = int(time.time())
        state["next_run_at"] = next_agentrouter_recovery_run_at(scheduled_at)
        state["last_interval_seconds"] = state["next_run_at"] - scheduled_at
        atomic_write_json(state_path, state)
        log_event(
            "scheduled",
            branch="agentrouter_recovery",
            interval_seconds=state["last_interval_seconds"],
            next_run_at=state["next_run_at"],
        )

    log_event("stopped")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except RecoveryError as exc:
        print(f"recovery worker configuration error: {exc}", file=sys.stderr)
        raise SystemExit(2)
