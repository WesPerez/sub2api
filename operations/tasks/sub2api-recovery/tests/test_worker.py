from __future__ import annotations

import importlib.util
import json
import sys
from copy import deepcopy
from datetime import datetime
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "worker.py"
SPEC = importlib.util.spec_from_file_location("sub2api_recovery_worker_test", SCRIPT)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class FakeClient:
    def __init__(
        self,
        accounts,
        recover_fail=(),
        disable_no_effect=(),
        recover_fail_after_active=(),
    ):
        self.accounts = accounts
        self.recover_fail = set(recover_fail)
        self.recover_fail_after_active = set(recover_fail_after_active)
        self.disable_no_effect = set(disable_no_effect)
        self.list_calls = []
        self.get_calls = []
        self.recover_calls = []
        self.schedulable_calls = []
        self.events = []

        self.recover_attempts = []

    def list_accounts(self):
        self.list_calls.append(None)
        return [dict(value) for value in self.accounts.values()]

    def get_account(self, account_id):
        if account_id not in self.accounts:
            raise MODULE.RecoveryError(f"account {account_id} disappeared")
        self.get_calls.append(account_id)
        self.events.append(("get", account_id))
        return dict(self.accounts[account_id])

    def account_base_url(self, account_value):
        value = self.accounts[account_value["id"]]
        return str((value.get("credentials") or {}).get("base_url") or "")

    def recover_state(self, account_id):
        self.events.append(("recover", account_id))
        self.recover_attempts.append(account_id)
        self.accounts[account_id]["status"] = "active"
        self.accounts[account_id]["error_message"] = ""
        for field in (
            "rate_limited_at", "rate_limit_reset_at", "overload_until",
            "temp_unschedulable_until", "temp_unschedulable_reason",
        ):
            if field in self.accounts[account_id]:
                self.accounts[account_id][field] = None
        extra = self.accounts[account_id].get("extra")
        if isinstance(extra, dict):
            self.accounts[account_id]["extra"] = {
                key: value for key, value in extra.items()
                if key not in {"model_rate_limits", "antigravity_quota_scopes"}
            }
        if account_id in self.recover_fail:
            raise MODULE.RecoveryError(f"recover account {account_id} failed")
        self.recover_calls.append(account_id)
        if account_id in self.recover_fail_after_active:
            raise MODULE.RecoveryError(f"recover account {account_id} failed after activation")

    def set_schedulable(self, account_id, schedulable):
        self.events.append(("disable" if not schedulable else "enable", account_id))
        self.schedulable_calls.append((account_id, schedulable))
        if schedulable or account_id not in self.disable_no_effect:
            self.accounts[account_id]["schedulable"] = schedulable


def account(
    account_id,
    status="active",
    error="",
    schedulable=False,
    name=None,
    platform="openai",
    type="apikey",
    notes="AgentRouter余额 0.00",
    base_url="https://agentrouter.org/v1",
):
    return {
        "id": account_id,
        "name": name or f"agentrouter-{account_id}",
        "platform": platform,
        "type": type,
        "status": status,
        "error_message": error,
        "schedulable": schedulable,
        "notes": notes,
        "credentials": {"base_url": base_url},
    }


