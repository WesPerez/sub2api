#!/usr/bin/env python3
"""Reconcile Sub2API accounts onto a small set of managed egress profiles."""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
from typing import Any
import urllib.error
import urllib.parse
import urllib.request


SCHEMA_VERSION = 2
ACCOUNT_ID_TEMPLATE = "{{account_id}}"
PROFILE_KEY_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")
ACCOUNT_PLATFORM_RE = re.compile(r"^[a-z0-9_-]+$")
STATIC_PROXY_USERNAME_RE = re.compile(r"^[A-Za-z0-9._-]{1,64}$")


class ReconcileError(RuntimeError):
    pass


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(",", ":"))


def atomic_json(path: Path, value: Any, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        os.fchmod(fd, mode)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(value, ensure_ascii=True, sort_keys=True, indent=2) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temporary.unlink()


def load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ReconcileError(f"cannot read JSON {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise ReconcileError(f"JSON root must be an object: {path}")
    return value


def check_private_file(path: Path, label: str) -> str:
    try:
        metadata = path.lstat()
    except OSError as exc:
        raise ReconcileError(f"cannot stat {label}: {path}: {exc}") from exc
    if path.is_symlink() or not stat.S_ISREG(metadata.st_mode):
        raise ReconcileError(f"{label} must be a regular non-symlink file: {path}")
    if stat.S_IMODE(metadata.st_mode) not in {0o400, 0o600}:
        raise ReconcileError(f"{label} must have mode 0400 or 0600: {path}")
    value = path.read_text(encoding="utf-8").strip()
    if not value:
        raise ReconcileError(f"{label} is empty: {path}")
    return value


def validate_config(config: dict[str, Any]) -> dict[str, Any]:
    if config.get("schema_version") != SCHEMA_VERSION:
        raise ReconcileError(f"schema_version must be {SCHEMA_VERSION}")
    state_dir = Path(str(config.get("state_dir") or ""))
    if not state_dir.is_absolute():
        raise ReconcileError("state_dir must be absolute")
    sub2 = config.get("sub2api")
    profiles = config.get("profiles")
    if not isinstance(sub2, dict) or not isinstance(profiles, list) or not profiles:
        raise ReconcileError("sub2api object and non-empty profiles array are required")
    base_url = str(sub2.get("base_url") or "").rstrip("/")
    if not base_url.startswith("http://127.0.0.1:"):
        raise ReconcileError("Sub2API admin base_url must use IPv4 loopback")
    operating = config.get("operating", {})
    if not isinstance(operating, dict) or set(operating) - {"audit_only", "sub2_timeout_seconds", "resin_timeout_seconds"}:
        raise ReconcileError("invalid operational settings")
    operating = {"audit_only": False, "sub2_timeout_seconds": 20, "resin_timeout_seconds": 10, **operating}
    if type(operating['audit_only']) is not bool:
        raise ReconcileError("audit_only must be boolean")
    for key, maximum in (("sub2_timeout_seconds", 20), ("resin_timeout_seconds", 10)):
        if type(operating[key]) is not int or not 5 <= operating[key] <= maximum:
            raise ReconcileError("operational timeout exceeds bounds")
    config = {**config, "operating": operating}

    keys: set[str] = set()
    names: set[str] = set()
    defaults: dict[str, str] = {}
    token_refs: set[str] = set()
    normalized_profiles: list[dict[str, Any]] = []
    for raw in profiles:
        if not isinstance(raw, dict):
            raise ReconcileError("profile entries must be objects")
        profile = dict(raw)
        key = str(profile.get("key") or "")
        name = str(profile.get("name") or "")
        data_plane = str(profile.get("data_plane") or "resin")
        platform_name = str(profile.get("resin_platform") or "")
        protocol = str(profile.get("protocol") or "socks5h")
        host = str(profile.get("host") or "")
        port = int(profile.get("port") or 0)
        username_template = str(profile.get("username_template") or "")
        token_ref = str(profile.get("token_ref") or "")
        inherit_legacy_lease = profile.get("inherit_legacy_lease", False)
        allowed = [str(value).lower() for value in profile.get("allowed_account_platforms") or []]
        default_for = [str(value).lower() for value in profile.get("default_for") or []]
        if not PROFILE_KEY_RE.fullmatch(key) or key in keys:
            raise ReconcileError(f"invalid or duplicate profile key: {key}")
        if not name or name in names:
            raise ReconcileError(f"invalid or duplicate profile name: {name}")
        if data_plane not in {"resin", "direct"}:
            raise ReconcileError(f"invalid data plane for profile {key}")
        if protocol not in {"socks5", "socks5h"} or not host or port < 1 or port > 65535:
            raise ReconcileError(f"invalid network endpoint for profile {key}")
        if data_plane == "resin":
            if not platform_name or any(char in platform_name for char in ".:|/\\@?#%~"):
                raise ReconcileError(f"invalid Resin platform for profile {key}")
            if username_template.count(ACCOUNT_ID_TEMPLATE) != 1:
                raise ReconcileError(
                    f"profile {key} username_template must contain one {ACCOUNT_ID_TEMPLATE}"
                )
            if not username_template.startswith(platform_name + "."):
                raise ReconcileError(
                    f"profile {key} username_template must start with its Resin platform"
                )
        else:
            if platform_name:
                raise ReconcileError(f"direct profile {key} must not declare a Resin platform")
            if not STATIC_PROXY_USERNAME_RE.fullmatch(username_template):
                raise ReconcileError(f"direct profile {key} must use a static proxy username")
            if inherit_legacy_lease:
                raise ReconcileError(f"direct profile {key} cannot inherit a Resin lease")
        if not token_ref:
            raise ReconcileError(f"profile {key} token_ref is required")
        if not isinstance(inherit_legacy_lease, bool):
            raise ReconcileError(f"profile {key} inherit_legacy_lease must be a boolean")
        if not allowed or any(not ACCOUNT_PLATFORM_RE.fullmatch(value) for value in allowed):
            raise ReconcileError(f"profile {key} allowed_account_platforms is invalid")
        if any(value not in allowed for value in default_for):
            raise ReconcileError(f"profile {key} default_for must be a subset of allowed platforms")
        for platform in default_for:
            if platform in defaults:
                raise ReconcileError(f"multiple default profiles for account platform {platform}")
            defaults[platform] = key
        profile.update({
            "key": key,
            "name": name,
            "data_plane": data_plane,
            "resin_platform": platform_name,
            "protocol": protocol,
            "host": host,
            "port": port,
            "username_template": username_template,
            "token_ref": token_ref,
            "inherit_legacy_lease": inherit_legacy_lease,
            "allowed_account_platforms": sorted(set(allowed)),
            "default_for": sorted(set(default_for)),
        })
        normalized_profiles.append(profile)
        keys.add(key)
        names.add(name)
        token_refs.add(token_ref)
    if not defaults:
        raise ReconcileError("at least one account platform default is required")

    legacy_patterns = config.get("legacy_proxy_name_regexes") or []
    if not isinstance(legacy_patterns, list):
        raise ReconcileError("legacy_proxy_name_regexes must be an array")
    for pattern in legacy_patterns:
        try:
            re.compile(str(pattern))
        except re.error as exc:
            raise ReconcileError(f"invalid legacy proxy regex: {pattern}") from exc

    config["state_dir"] = str(state_dir)
    config["profiles"] = normalized_profiles
    config["legacy_proxy_name_regexes"] = [str(value) for value in legacy_patterns]
    sub2["base_url"] = base_url
    config["_defaults"] = defaults
    config["_token_refs"] = sorted(token_refs)
    return config


@contextlib.contextmanager
def exclusive_lock(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with path.open("a+", encoding="utf-8") as handle:
        os.chmod(path, 0o600)
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise ReconcileError("another reconcile is already running") from exc
        yield


def run_inventory(config: dict[str, Any]) -> dict[str, Any]:
    sub2 = config["sub2api"]
    platforms = sorted(config["_defaults"])
    platform_sql = ",".join("'" + value.replace("'", "''") + "'" for value in platforms)
    sql = f"""
WITH account_rows AS (
  SELECT id,lower(platform) AS platform,proxy_id,status,schedulable,type,parent_account_id,
         COALESCE(credentials->>'base_url','') AS base_url
  FROM accounts
  WHERE deleted_at IS NULL AND lower(platform) IN ({platform_sql})
), proxy_rows AS (
  SELECT id,name,protocol,host,port,COALESCE(username,'') AS username,
         encode(sha256(convert_to(COALESCE(password,''),'UTF8')),'hex') AS password_sha256,
         status,(deleted_at IS NOT NULL) AS deleted
  FROM proxies
)
SELECT json_build_object(
  'accounts', COALESCE((SELECT json_agg(row_to_json(a) ORDER BY a.id) FROM account_rows a), '[]'::json),
  'proxies', COALESCE((SELECT json_agg(row_to_json(p) ORDER BY p.id) FROM proxy_rows p), '[]'::json)
)::text;
""".strip()
    command = [
        str(sub2.get("docker_binary") or "/usr/bin/docker"),
        "exec", "-i",
        "-e", "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=15000",
        str(sub2["postgres_container"]),
        "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1",
        "-U", str(sub2["db_user"]), "-d", str(sub2["db_name"]),
    ]
    try:
        result = subprocess.run(command, input=sql + "\n", check=True, capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
        stderr = getattr(exc, "stderr", "") or ""
        summary = stderr.strip().splitlines()[-1:] or [str(exc)]
        raise ReconcileError(f"Sub2API inventory query failed: {summary[0]}") from exc
    payload_line = next((line.strip() for line in result.stdout.splitlines() if line.lstrip().startswith("{")), "")
    if not payload_line:
        raise ReconcileError("Sub2API inventory query returned no JSON")
    try:
        payload = json.loads(payload_line)
    except json.JSONDecodeError as exc:
        raise ReconcileError("Sub2API inventory query returned invalid JSON") from exc
    if not isinstance(payload, dict):
        raise ReconcileError("Sub2API inventory payload is not an object")
    return payload


class Sub2Client:
    def __init__(self, base_url: str, admin_key: str, timeout: float = 20) -> None:
        self.base_url = base_url
        self.admin_key = admin_key
        self.timeout = timeout
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> Any:
        headers = {"x-api-key": self.admin_key}
        if extra_headers:
            headers.update(extra_headers)
        data = None
        if body is not None:
            data = canonical_json(body).encode()
            headers["Content-Type"] = "application/json; charset=utf-8"
        request = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                raw = response.read(2 * 1024 * 1024)
                status = int(response.status)
        except urllib.error.HTTPError as exc:
            raw = exc.read(2 * 1024 * 1024)
            raise ReconcileError(
                f"Sub2API {method} {path} returned HTTP {exc.code}: "
                f"{raw[:512].decode('utf-8', 'replace')}"
            ) from exc
        except (OSError, urllib.error.URLError) as exc:
            raise ReconcileError(f"Sub2API {method} {path} failed: {exc}") from exc
        if status < 200 or status >= 300:
            raise ReconcileError(f"Sub2API {method} {path} returned HTTP {status}")
        try:
            payload = json.loads(raw) if raw else {}
        except json.JSONDecodeError as exc:
            raise ReconcileError(f"Sub2API {method} {path} returned invalid JSON") from exc
        if isinstance(payload, dict) and "code" in payload:
            if int(payload.get("code") or 0) != 0:
                raise ReconcileError(
                    f"Sub2API {method} {path} failed: {payload.get('message') or payload.get('code')}"
                )
            return payload.get("data")
        return payload


class ResinClient:
    def __init__(self, timeout: float = 10) -> None:
        self.timeout = timeout
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def inherit_lease(
        self,
        profile: dict[str, Any],
        token: str,
        parent_account: str,
        new_account: str,
    ) -> bool:
        host = str(profile["host"])
        if ":" in host and not host.startswith("["):
            host = f"[{host}]"
        token_segment = urllib.parse.quote(token, safe="")
        platform_segment = urllib.parse.quote(str(profile["resin_platform"]), safe="")
        url = (
            f"http://{host}:{int(profile['port'])}/{token_segment}/api/v1/"
            f"{platform_segment}/actions/inherit-lease"
        )
        body = canonical_json({
            "parent_account": parent_account,
            "new_account": new_account,
        }).encode()
        request = urllib.request.Request(
            url,
            data=body,
            headers={"Content-Type": "application/json; charset=utf-8"},
            method="POST",
        )
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                raw = response.read(1024 * 1024)
                status = int(response.status)
        except urllib.error.HTTPError as exc:
            raw = exc.read(1024 * 1024)
            if exc.code == 404:
                try:
                    payload = json.loads(raw) if raw else {}
                except json.JSONDecodeError:
                    payload = {}
                error = payload.get("error") if isinstance(payload, dict) else None
                message = (
                    str(error.get("message") or "").strip().casefold()
                    if isinstance(error, dict)
                    else ""
                )
                if message == "parent lease not found":
                    return False
            summary = raw[:512].decode("utf-8", "replace")
            raise ReconcileError(
                f"Resin inherit lease for profile {profile['key']} returned HTTP {exc.code}: {summary}"
            ) from exc
        except (OSError, urllib.error.URLError) as exc:
            raise ReconcileError(
                f"Resin inherit lease for profile {profile['key']} failed: {exc}"
            ) from exc
        if status < 200 or status >= 300:
            raise ReconcileError(
                f"Resin inherit lease for profile {profile['key']} returned HTTP {status}"
            )
        try:
            payload = json.loads(raw) if raw else {}
        except json.JSONDecodeError as exc:
            raise ReconcileError(
                f"Resin inherit lease for profile {profile['key']} returned invalid JSON"
            ) from exc
        if not isinstance(payload, dict) or payload.get("status") != "ok":
            raise ReconcileError(
                f"Resin inherit lease for profile {profile['key']} returned an invalid response"
            )
        return True


def token_sha256(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def profile_payload(profile: dict[str, Any], token: str) -> dict[str, Any]:
    return {
        "name": profile["name"],
        "protocol": profile["protocol"],
        "host": profile["host"],
        "port": profile["port"],
        "username": profile["username_template"],
        "password": token,
        "fallback_mode": "none",
        "expiry_warn_days": 7,
    }


def profile_matches(profile: dict[str, Any], row: dict[str, Any], token: str) -> bool:
    return (
        str(row.get("name") or "") == profile["name"]
        and str(row.get("protocol") or "") == profile["protocol"]
        and str(row.get("host") or "") == profile["host"]
        and int(row.get("port") or 0) == profile["port"]
        and str(row.get("username") or "") == profile["username_template"]
        and str(row.get("password_sha256") or "") == token_sha256(token)
        and str(row.get("status") or "") == "active"
    )


def index_inventory(
    config: dict[str, Any], inventory: dict[str, Any], tokens: dict[str, str]
) -> tuple[list[dict[str, Any]], dict[str, dict[str, Any]], set[int]]:
    accounts = [dict(row) for row in inventory.get("accounts") or []]
    proxies = [dict(row) for row in inventory.get("proxies") or []]
    active = [row for row in proxies if not row.get("deleted")]
    profile_rows: dict[str, dict[str, Any]] = {}
    for profile in config["profiles"]:
        matches = [row for row in active if str(row.get("name") or "") == profile["name"]]
        if len(matches) > 1:
            raise ReconcileError(f"multiple active proxies use profile name {profile['name']}")
        if matches:
            row = matches[0]
            if not profile_matches(profile, row, tokens[profile["token_ref"]]):
                raise ReconcileError(
                    f"profile {profile['key']} differs from config; use a new versioned profile name for rotation"
                )
            profile_rows[profile["key"]] = row
    legacy_ids: set[int] = set()
    patterns = [re.compile(value) for value in config["legacy_proxy_name_regexes"]]
    for row in active:
        name = str(row.get("name") or "")
        if any(pattern.search(name) for pattern in patterns):
            legacy_ids.add(int(row["id"]))
    return accounts, profile_rows, legacy_ids


def analyze_bindings(
    config: dict[str, Any], accounts: list[dict[str, Any]], profile_rows: dict[str, dict[str, Any]]
) -> dict[str, Any]:
    accounts_by_id = {int(row["id"]): row for row in accounts}
    profile_key_by_id = {int(row["id"]): key for key, row in profile_rows.items()}
    desired_key: dict[int, str] = {}

    def resolve(account_id: int, stack: set[int]) -> str:
        if account_id in desired_key:
            return desired_key[account_id]
        if account_id in stack:
            raise ReconcileError(f"shadow parent cycle at account {account_id}")
        account = accounts_by_id.get(account_id)
        if account is None:
            raise ReconcileError(f"account {account_id} is missing from inventory")
        current_id = int(account["proxy_id"]) if account.get("proxy_id") is not None else None
        if current_id is not None:
            selected_profile = profile_key_by_id.get(current_id)
            if selected_profile is not None:
                desired_key[account_id] = selected_profile
                return selected_profile
        parent_id = account.get("parent_account_id")
        if parent_id is not None:
            target = resolve(int(parent_id), {*stack, account_id})
            desired_key[account_id] = target
            return target
        platform = str(account["platform"]).lower()
        target = config["_defaults"].get(platform)
        if not target:
            raise ReconcileError(f"no default proxy profile for account platform {platform}")
        desired_key[account_id] = target
        return target

    for account_id in sorted(accounts_by_id):
        resolve(account_id, set())

    actions: list[dict[str, Any]] = []
    preserved = 0
    for account_id in sorted(accounts_by_id):
        account = accounts_by_id[account_id]
        current_id = int(account["proxy_id"]) if account.get("proxy_id") is not None else None
        target_key = desired_key[account_id]
        target_id = int(profile_rows[target_key]["id"]) if target_key in profile_rows else None
        if target_id is not None and current_id == target_id:
            preserved += 1
            continue
        actions.append({
            "account_id": account_id,
            "from_proxy_id": current_id,
            "to_profile": target_key,
            "to_proxy_id": target_id,
        })
    return {
        "actions": actions,
        "desired_key": desired_key,
        "preserved": preserved,
    }


def create_profile(
    client: Sub2Client, profile: dict[str, Any], token: str
) -> int:
    name_digest = hashlib.sha256(str(profile["name"]).encode("utf-8")).hexdigest()[:16]
    row = client.request(
        "POST",
        "/admin/proxies",
        profile_payload(profile, token),
        {"Idempotency-Key": f"sub2-resin-profile-v2-{profile['key']}-{name_digest}"},
    )
    if not isinstance(row, dict) or int(row.get("id") or 0) <= 0:
        raise ReconcileError(f"create profile {profile['key']} returned no proxy id")
    return int(row["id"])


def update_account_binding(client: Sub2Client, account_id: int, proxy_id: int | None) -> None:
    client.request("PUT", f"/admin/accounts/{account_id}", {"proxy_id": proxy_id or 0})


def delete_legacy_proxies(
    client: Sub2Client, proxy_ids: list[int]
) -> tuple[list[int], list[int]]:
    if not proxy_ids:
        return [], []
    payload = client.request(
        "POST",
        "/admin/proxies/batch-delete",
        {"ids": proxy_ids},
    )
    if not isinstance(payload, dict):
        raise ReconcileError("legacy proxy batch delete returned an invalid response")
    deleted_raw = payload.get("deleted_ids")
    skipped_raw = payload.get("skipped")
    if deleted_raw is None:
        deleted_raw = []
    if skipped_raw is None:
        skipped_raw = []
    if not isinstance(deleted_raw, list) or not isinstance(skipped_raw, list):
        raise ReconcileError("legacy proxy batch delete returned an invalid result shape")
    try:
        deleted = sorted({int(value) for value in deleted_raw})
    except (TypeError, ValueError) as exc:
        raise ReconcileError("legacy proxy batch delete returned invalid proxy IDs") from exc
    skipped_ids = sorted({
        int(row.get("id"))
        for row in skipped_raw
        if isinstance(row, dict) and str(row.get("id") or "").isdigit()
    })
    return deleted, skipped_ids


def resin_account_from_username(profile: dict[str, Any], username: str) -> str:
    prefix = str(profile["resin_platform"]) + "."
    if not username.startswith(prefix):
        raise ReconcileError(
            f"legacy proxy username is outside Resin platform {profile['resin_platform']}"
        )
    account = username[len(prefix):].strip()
    if not account:
        raise ReconcileError("legacy proxy username has an empty Resin account")
    return account


def expanded_resin_account(profile: dict[str, Any], account_id: int) -> str:
    username = str(profile["username_template"]).replace(ACCOUNT_ID_TEMPLATE, str(account_id))
    return resin_account_from_username(profile, username)


def plan_lease_inheritance(
    profiles: dict[str, dict[str, Any]],
    accounts: list[dict[str, Any]],
    proxies: list[dict[str, Any]],
    legacy_ids: set[int],
    actions: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    accounts_by_id = {int(row["id"]): row for row in accounts}
    proxies_by_id = {int(row["id"]): row for row in proxies}
    planned: list[dict[str, Any]] = []
    seen: set[tuple[str, str, str]] = set()
    for action in actions:
        target_key = action.get("to_profile")
        if target_key is None:
            continue
        profile = profiles[str(target_key)]
        if not profile["inherit_legacy_lease"]:
            continue
        old_id = action.get("from_proxy_id")
        if old_id is None or int(old_id) not in legacy_ids:
            continue
        source_proxy = proxies_by_id.get(int(old_id))
        if source_proxy is None:
            raise ReconcileError(f"legacy proxy {old_id} is missing from inventory")
        account_id = int(action["account_id"])
        account = accounts_by_id[account_id]
        parent_id = account.get("parent_account_id")
        identity_id = int(parent_id) if parent_id is not None and int(parent_id) > 0 else account_id
        parent_account = resin_account_from_username(profile, str(source_proxy.get("username") or ""))
        new_account = expanded_resin_account(profile, identity_id)
        identity = (str(target_key), parent_account, new_account)
        if identity in seen:
            continue
        seen.add(identity)
        planned.append({
            "account_id": account_id,
            "profile": str(target_key),
            "parent_account": parent_account,
            "new_account": new_account,
        })
    return planned


def desired_fingerprint(config: dict[str, Any], tokens: dict[str, str]) -> str:
    payload = {
        "profiles": config["profiles"],
        "token_sha256": {key: token_sha256(value) for key, value in sorted(tokens.items())},
    }
    return hashlib.sha256(canonical_json(payload).encode()).hexdigest()


def state_payload(
    config: dict[str, Any],
    tokens: dict[str, str],
    profile_rows: dict[str, dict[str, Any]],
) -> dict[str, Any]:
    return {
        "schema_version": SCHEMA_VERSION,
        "updated_at": utc_now(),
        "desired_fingerprint": desired_fingerprint(config, tokens),
        "profiles": {
            key: int(row["id"])
            for key, row in sorted(profile_rows.items())
        },
    }


def reconcile(
    config: dict[str, Any],
    admin_key: str,
    tokens: dict[str, str],
    apply: bool,
    cleanup_unreferenced_legacy: bool = False,
) -> dict[str, Any]:
    state_dir = Path(config["state_dir"])
    state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(state_dir, 0o700)
    lock_path = Path(str(config.get("lock_file") or state_dir / "reconcile.lock"))
    profiles = {profile["key"]: profile for profile in config["profiles"]}
    with exclusive_lock(lock_path):
        inventory = run_inventory(config)
        accounts, profile_rows, legacy_ids = index_inventory(config, inventory, tokens)
        missing_profiles = sorted(set(profiles) - set(profile_rows))
        analysis = analyze_bindings(config, accounts, profile_rows)
        lease_plan = plan_lease_inheritance(
            profiles,
            accounts,
            [dict(row) for row in inventory.get("proxies") or []],
            legacy_ids,
            analysis["actions"],
        )
        plan = {
            "accounts": len(accounts),
            "configured_profiles": len(profiles),
            "active_profiles": len(profile_rows),
            "create_profiles": missing_profiles,
            "binding_changes": analysis["actions"],
            "preserved_bindings": analysis["preserved"],
            "lease_inheritance_candidates": sum(
                1 for row in lease_plan if row["parent_account"] != row["new_account"]
            ),
            "lease_already_stable_candidates": sum(
                1 for row in lease_plan if row["parent_account"] == row["new_account"]
            ),
            "referenced_legacy_proxy_ids": sorted(
                legacy_ids
                & {
                    int(row["proxy_id"])
                    for row in accounts
                    if row.get("proxy_id") is not None
                }
            ),
            "unreferenced_legacy_proxy_ids": sorted(
                legacy_ids
                - {
                    int(row["proxy_id"])
                    for row in accounts
                    if row.get("proxy_id") is not None
                }
            ),
            "cleanup_unreferenced_legacy": cleanup_unreferenced_legacy,
        }
        if not apply:
            return {"status": "planned", "plan": plan}
        if not cleanup_unreferenced_legacy and not missing_profiles and not analysis["actions"]:
            atomic_json(state_dir / "state.json", state_payload(config, tokens, profile_rows))
            return {
                "status": "ok",
                "no_changes": True,
                "accounts": len(accounts),
                "profiles": len(profile_rows),
                "created_profiles": 0,
                "rebound_accounts": 0,
                "inherited_leases": 0,
                "already_stable_leases": 0,
                "missing_parent_leases": 0,
                "unreferenced_legacy_proxy_ids": plan["unreferenced_legacy_proxy_ids"],
                "deleted_legacy_proxies": 0,
                "recovery_manifest": None,
            }
        if cleanup_unreferenced_legacy and (missing_profiles or analysis["actions"]):
            raise ReconcileError(
                "legacy cleanup requires an already converged shared-profile migration"
            )

        run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        recovery_path = state_dir / "recovery" / f"{run_id}.json"
        recovery = {
            "schema_version": SCHEMA_VERSION,
            "run_id": run_id,
            "started_at": utc_now(),
            "status": "running",
            "before_bindings": [
                {"account_id": int(row["id"]), "proxy_id": row.get("proxy_id")}
                for row in accounts
            ],
            "created_profile_ids": [],
            "changed_account_ids": [],
            "lease_inheritance": [],
            "deleted_legacy_proxy_ids": [],
            "skipped_legacy_proxy_ids": [],
        }
        atomic_json(recovery_path, recovery)
        client = Sub2Client(config["sub2api"]["base_url"], admin_key, config.get('operating', {}).get('sub2_timeout_seconds', 20))
        changed: list[tuple[int, int | None]] = []
        try:
            for key in missing_profiles:
                profile = profiles[key]
                proxy_id = create_profile(client, profile, tokens[profile["token_ref"]])
                recovery["created_profile_ids"].append(proxy_id)
                atomic_json(recovery_path, recovery)

            refreshed = run_inventory(config)
            refreshed_accounts, refreshed_profiles, refreshed_legacy_ids = index_inventory(
                config, refreshed, tokens
            )
            if set(refreshed_profiles) != set(profiles):
                raise ReconcileError("profile creation readback is incomplete")
            refreshed_analysis = analyze_bindings(config, refreshed_accounts, refreshed_profiles)
            refreshed_lease_plan = plan_lease_inheritance(
                profiles,
                refreshed_accounts,
                [dict(row) for row in refreshed.get("proxies") or []],
                refreshed_legacy_ids,
                refreshed_analysis["actions"],
            )
            resin_client = ResinClient(config.get('operating', {}).get('resin_timeout_seconds', 10))
            for item in refreshed_lease_plan:
                if item["parent_account"] == item["new_account"]:
                    recovery["lease_inheritance"].append({
                        **item,
                        "status": "already_stable",
                    })
                    atomic_json(recovery_path, recovery)
                    continue
                profile = profiles[item["profile"]]
                inherited = resin_client.inherit_lease(
                    profile,
                    tokens[profile["token_ref"]],
                    item["parent_account"],
                    item["new_account"],
                )
                recovery["lease_inheritance"].append({
                    **item,
                    "status": "inherited" if inherited else "parent_not_found",
                })
                atomic_json(recovery_path, recovery)
            for action in refreshed_analysis["actions"]:
                target_id = action["to_proxy_id"]
                account_id = int(action["account_id"])
                old_id = action["from_proxy_id"]
                update_account_binding(client, account_id, target_id)
                changed.append((account_id, int(old_id) if old_id is not None else None))
                recovery["changed_account_ids"].append(account_id)
                atomic_json(recovery_path, recovery)

            final_inventory = run_inventory(config)
            final_accounts, final_profiles, final_legacy_ids = index_inventory(
                config, final_inventory, tokens
            )
            final_analysis = analyze_bindings(config, final_accounts, final_profiles)
            if final_analysis["actions"]:
                raise ReconcileError(
                    f"readback found unresolved bindings: {final_analysis['actions'][:20]}"
                )
            referenced_ids = {
                int(row["proxy_id"])
                for row in final_accounts
                if row.get("proxy_id") is not None
            }
            unreferenced_legacy = sorted(final_legacy_ids - referenced_ids)
            if cleanup_unreferenced_legacy:
                expected_cleanup = sorted(set(unreferenced_legacy))
                deleted_legacy, skipped_legacy = delete_legacy_proxies(
                    client, expected_cleanup
                )
                recovery["deleted_legacy_proxy_ids"] = deleted_legacy
                recovery["skipped_legacy_proxy_ids"] = skipped_legacy
                atomic_json(recovery_path, recovery)
                if skipped_legacy or deleted_legacy != expected_cleanup:
                    raise ReconcileError(
                        "legacy proxy batch delete was incomplete: "
                        f"deleted={len(deleted_legacy)}/{len(expected_cleanup)} "
                        f"skipped_ids={skipped_legacy[:20]}"
                    )
                cleanup_inventory = run_inventory(config)
                _, _, cleanup_legacy_ids = index_inventory(
                    config, cleanup_inventory, tokens
                )
                remaining_deleted_ids = sorted(set(deleted_legacy) & cleanup_legacy_ids)
                if remaining_deleted_ids:
                    raise ReconcileError(
                        "legacy proxy cleanup readback still contains deleted IDs: "
                        f"{remaining_deleted_ids[:20]}"
                    )
                unreferenced_legacy = sorted(cleanup_legacy_ids)
            atomic_json(
                state_dir / "state.json",
                state_payload(config, tokens, final_profiles),
            )
            recovery["status"] = "completed"
            recovery["completed_at"] = utc_now()
            recovery["unreferenced_legacy_proxy_ids"] = unreferenced_legacy
            atomic_json(recovery_path, recovery)
        except Exception as exc:
            rollback_errors: list[str] = []
            for account_id, old_id in reversed(changed):
                try:
                    update_account_binding(client, account_id, old_id)
                except Exception as rollback_exc:
                    rollback_errors.append(f"account {account_id}: {rollback_exc}")
            recovery["status"] = (
                "rolled_back" if changed and not rollback_errors
                else "rollback_incomplete" if rollback_errors
                else "failed_before_binding"
            )
            recovery["error"] = str(exc)
            recovery["rollback_errors"] = rollback_errors
            recovery["completed_at"] = utc_now()
            atomic_json(recovery_path, recovery)
            if rollback_errors:
                raise ReconcileError(
                    f"reconcile failed and rollback was incomplete: {exc}; {rollback_errors}"
                ) from exc
            raise
        return {
            "status": "ok",
            "accounts": len(final_accounts),
            "profiles": len(final_profiles),
            "created_profiles": len(recovery["created_profile_ids"]),
            "rebound_accounts": len(recovery["changed_account_ids"]),
            "inherited_leases": sum(
                1 for row in recovery["lease_inheritance"] if row["status"] == "inherited"
            ),
            "already_stable_leases": sum(
                1
                for row in recovery["lease_inheritance"]
                if row["status"] == "already_stable"
            ),
            "missing_parent_leases": sum(
                1 for row in recovery["lease_inheritance"] if row["status"] == "parent_not_found"
            ),
            "unreferenced_legacy_proxy_ids": unreferenced_legacy,
            "deleted_legacy_proxies": len(recovery["deleted_legacy_proxy_ids"]),
            "recovery_manifest": str(recovery_path),
        }


def parse_token_files(values: list[str]) -> dict[str, Path]:
    result: dict[str, Path] = {}
    for value in values:
        key, separator, raw_path = value.partition("=")
        if not separator or not PROFILE_KEY_RE.fullmatch(key) or not raw_path:
            raise ReconcileError("profile token file must use key=/absolute/path")
        path = Path(raw_path)
        if not path.is_absolute() or key in result:
            raise ReconcileError(f"invalid or duplicate profile token file: {key}")
        result[key] = path
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("--sub2-admin-key-file", required=True)
    parser.add_argument("--profile-token-file", action="append", default=[])
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--cleanup-unreferenced-legacy", action="store_true")
    args = parser.parse_args()
    try:
        config = validate_config(load_json(Path(args.config)))
        admin_key = check_private_file(Path(args.sub2_admin_key_file), "Sub2API admin key")
        token_paths = parse_token_files(args.profile_token_file)
        if set(token_paths) != set(config["_token_refs"]):
            raise ReconcileError("profile token file keys do not match config token_ref values")
        tokens = {
            key: check_private_file(path, f"Resin proxy token {key}")
            for key, path in token_paths.items()
        }
        result = reconcile(
            config,
            admin_key,
            tokens,
            args.apply and not config['operating']['audit_only'],
            args.cleanup_unreferenced_legacy,
        )
        print(canonical_json(result))
        return 0
    except ReconcileError as exc:
        print(canonical_json({"status": "error", "error": str(exc)}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
