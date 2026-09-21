import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import urllib.error


MODULE_PATH = Path(__file__).resolve().parents[1] / "sub2api_proxy_reconcile.py"
SPEC = importlib.util.spec_from_file_location("sub2api_proxy_reconcile", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(MODULE)


def token_hash(value):
    return hashlib.sha256(value.encode()).hexdigest()


class ReconcileUnitTests(unittest.TestCase):
    def setUp(self):
        self.tokens = {
            "openai": "openai-secret",
            "apps": "apps-secret",
            "direct": "direct-secret",
        }
        self.config = MODULE.validate_config({
            "schema_version": 2,
            "state_dir": "/tmp/state",
            "legacy_proxy_name_regexes": [
                "^legacy-proxy-[0-9]+$",
            ],
            "sub2api": {
                "base_url": "http://127.0.0.1:13080/api/v1",
                "postgres_container": "postgres",
                "db_user": "sub2api",
                "db_name": "sub2api",
            },
            "profiles": [
                {
                    "key": "global",
                    "name": "Global",
                    "resin_platform": "AppsGlobal",
                    "protocol": "socks5h",
                    "host": "proxy.internal",
                    "port": 10834,
                    "username_template": "AppsGlobal.sub2-{{account_id}}",
                    "token_ref": "apps",
                    "inherit_legacy_lease": False,
                    "allowed_account_platforms": ["grok", "openai"],
                    "default_for": ["grok", "openai"],
                },
                {
                    "key": "cn",
                    "name": "CN",
                    "resin_platform": "AppsCN",
                    "protocol": "socks5h",
                    "host": "proxy.internal",
                    "port": 10834,
                    "username_template": "AppsCN.sub2-{{account_id}}",
                    "token_ref": "apps",
                    "inherit_legacy_lease": False,
                    "allowed_account_platforms": ["grok", "openai"],
                    "default_for": [],
                },
                {
                    "key": "direct",
                    "name": "直连",
                    "data_plane": "direct",
                    "protocol": "socks5h",
                    "host": "proxy.internal",
                    "port": 12400,
                    "username_template": "sub2-direct",
                    "token_ref": "direct",
                    "inherit_legacy_lease": False,
                    "allowed_account_platforms": ["grok", "openai"],
                    "default_for": [],
                },
            ],
        })

    def profile_row(self, key, proxy_id):
        profile = next(row for row in self.config["profiles"] if row["key"] == key)
        return {
            "id": proxy_id,
            "name": profile["name"],
            "protocol": profile["protocol"],
            "host": profile["host"],
            "port": profile["port"],
            "username": profile["username_template"],
            "password_sha256": token_hash(self.tokens[profile["token_ref"]]),
            "status": "active",
            "deleted": False,
        }

    def all_profile_rows(self):
        return [
            self.profile_row("global", 100),
            self.profile_row("cn", 101),
            self.profile_row("direct", 102),
        ]

    def test_defaults_and_template_are_validated(self):
        self.assertEqual(self.config["_defaults"], {"grok": "global", "openai": "global"})
        self.assertNotIn("cn", self.config["_defaults"].values())
        self.assertNotIn("direct", self.config["_defaults"].values())
        bad = dict(self.config)
        bad["schema_version"] = 1
        with self.assertRaises(MODULE.ReconcileError):
            MODULE.validate_config(bad)

    def test_index_accepts_three_profiles_and_tracks_legacy(self):
        inventory = {
            "accounts": [],
            "proxies": self.all_profile_rows() + [
                {"id": 9, "name": "legacy-proxy-1", "username": "OpenAI.sub2-1", "deleted": False},
            ],
        }
        _, profiles, legacy = MODULE.index_inventory(self.config, inventory, self.tokens)
        self.assertEqual({key: row["id"] for key, row in profiles.items()}, {
            "global": 100,
            "cn": 101,
            "direct": 102,
        })
        self.assertEqual(legacy, {9})

    def test_duplicate_profile_name_fails_closed(self):
        duplicate = self.profile_row("global", 103)
        inventory = {"accounts": [], "proxies": self.all_profile_rows() + [duplicate]}
        with self.assertRaises(MODULE.ReconcileError):
            MODULE.index_inventory(self.config, inventory, self.tokens)

    def test_mismatched_profile_requires_versioned_rotation(self):
        row = self.profile_row("global", 100)
        row["host"] = "127.0.0.1"
        with self.assertRaises(MODULE.ReconcileError):
            MODULE.index_inventory(self.config, {"accounts": [], "proxies": [row]}, self.tokens)

    def test_only_three_managed_bindings_are_preserved(self):
        accounts = [
            {"id": 1, "platform": "grok", "proxy_id": 9, "parent_account_id": None, "base_url": ""},
            {"id": 2, "platform": "openai", "proxy_id": 101, "parent_account_id": None, "base_url": ""},
            {"id": 3, "platform": "openai", "proxy_id": 100, "parent_account_id": None, "base_url": ""},
        ]
        _, profiles, _ = MODULE.index_inventory(
            self.config,
            {"accounts": accounts, "proxies": self.all_profile_rows()},
            self.tokens,
        )
        analysis = MODULE.analyze_bindings(self.config, accounts, profiles)
        self.assertEqual(analysis["actions"], [
            {"account_id": 1, "from_proxy_id": 9, "to_profile": "global", "to_proxy_id": 100},
        ])
        self.assertEqual(analysis["preserved"], 2)

    def test_only_empty_bindings_receive_platform_defaults(self):
        accounts = [
            {"id": 1, "platform": "grok", "proxy_id": None, "parent_account_id": None, "base_url": ""},
            {"id": 2, "platform": "openai", "proxy_id": None, "parent_account_id": None, "base_url": ""},
        ]
        profiles = {
            "global": self.profile_row("global", 100),
        }

        analysis = MODULE.analyze_bindings(self.config, accounts, profiles)

        self.assertEqual(analysis["actions"], [
            {"account_id": 1, "from_proxy_id": None, "to_profile": "global", "to_proxy_id": 100},
            {"account_id": 2, "from_proxy_id": None, "to_profile": "global", "to_proxy_id": 100},
        ])

    def test_manual_cn_profile_is_preserved_for_both_platforms(self):
        accounts = [
            {"id": 1, "platform": "grok", "proxy_id": 101, "parent_account_id": None, "base_url": ""},
            {"id": 2, "platform": "openai", "proxy_id": 101, "parent_account_id": None, "base_url": ""},
        ]
        profiles = {"global": self.profile_row("global", 100),
                    "cn": self.profile_row("cn", 101)}
        analysis = MODULE.analyze_bindings(self.config, accounts, profiles)
        self.assertEqual(analysis["actions"], [])
        self.assertEqual(analysis["preserved"], 2)

    def test_manual_direct_and_shadow_proxy_values_are_both_preserved(self):
        accounts = [
            {"id": 1, "platform": "grok", "proxy_id": 102, "parent_account_id": None, "base_url": ""},
            {"id": 2, "platform": "openai", "proxy_id": 102, "parent_account_id": None, "base_url": ""},
            {"id": 3, "platform": "openai", "proxy_id": 100, "parent_account_id": 2, "base_url": ""},
        ]
        profiles = {
            "global": self.profile_row("global", 100),
            "direct": self.profile_row("direct", 102),
        }

        analysis = MODULE.analyze_bindings(self.config, accounts, profiles)

        self.assertEqual(analysis["actions"], [])
        self.assertEqual(analysis["preserved"], 3)

    def test_direct_profile_rejects_account_username_template(self):
        bad = {
            key: value
            for key, value in self.config.items()
            if not str(key).startswith("_")
        }
        bad["profiles"] = [dict(profile) for profile in self.config["profiles"]]
        direct = next(profile for profile in bad["profiles"] if profile["key"] == "direct")
        direct["username_template"] = "Direct.sub2-{{account_id}}"
        with self.assertRaisesRegex(MODULE.ReconcileError, "static proxy username"):
            MODULE.validate_config(bad)

    def test_urls_do_not_affect_policy_and_unmanaged_bindings_converge(self):
        accounts = [
            {"id": 1, "platform": "openai", "proxy_id": 999, "parent_account_id": None, "base_url": ""},
            {"id": 2, "platform": "openai", "proxy_id": 101, "parent_account_id": 1, "base_url": ""},
            {
                "id": 3,
                "platform": "openai",
                "proxy_id": None,
                "parent_account_id": None,
                "base_url": "https://cn-provider.example/v1",
            },
            {
                "id": 4,
                "platform": "openai",
                "proxy_id": None,
                "parent_account_id": None,
                "base_url": "https://global-provider.example/v1",
            },
        ]
        profiles = {"global": self.profile_row("global", 100),
                    "cn": self.profile_row("cn", 101)}
        analysis = MODULE.analyze_bindings(self.config, accounts, profiles)
        self.assertEqual(analysis["actions"], [
            {"account_id": 1, "from_proxy_id": 999, "to_profile": "global", "to_proxy_id": 100},
            {"account_id": 3, "from_proxy_id": None, "to_profile": "global", "to_proxy_id": 100},
            {"account_id": 4, "from_proxy_id": None, "to_profile": "global", "to_proxy_id": 100},
        ])
        self.assertEqual(analysis["preserved"], 1)

    def test_fingerprint_changes_with_tokens_without_exposing_them(self):
        first = MODULE.desired_fingerprint(self.config, self.tokens)
        changed = dict(self.tokens)
        changed["apps"] = "different"
        second = MODULE.desired_fingerprint(self.config, changed)
        self.assertNotEqual(first, second)
        self.assertNotIn("secret", first)

    def test_parse_token_files_requires_exact_key_path_pairs(self):
        parsed = MODULE.parse_token_files(["openai=/tmp/openai", "apps=/tmp/apps"])
        self.assertEqual(parsed["apps"], Path("/tmp/apps"))
        with self.assertRaises(MODULE.ReconcileError):
            MODULE.parse_token_files(["broken"])

    def test_profile_idempotency_key_changes_with_versioned_name(self):
        calls = []

        class Client:
            def request(self, method, path, body=None, extra_headers=None):
                calls.append((method, path, body, extra_headers))
                return {"id": 123}

        profile = dict(self.config["profiles"][0])
        MODULE.create_profile(Client(), profile, self.tokens[profile["token_ref"]])
        profile["name"] += "-v2"
        MODULE.create_profile(Client(), profile, self.tokens[profile["token_ref"]])

        first_key = calls[0][3]["Idempotency-Key"]
        second_key = calls[1][3]["Idempotency-Key"]
        self.assertNotEqual(first_key, second_key)
        self.assertTrue(first_key.startswith(f"sub2-resin-profile-v2-{profile['key']}-"))

    def test_delete_legacy_proxies_uses_exact_batch_and_returns_skips(self):
        class Client:
            def request(self, method, path, body):
                self.call = (method, path, body)
                return {
                    "deleted_ids": [9],
                    "skipped": [{"id": 10, "reason": "proxy is in use"}],
                }

        client = Client()
        deleted, skipped = MODULE.delete_legacy_proxies(client, [9, 10])
        self.assertEqual(client.call, (
            "POST",
            "/admin/proxies/batch-delete",
            {"ids": [9, 10]},
        ))
        self.assertEqual(deleted, [9])
        self.assertEqual(skipped, [10])

    def test_delete_legacy_proxies_accepts_null_empty_slices(self):
        client = mock.Mock()
        client.request.return_value = {"deleted_ids": [9], "skipped": None}

        deleted, skipped = MODULE.delete_legacy_proxies(client, [9])

        self.assertEqual(deleted, [9])
        self.assertEqual(skipped, [])

    def test_resin_inherit_lease_accepts_success_and_redacts_token(self):
        class Response:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit):
                return b'{"status":"ok"}'

        class Opener:
            request = None

            def open(self, request, timeout):
                self.request = request
                self.timeout = timeout
                return Response()

        client = MODULE.ResinClient()
        client.opener = Opener()
        profile = next(row for row in self.config["profiles"] if row["key"] == "global")
        self.assertTrue(client.inherit_lease(profile, "token/with slash", "shard-1", "sub2-1"))
        self.assertIn("/token%2Fwith%20slash/", client.opener.request.full_url)
        self.assertNotIn("token/with slash", client.opener.request.full_url)

    def test_resin_inherit_lease_treats_only_parent_missing_404_as_expected(self):
        class Opener:
            def open(self, request, timeout):
                raise urllib.error.HTTPError(
                    request.full_url,
                    404,
                    "Not Found",
                    {},
                    io.BytesIO(
                        b'{"error":{"code":"NOT_FOUND","message":"parent lease not found"}}'
                    ),
                )

        client = MODULE.ResinClient()
        client.opener = Opener()
        profile = next(row for row in self.config["profiles"] if row["key"] == "global")
        self.assertFalse(client.inherit_lease(profile, "secret", "missing", "sub2-1"))

    def test_resin_inherit_lease_rejects_ambiguous_404_responses(self):
        profile = next(row for row in self.config["profiles"] if row["key"] == "global")
        responses = [
            b'{"error":{"code":"NOT_FOUND","message":"platform not found"}}',
            b'{"code":"NOT_FOUND"}',
            b'404 page not found',
        ]
        for response_body in responses:
            with self.subTest(response_body=response_body):
                class Opener:
                    def open(self, request, timeout):
                        raise urllib.error.HTTPError(
                            request.full_url,
                            404,
                            "Not Found",
                            {},
                            io.BytesIO(response_body),
                        )

                client = MODULE.ResinClient()
                client.opener = Opener()
                with self.assertRaisesRegex(MODULE.ReconcileError, "returned HTTP 404"):
                    client.inherit_lease(profile, "secret", "missing", "sub2-1")

    def test_apply_binds_an_empty_account_to_its_platform_default(self):
        current_proxy_ids = {1: None}
        events = []

        def inventory(_config):
            return {
                "accounts": [
                    {
                        "id": 1,
                        "platform": "grok",
                        "proxy_id": current_proxy_ids[1],
                        "parent_account_id": None,
                        "base_url": "",
                    }
                ],
                "proxies": self.all_profile_rows(),
            }

        class FakeResinClient(MODULE.ResinClient):
            def inherit_lease(_self, *_args):
                raise AssertionError("empty-account binding must not inherit a lease")

        def update(_client, account_id, proxy_id):
            events.append(("bind", account_id, proxy_id))
            current_proxy_ids[account_id] = proxy_id

        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with (
                mock.patch.object(MODULE, "run_inventory", side_effect=inventory),
                mock.patch.object(MODULE, "Sub2Client", return_value=object()),
                mock.patch.object(MODULE, "ResinClient", FakeResinClient),
                mock.patch.object(MODULE, "update_account_binding", side_effect=update),
            ):
                result = MODULE.reconcile(config, "admin", self.tokens, True)

        self.assertEqual(events, [("bind", 1, 100)])
        self.assertEqual(result["inherited_leases"], 0)
        self.assertEqual(result["already_stable_leases"], 0)
        self.assertEqual(current_proxy_ids, {1: 100})

    def test_apply_migrates_an_unmanaged_binding_with_recovery_manifest(self):
        current_proxy_ids = {3: 10}

        def inventory(_config):
            return {
                "accounts": [
                    {
                        "id": 3,
                        "platform": "openai",
                        "proxy_id": current_proxy_ids[3],
                        "parent_account_id": None,
                        "base_url": "",
                    }
                ],
                "proxies": self.all_profile_rows()
                + [
                    {
                        "id": 10,
                        "name": "legacy-proxy-3",
                        "username": "OpenAI.sub2-3",
                        "deleted": False,
                    }
                ],
            }

        events = []

        class FakeResinClient(MODULE.ResinClient):
            def inherit_lease(_self, *_args):
                raise AssertionError("target profile does not inherit legacy leases")

        def update(_client, account_id, proxy_id):
            events.append((account_id, proxy_id))
            current_proxy_ids[account_id] = proxy_id

        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with (
                mock.patch.object(MODULE, "run_inventory", side_effect=inventory),
                mock.patch.object(MODULE, "Sub2Client", return_value=object()),
                mock.patch.object(MODULE, "ResinClient", FakeResinClient),
                mock.patch.object(MODULE, "update_account_binding", side_effect=update),
            ):
                result = MODULE.reconcile(config, "admin", self.tokens, True)

        self.assertFalse(result.get("no_changes", False))
        self.assertIsNotNone(result["recovery_manifest"])
        self.assertEqual(events, [(3, 100)])
        self.assertEqual(current_proxy_ids, {3: 100})

    def test_binding_failure_rolls_back_in_reverse_order(self):
        current_proxy_ids = {1: None, 2: None}
        events = []

        def inventory(_config):
            return {
                "accounts": [
                    {
                        "id": account_id,
                        "platform": "grok",
                        "proxy_id": proxy_id,
                        "parent_account_id": None,
                        "base_url": "",
                    }
                    for account_id, proxy_id in sorted(current_proxy_ids.items())
                ],
                "proxies": self.all_profile_rows(),
            }

        class FakeResinClient(MODULE.ResinClient):
            def inherit_lease(_self, profile, token, parent_account, new_account):
                raise AssertionError("empty-account binding must not inherit a lease")

        def update(_client, account_id, proxy_id):
            events.append(("bind", account_id, proxy_id))
            if account_id == 2 and proxy_id == 100:
                raise MODULE.ReconcileError("synthetic binding failure")
            current_proxy_ids[account_id] = proxy_id

        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with (
                mock.patch.object(MODULE, "run_inventory", side_effect=inventory),
                mock.patch.object(MODULE, "Sub2Client", return_value=object()),
                mock.patch.object(MODULE, "ResinClient", FakeResinClient),
                mock.patch.object(MODULE, "update_account_binding", side_effect=update),
            ):
                with self.assertRaisesRegex(MODULE.ReconcileError, "synthetic binding failure"):
                    MODULE.reconcile(config, "admin", self.tokens, True)
            manifest_path = next((Path(temporary) / "recovery").glob("*.json"))
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

        self.assertEqual(events[-1], ("bind", 1, None))
        self.assertEqual(current_proxy_ids, {1: None, 2: None})
        self.assertEqual(manifest["status"], "rolled_back")

    def test_final_readback_failure_is_recorded_and_rolled_back(self):
        events = []

        def inventory(_config):
            return {
                "accounts": [
                    {
                        "id": 1,
                        "platform": "grok",
                        "proxy_id": None,
                        "parent_account_id": None,
                        "base_url": "",
                    }
                ],
                "proxies": self.all_profile_rows(),
            }

        class FakeResinClient(MODULE.ResinClient):
            def inherit_lease(_self, profile, token, parent_account, new_account):
                return True

        def update(_client, account_id, proxy_id):
            events.append((account_id, proxy_id))

        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with (
                mock.patch.object(MODULE, "run_inventory", side_effect=inventory),
                mock.patch.object(MODULE, "Sub2Client", return_value=object()),
                mock.patch.object(MODULE, "ResinClient", FakeResinClient),
                mock.patch.object(MODULE, "update_account_binding", side_effect=update),
            ):
                with self.assertRaisesRegex(MODULE.ReconcileError, "unresolved bindings"):
                    MODULE.reconcile(config, "admin", self.tokens, True)
            manifest_path = next((Path(temporary) / "recovery").glob("*.json"))
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

        self.assertEqual(events, [(1, 100), (1, None)])
        self.assertEqual(manifest["status"], "rolled_back")

    def test_cleanup_requires_a_converged_profile_migration(self):
        inventory = {
            "accounts": [
                {
                    "id": 1,
                    "platform": "grok",
                    "proxy_id": None,
                    "parent_account_id": None,
                    "base_url": "",
                }
            ],
            "proxies": self.all_profile_rows(),
        }
        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with mock.patch.object(MODULE, "run_inventory", return_value=inventory):
                with self.assertRaisesRegex(MODULE.ReconcileError, "already converged"):
                    MODULE.reconcile(
                        config,
                        "admin",
                        self.tokens,
                        True,
                        cleanup_unreferenced_legacy=True,
                    )
        self.assertFalse((Path(temporary) / "recovery").exists())

    def test_converged_apply_is_a_noop_without_recovery_manifest(self):
        inventory = {
            "accounts": [
                {
                    "id": 1,
                    "platform": "grok",
                    "proxy_id": 100,
                    "parent_account_id": None,
                    "base_url": "",
                }
            ],
            "proxies": self.all_profile_rows(),
        }
        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with (
                mock.patch.object(MODULE, "run_inventory", return_value=inventory),
                mock.patch.object(MODULE, "Sub2Client") as client,
            ):
                result = MODULE.reconcile(config, "admin", self.tokens, True)
            state = json.loads(
                (Path(temporary) / "state.json").read_text(encoding="utf-8")
            )

        self.assertTrue(result["no_changes"])
        self.assertIsNone(result["recovery_manifest"])
        self.assertEqual(state["profiles"], {
            "cn": 101,
            "direct": 102,
            "global": 100,
        })
        client.assert_not_called()
        self.assertFalse((Path(temporary) / "recovery").exists())

    def test_cleanup_soft_deletes_only_unreferenced_legacy_proxies(self):
        deleted = False

        def inventory(_config):
            proxies = self.all_profile_rows()
            if not deleted:
                proxies = proxies + [
                    {
                        "id": 9,
                        "name": "legacy-proxy-7",
                        "username": "OpenAI.sub2-7",
                        "deleted": False,
                    }
                ]
            return {
                "accounts": [
                    {
                        "id": 1,
                        "platform": "grok",
                        "proxy_id": 100,
                        "parent_account_id": None,
                        "base_url": "",
                    }
                ],
                "proxies": proxies,
            }

        class Client:
            def __init__(self, *_args):
                pass

            def request(self, method, path, body=None, extra_headers=None):
                nonlocal deleted
                self.assertions = (method, path, body, extra_headers)
                if path != "/admin/proxies/batch-delete":
                    raise AssertionError(f"unexpected path: {path}")
                self.__class__.last_call = (method, path, body, extra_headers)
                deleted = True
                return {"deleted_ids": [9], "skipped": []}

        with tempfile.TemporaryDirectory() as temporary:
            config = dict(self.config)
            config["state_dir"] = temporary
            with (
                mock.patch.object(MODULE, "run_inventory", side_effect=inventory),
                mock.patch.object(MODULE, "Sub2Client", Client),
            ):
                result = MODULE.reconcile(
                    config,
                    "admin",
                    self.tokens,
                    True,
                    cleanup_unreferenced_legacy=True,
                )
            manifest_path = next((Path(temporary) / "recovery").glob("*.json"))
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

        self.assertEqual(Client.last_call[0:3], (
            "POST",
            "/admin/proxies/batch-delete",
            {"ids": [9]},
        ))
        self.assertEqual(result["deleted_legacy_proxies"], 1)
        self.assertEqual(result["unreferenced_legacy_proxy_ids"], [])
        self.assertEqual(manifest["deleted_legacy_proxy_ids"], [9])
        self.assertEqual(manifest["status"], "completed")


if __name__ == "__main__":
    unittest.main()