def test_agentrouter_recovers_errors_then_disables_all_and_enables_top_three():
    accounts = {
        3001: account(3001, "error", "upstream 500", False, name="agentrouter-1-gpt", notes="AgentRouter余额 10.00"),
        3002: account(3002, "active", "", False, name="agentrouter-2-gpt", notes="AgentRouter余额 20.00"),
        3003: account(3003, "active", "", True, name="agentrouter-3-gpt", notes="AgentRouter余额 30.00"),
        3004: account(3004, "error", "upstream 500", False, name="agentrouter-1-claude", notes="AgentRouter余额 50.00"),
        3005: account(3005, "active", "", False, name="agentrouter-2-claude", notes="AgentRouter余额 40.00"),
        3006: account(3006, "active", "", False, name="agentrouter-3-claude", notes="AgentRouter余额 45.00"),
        3007: account(3007, "active", "", False, name="agentrouter-1-deepseek", notes="AgentRouter余额 10.00"),
        3008: account(3008, "active", "", True, name="agentrouter-2-deepseek", notes="AgentRouter余额 20.00"),
        3009: account(3009, "active", "", True, name="agentrouter-4-gpt", notes="AgentRouter余额 5.00"),
        3010: account(3010, "active", "", True, name="agentrouter-4-claude", notes="AgentRouter余额 35.00"),
        3011: account(3011, "active", "", False, name="agentrouter-3-deepseek", notes="AgentRouter余额 30.00"),
        3012: account(3012, "active", "", True, name="agentrouter-4-deepseek", notes="AgentRouter余额 5.00"),
    }
    client = FakeClient(accounts)
    original_notes = {
        account_id: value["notes"] for account_id, value in accounts.items()
    }
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["targets"] == list(range(3001, 3013))
    assert summary["schedulable_disabled"] == list(range(3001, 3013))
    assert summary["selected"] == [3003, 3002, 3001, 3004, 3006, 3005, 3011, 3008, 3007]
    assert summary["recovered"] == list(range(3001, 3013))
    assert summary["schedulable_enabled"] == [3003, 3002, 3001, 3004, 3006, 3005, 3011, 3008, 3007]
    assert summary["failed"] == []
    assert client.recover_calls == list(range(3001, 3013))
    assert client.schedulable_calls[:12] == [(account_id, False) for account_id in range(3001, 3013)]
    assert client.schedulable_calls[12:] == [(account_id, True) for account_id in summary["selected"]]
    mutation_events = [
        event for event in client.events if event[0] != "get"
    ]
    assert mutation_events == (
        [("recover", account_id) for account_id in range(3001, 3013)]
        + [("disable", account_id) for account_id in range(3001, 3013)]
        + [("enable", account_id) for account_id in summary["selected"]]
    )
    assert {
        account_id: {
            **value,
            "status": "active",
            "error_message": "",
            "schedulable": account_id in summary["schedulable_enabled"],
        }
        for account_id, value in accounts.items()
    } == accounts
    assert {account_id: value["notes"] for account_id, value in accounts.items()} == original_notes
    assert [item["id"] for item in summary["groups"]["gpt"]] == [3003, 3002, 3001, 3009]
    assert [item["id"] for item in summary["groups"]["claude"]] == [3004, 3006, 3005, 3010]
    assert [item["id"] for item in summary["groups"]["deepseek"]] == [3011, 3008, 3007, 3012]


def test_agentrouter_all_active_accounts_use_ui_recovery_before_disabling():
    accounts = {
        3401: account(3401, name="agentrouter-1-gpt", notes="AgentRouter余额 10.00"),
        3402: account(3402, name="agentrouter-2-claude", notes="AgentRouter余额 20.00"),
        3403: account(3403, name="agentrouter-1-deepseek", notes="AgentRouter余额 30.00"),
    }
    client = FakeClient(accounts)
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["recovered"] == [3401, 3402, 3403]
    assert client.recover_calls == [3401, 3402, 3403]
    assert client.schedulable_calls == [
        (3401, False), (3402, False), (3403, False),
        (3401, True), (3402, True), (3403, True),
    ]
    assert [event for event in client.events if event[0] != "get"] == [
        ("recover", 3401), ("recover", 3402), ("recover", 3403),
        ("disable", 3401), ("disable", 3402), ("disable", 3403),
        ("enable", 3401), ("enable", 3402), ("enable", 3403),
    ]


def test_agentrouter_clears_active_runtime_blocks_even_on_unselected_accounts():
    accounts = {
        account_id: account(
            account_id, name=f"upstream-{account_id}-gpt",
            notes=f"AgentRouter余额 {balance}", schedulable=True,
        )
        for account_id, balance in ((3411, 30), (3412, 20), (3413, 10), (3414, 5))
    }
    for value in accounts.values():
        value.update({
            "rate_limited_at": "2026-09-12T00:00:00Z",
            "rate_limit_reset_at": "2099-09-12T00:00:00Z",
            "overload_until": "2099-09-12T00:00:00Z",
            "temp_unschedulable_until": "2099-09-12T00:00:00Z",
            "temp_unschedulable_reason": "temporary upstream failure",
            "extra": {
                "model_rate_limits": {"model": {"reset_at": "2099-09-12T00:00:00Z"}},
                "antigravity_quota_scopes": {"scope": {"exhausted": True}},
                "unrelated_setting": "preserved",
            },
        })
    original_notes = {key: value["notes"] for key, value in accounts.items()}
    client = FakeClient(accounts)
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["recovered"] == [3411, 3412, 3413, 3414]
    assert summary["selected"] == [3411, 3412, 3413]
    assert summary["failed"] == []
    for account_id, value in accounts.items():
        assert value["status"] == "active"
        assert value["schedulable"] is (account_id in summary["selected"])
        assert value["notes"] == original_notes[account_id]
        assert all(value[field] is None for field in (
            "rate_limited_at", "rate_limit_reset_at", "overload_until",
            "temp_unschedulable_until", "temp_unschedulable_reason",
        ))
        assert value["extra"] == {"unrelated_setting": "preserved"}


def test_agentrouter_active_recovery_failure_is_disabled_and_excluded_from_top_three():
    accounts = {
        account_id: account(
            account_id, name=f"upstream-{account_id}-gpt",
            notes=f"AgentRouter余额 {balance}", schedulable=True,
        )
        for account_id, balance in ((3421, 30), (3422, 20), (3423, 10), (3424, 5), (3425, 1))
    }
    client = FakeClient(accounts, recover_fail=(3421,))
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["recover_failed"] == [3421]
    assert summary["schedulable_disabled"] == [3421, 3422, 3423, 3424, 3425]
    assert summary["selected"] == [3422, 3423, 3424]
    assert summary["schedulable_enabled"] == [3422, 3423, 3424]
    assert client.accounts[3421]["schedulable"] is False
    assert client.accounts[3425]["schedulable"] is False


def test_agentrouter_continues_after_global_recovery_failure_and_still_disables():
    failed = account(3301, "error", "upstream 500", False, name="agentrouter-1-gpt", notes="AgentRouter余额 20.00")
    good = account(3302, "active", "", False, name="agentrouter-2-claude", notes="AgentRouter余额 10.00")
    client = FakeClient({3301: failed, 3302: good}, recover_fail=(3301,))
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["recover_failed"] == [3301]
    assert summary["disable_failed"] == []
    assert summary["schedulable_disabled"] == [3301, 3302]
    assert summary["skipped_recover_failed"] == [3301]
    assert summary["selected"] == [3302]
    assert summary["schedulable_enabled"] == [3302]
    assert summary["failed"] == [3301]
    assert client.schedulable_calls == [(3301, False), (3302, False), (3302, True)]


def test_agentrouter_recovery_exception_after_activation_still_disables_but_never_enables():
    failed = account(3305, "error", "upstream 500", False, name="agentrouter-1-gpt", notes="AgentRouter余额 20.00")
    good = account(3306, "active", "", False, name="agentrouter-2-claude", notes="AgentRouter余额 10.00")
    failed["schedulable"] = True
    good["schedulable"] = True
    client = FakeClient(
        {3305: failed, 3306: good},
        recover_fail_after_active=(3305,),
    )
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["recover_failed"] == [3305]
    assert summary["schedulable_disabled"] == [3305, 3306]
    assert summary["skipped_recover_failed"] == [3305]
    assert summary["disable_failed"] == []
    assert summary["groups"]["gpt"] == []
    assert summary["selected"] == [3306]
    assert summary["schedulable_enabled"] == [3306]
    assert summary["failed"] == [3305]
    assert client.recover_attempts == [3305, 3306]
    assert client.accounts[3305]["status"] == "active"
    assert client.accounts[3305]["schedulable"] is False
    assert client.schedulable_calls == [(3305, False), (3306, False), (3306, True)]
    diagnostics = summary["account_errors"]["3305"]
    assert diagnostics == "recover: RecoveryError: recover account 3305 failed after activation"
    json.dumps(summary["account_errors"])


def test_record_account_failure_appends_cross_stage_diagnostics() -> None:
    summary = {
        "recover_failed": [],
        "disable_failed": [],
        "failed": [],
        "account_errors": {},
    }
    MODULE.record_account_failure(summary, "recover", 3701, MODULE.RecoveryError("first"))
    MODULE.record_account_failure(summary, "disable", 3701, MODULE.RecoveryError("second"))
    assert summary["failed"] == [3701]
    assert summary["account_errors"]["3701"] == (
        "recover: RecoveryError: first; disable: RecoveryError: second"
    )
    json.dumps(summary["account_errors"])


def test_agentrouter_excludes_account_when_disable_verification_fails():
    failed = account(3303, "active", "", False, name="agentrouter-1-gpt", notes="AgentRouter余额 20.00")
    good = account(3304, "active", "", False, name="agentrouter-2-claude", notes="AgentRouter余额 10.00")
    failed["schedulable"] = True
    good["schedulable"] = True
    client = FakeClient({3303: failed, 3304: good}, disable_no_effect=(3303,))
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["disable_failed"] == [3303]
    assert summary["schedulable_disabled"] == [3304]
    assert summary["selected"] == [3304]
    assert summary["schedulable_enabled"] == [3304]
    assert summary["failed"] == [3303]
    assert client.schedulable_calls == [(3303, False), (3304, False), (3304, True)]
    assert client.accounts[3303]["schedulable"] is True


def test_agentrouter_targets_by_url_only_and_skips_deleted_or_url_mismatches():
    # A matching URL is required and sufficient; account names are only used for grouping.
    renamed = account(3100, "error", "upstream 500", False, name="upstream-1-gpt")
    deleted = account(3101, "error", "upstream 500", False, name="agentrouter-1-gpt")
    deleted["deleted_at"] = "2026-08-01T00:00:00Z"
    mismatched = account(3102, "error", "upstream 500", False, name="agentrouter-2-claude", base_url="https://other.example.com/v1")
    enabled = account(3103, "active", "", True, name="agentrouter-3-deepseek")
    untouched = deepcopy({3101: deleted, 3102: mismatched})
    client = FakeClient({3100: renamed, 3101: deleted, 3102: mismatched, 3103: enabled})
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["targets"] == [3100, 3103]
    assert summary["skipped_deleted"] == [3101]
    assert summary["skipped_url_mismatch"] == [3102]
    assert summary["recovered"] == [3100, 3103]
    assert summary["schedulable_disabled"] == [3100, 3103]
    assert summary["selected"] == [3100, 3103]
    assert summary["schedulable_enabled"] == [3100, 3103]
    assert client.recover_calls == [3100, 3103]
    assert client.schedulable_calls == [
        (3100, False), (3103, False), (3100, True), (3103, True),
    ]
    assert {account_id: client.accounts[account_id] for account_id in untouched} == untouched


def test_agentrouter_balance_ties_use_stable_account_id_order():
    accounts = {
        3804: account(3804, name="upstream-4-gpt", notes="AgentRouter余额 9.00"),
        3801: account(3801, name="upstream-1-gpt", notes="AgentRouter余额 10.00"),
        3802: account(3802, name="upstream-2-gpt", notes="AgentRouter余额 10.00"),
        3803: account(3803, name="upstream-3-gpt", notes="AgentRouter余额 9.00"),
    }
    client = FakeClient(accounts)
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert [item["id"] for item in summary["groups"]["gpt"]] == [3801, 3802, 3803, 3804]
    assert summary["selected"] == [3801, 3802, 3803]
    assert summary["schedulable_enabled"] == [3801, 3802, 3803]
    assert client.accounts[3804]["schedulable"] is False


def test_agentrouter_preserves_unknown_suffix_and_group_without_balance():
    unknown = account(3201, "active", "", True, name="agentrouter-1-other", notes="AgentRouter余额 100.00")
    invalid = account(3202, "active", "", True, name="agentrouter-2-gpt", notes="no balance")
    valid = account(3203, "active", "", True, name="agentrouter-3-claude", notes="AgentRouter余额 25.00")
    client = FakeClient({3201: unknown, 3202: invalid, 3203: valid})
    summary = MODULE.run_agentrouter_recovery_cycle(client)
    assert summary["schedulable_disabled"] == [3203]
    assert client.accounts[3201]["schedulable"] is True
    assert client.accounts[3202]["schedulable"] is True
    assert summary["policy_errors"]
    assert summary["skipped_unknown_suffix"] == [3201]
    assert summary["skipped_invalid_balance"] == [3202]
    assert summary["selected"] == [3203]
    assert summary["schedulable_enabled"] == [3203]


def test_agentrouter_balance_and_suffix_parsers():
    assert MODULE.agentrouter_group_suffix("!N-git-1-agentrouter-1223-gpt") == "gpt"
    assert MODULE.agentrouter_group_suffix("!N-gitc-0-agentrouter-6945-claude") == "claude"
    assert MODULE.agentrouter_group_suffix("!N-linux-0-agentrouter-6945-deepseek") == "deepseek"
    assert MODULE.agentrouter_group_suffix("agentrouter-other") is None
    assert MODULE.parse_agentrouter_balance("AgentRouter余额 25.76") == MODULE.Decimal("25.76")
    assert MODULE.parse_agentrouter_balance("AgentRouter余额 -0.11") == MODULE.Decimal("-0.11")
    assert MODULE.parse_agentrouter_balance("no balance") is None


def test_sub2api_client_lists_all_accounts_without_search_or_type_filters():
    client = MODULE.Sub2APIClient("http://127.0.0.1:13080", "key", 1)
    seen_queries = []

    def fake_request(method, path, payload=None):
        seen_queries.append(path)
        assert path.startswith("/api/v1/admin/accounts?")
        page = 1 if "page=1" in path else 2
        items = (
            [
                {"id": 3501, "name": "agentrouter one", "platform": "anthropic", "type": "apikey"},
            ]
            if page == 1
            else [
                {"id": 3502, "name": "not agentrouter in name", "platform": "openai", "type": "oauth"},
            ]
        )
        return 200, json.dumps({"data": {"items": items, "pages": 2}})

    client._request = fake_request
    result = client.list_accounts()
    assert [item["id"] for item in result] == [3501, 3502]
    assert len(seen_queries) == 2
    assert "page=2" in seen_queries[1]
    assert "search=" not in seen_queries[0]
    assert "search=" not in seen_queries[1]
    assert "platform=" not in seen_queries[0]
    assert "type=" not in seen_queries[0]


def test_preview_never_mutates_and_configurable_aliases_share_stable_group():
    policy = MODULE.default_policy()
    policy["groups"] = [{"id": "reasoning", "label": "推理模型", "aliases": ["deepseek", "ds"], "top_n": 1}]
    client = FakeClient({1: account(1, name="old-name-deepseek", notes="余额 20"),
                         2: account(2, name="new-name-ds", notes="余额 30")})
    preview = MODULE.run_agentrouter_recovery_cycle(client, policy=policy, dry_run=True)
    assert preview["selected"] == [2]
    assert preview["preview_groups"][0]["matched"] == 2
    assert not client.recover_attempts and not client.schedulable_calls
    result = MODULE.run_agentrouter_recovery_cycle(client, policy=policy)
    assert result["selected"] == [2]
    assert result["schedulable_enabled"] == [2]
    assert set(result["groups"]) == {"reasoning"}


def test_site_gate_rejects_url_substrings_and_unclassified_accounts_are_untouched():
    for url in ("https://agentrouter.org.evil.example/v1", "https://evil.example/agentrouter.org",
                "https://agentrouter.org@evil.example", "https://evil.example/?site=agentrouter.org"):
        assert not MODULE.matches_site(url)
    assert MODULE.matches_site("https://agentrouter.org/v1")
    client = FakeClient({1: account(1, name="site-renamed", schedulable=True),
                         2: account(2, name="site-deepseek", base_url="https://evil.example/agentrouter.org", schedulable=True)})
    result = MODULE.run_agentrouter_recovery_cycle(client)
    assert result["targets"] == []
    assert result["policy_errors"]
    assert client.accounts[1]["schedulable"] and client.accounts[2]["schedulable"]
    assert not client.recover_attempts and not client.schedulable_calls


def test_policy_rejects_overlapping_aliases_invalid_limits_and_arbitrary_sites():
    import pytest
    for patch in ({"site_host": "other.example"}, {"version": True}):
        value = {**MODULE.default_policy(), **patch}
        with pytest.raises(MODULE.RecoveryError):
            MODULE.validate_policy(value)
    value = MODULE.default_policy()
    value["groups"][1]["aliases"] = ["GPT"]
    with pytest.raises(MODULE.RecoveryError):
        MODULE.validate_policy(value)
    for limit in (0, 21, True, "3"):
        value = MODULE.default_policy()
        value["groups"][0]["top_n"] = limit
        with pytest.raises(MODULE.RecoveryError):
            MODULE.validate_policy(value)


def test_sub2api_client_account_base_url_checks_identity_without_proxy_request():
    client = MODULE.Sub2APIClient("http://127.0.0.1:13080", "key", 1)
    paths = []

    def fake_request(method, path, payload=None):
        paths.append((method, path))
        assert "include_proxies=false" in path
        return 200, json.dumps({
            "data": {
                "accounts": [{
                    "id": 3601,
                    "name": "agentrouter identity",
                    "platform": "anthropic",
                    "type": "apikey",
                    "credentials": {"base_url": "https://agentrouter.org/v1"},
                }]
            }
        })

    client._request = fake_request
    current = account(3601, name="agentrouter identity", platform="anthropic")
    assert client.account_base_url(current) == "https://agentrouter.org/v1"
    assert paths == [("GET", "/api/v1/admin/accounts/data?ids=3601&include_proxies=false")]


def test_agentrouter_schedule_runs_at_every_hour():
    def epoch(day, hour, minute, second=0):
        return int(datetime(2026, 7, day, hour, minute, second, tzinfo=MODULE.LOCAL_TIMEZONE).timestamp())

    for hour in range(24):
        expected = epoch(16, hour + 1, 0) if hour < 23 else epoch(17, 0, 0)
        for minute, second in ((0, 0), (30, 0), (59, 59)):
            assert MODULE.next_agentrouter_recovery_run_at(epoch(16, hour, minute, second)) == expected


def test_agentrouter_schedule_reconciles_persisted_points_to_next_hour():
    def epoch(day, hour, minute, second=0):
        return int(datetime(2026, 7, day, hour, minute, second, tzinfo=MODULE.LOCAL_TIMEZONE).timestamp())

    state = {"next_run_at": epoch(16, 8, 0), "last_cycle_at": epoch(16, 0, 0)}
    assert MODULE.reconcile_agentrouter_recovery_schedule(state, epoch(16, 0, 18)) is True
    assert state["next_run_at"] == epoch(16, 1, 0)
    assert state["last_interval_seconds"] == 42 * 60
    assert state["last_cycle_at"] == epoch(16, 0, 0)
    state = {"next_run_at": epoch(16, 16, 0)}
    assert MODULE.reconcile_agentrouter_recovery_schedule(state, epoch(16, 12, 45)) is True
    assert state["next_run_at"] == epoch(16, 13, 0)
    state = {"next_run_at": epoch(16, 8, 0)}
    assert MODULE.reconcile_agentrouter_recovery_schedule(state, epoch(16, 7, 44)) is False
    state = {"next_run_at": epoch(16, 12, 30)}
    assert MODULE.reconcile_agentrouter_recovery_schedule(state, epoch(16, 12, 45)) is True
    assert state["next_run_at"] == epoch(16, 13, 0)
    state = {"next_run_at": epoch(16, 13, 30)}
    assert MODULE.reconcile_agentrouter_recovery_schedule(state, epoch(16, 12, 45)) is True
    assert state["next_run_at"] == epoch(16, 13, 0)
