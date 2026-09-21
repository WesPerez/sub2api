"""Regression tests for boot conflicts and interrupted network recovery."""

import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("restore_network", Path(__file__).resolve().parents[1] / "restore-network.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def endpoint(ip, priority=0):
    return {"IPAddress": ip, "IPPrefixLen": 16, "IPAMConfig": {},
            "GwPriority": priority, "Aliases": [], "Links": None, "DriverOpts": {}}


class FakeDocker:
    def __init__(self):
        self.containers = {
            module.APP: {
                "Id": "app-id", "Name": "/" + module.APP,
                "State": {"Status": "running", "Health": {"Status": "healthy"}},
                "NetworkSettings": {"Networks": {
                    module.APP_NETWORK: endpoint("172.18.0.5"),
                    module.BRIDGE: endpoint("172.17.0.3", -1),
                }},
            },
            "antigravity-manager": {
                "Id": "peer-id", "Name": "/antigravity-manager",
                "State": {"Status": "running"},
                "NetworkSettings": {"Networks": {
                    module.APP_NETWORK: endpoint("172.18.0.4"),
                    module.BRIDGE: endpoint(module.ADDRESS),
                }},
            },
        }
        self.mutations = []
        self.fail_connect = None
        self.disconnect_error = None

    def get(self, name):
        for key, container in self.containers.items():
            if name in (key, container["Id"]):
                return container
        raise subprocess.CalledProcessError(1, ("docker", "inspect", name), stderr="No such object")

    def nets(self, name):
        return self.get(name)["NetworkSettings"]["Networks"]

    def __call__(self, *args):
        if args[:2] == ("container", "ls"):
            wanted = args[-1].removeprefix("id=")
            return "\n".join(c["Id"] for c in self.containers.values() if c["Id"] == wanted)
        if args[0] == "inspect":
            return json.dumps([self.get(args[1])])
        if args[:3] == ("network", "inspect", module.BRIDGE):
            return json.dumps([{"Containers": {
                c["Id"]: {"Name": n, "IPv4Address": self.nets(n)[module.BRIDGE]["IPAddress"] + "/16"}
                for n, c in self.containers.items() if module.BRIDGE in self.nets(n)
            }}])
        if args[:2] == ("network", "disconnect"):
            self.mutations.append(args)
            self.nets(args[-1]).pop(module.BRIDGE)
            if self.disconnect_error:
                error, self.disconnect_error = self.disconnect_error, None
                raise error
            return ""
        if args[:2] == ("network", "connect"):
            self.mutations.append(args)
            if self.fail_connect and self.fail_connect(args[-1]):
                raise subprocess.CalledProcessError(1, args)
            occupied = {net["IPAddress"] for c in self.containers.values()
                        for name, net in c["NetworkSettings"]["Networks"].items()
                        if name == module.BRIDGE}
            ip = next(f"172.17.0.{i}" for i in range(2, 254) if f"172.17.0.{i}" not in occupied)
            self.nets(args[-1])[module.BRIDGE] = endpoint(ip, int(args[3]))
            return ""
        raise AssertionError(f"Unexpected Docker operation: {args}")


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.docker = FakeDocker()
        self.journal = Path(self.directory.name) / "pending.json"
        self.recovery = module.Recovery(self.docker, self.journal)
        self.sleep = patch.object(module.time, "sleep")
        self.sleep.start()
        self.addCleanup(self.sleep.stop)

    def assert_connected(self):
        for name in (module.APP, "antigravity-manager"):
            self.assertIn(module.BRIDGE, self.docker.nets(name))
            self.assertIn(module.APP_NETWORK, self.docker.nets(name))
            self.assertEqual(self.docker.get(name)["State"]["Status"], "running")

    def test_boot_address_conflict_recovers_without_stopping_containers(self):
        original = copy.deepcopy(self.docker.nets("antigravity-manager")[module.APP_NETWORK])
        self.assertTrue(self.recovery.restore_bridge())
        self.assertEqual(self.docker.nets(module.APP)[module.BRIDGE]["IPAddress"], module.ADDRESS)
        self.assertEqual(self.docker.nets("antigravity-manager")[module.BRIDGE]["IPAddress"], "172.17.0.3")
        self.assertEqual(self.docker.nets("antigravity-manager")[module.APP_NETWORK], original)
        self.assert_connected()
        self.assertFalse(self.journal.exists())
        self.docker.mutations.clear()
        self.assertTrue(self.recovery.restore_bridge())
        self.assertEqual(self.docker.mutations, [])

    def test_unknown_owner_is_never_displaced(self):
        peer = self.docker.containers.pop("antigravity-manager")
        self.docker.containers["unrelated-service"] = peer
        with self.assertRaises(module.UnsafeConflict):
            self.recovery.restore_bridge()
        self.assertEqual(self.docker.mutations, [])

    def test_peer_requires_verified_alternate_network_and_dynamic_address(self):
        for change in ("missing-alternate", "static-address", "custom-alias", "custom-driver"):
            with self.subTest(change=change):
                self.docker = FakeDocker()
                self.recovery.command = self.docker
                nets = self.docker.nets("antigravity-manager")
                if change == "missing-alternate":
                    nets.pop(module.APP_NETWORK)
                elif change == "static-address":
                    nets[module.BRIDGE]["IPAMConfig"] = {"IPv4Address": module.ADDRESS}
                elif change == "custom-alias":
                    nets[module.BRIDGE]["Aliases"] = ["custom"]
                else:
                    nets[module.BRIDGE]["DriverOpts"] = {"custom": "value"}
                with self.assertRaises(module.UnsafeConflict):
                    self.recovery.restore_bridge()
                self.assertEqual(self.docker.mutations, [])

    def test_missing_application_network_is_not_modified(self):
        self.docker.nets(module.APP).pop(module.APP_NETWORK)
        with self.assertRaises(module.UnsafeConflict):
            self.recovery.restore_bridge()
        self.assertEqual(self.docker.mutations, [])

    def test_missing_bridge_with_no_owner_is_attached(self):
        self.docker.nets(module.APP).pop(module.BRIDGE)
        self.docker.nets("antigravity-manager").pop(module.BRIDGE)
        self.assertTrue(self.recovery.restore_bridge())
        self.assertEqual(self.docker.nets(module.APP)[module.BRIDGE]["IPAddress"], module.ADDRESS)

    def test_connection_failure_restores_original_attachments(self):
        failures = iter([True, False, False])
        self.docker.fail_connect = lambda _name: next(failures, False)
        with self.assertRaises(subprocess.CalledProcessError):
            self.recovery.restore_bridge()
        self.assert_connected()
        self.assertEqual(self.docker.nets("antigravity-manager")[module.BRIDGE]["IPAddress"], module.ADDRESS)
        self.assertFalse(self.journal.exists())

    def test_disconnect_timeout_after_mutation_is_cleaned_up(self):
        self.docker.disconnect_error = subprocess.TimeoutExpired("docker", 15)
        with self.assertRaises(subprocess.TimeoutExpired):
            self.recovery.restore_bridge()
        self.assert_connected()
        self.assertFalse(self.journal.exists())

    def test_termination_runs_cleanup(self):
        self.docker.disconnect_error = SystemExit(143)
        with self.assertRaises(SystemExit):
            self.recovery.restore_bridge()
        self.assert_connected()
        self.assertFalse(self.journal.exists())

    def test_failed_cleanup_is_retried_on_next_process_even_with_healthy_app(self):
        self.docker.fail_connect = lambda name: name == "peer-id"
        with self.assertRaisesRegex(RuntimeError, "cleanup pending"):
            self.recovery.restore_bridge()
        self.assertTrue(self.journal.exists())
        self.assertNotIn(module.BRIDGE, self.docker.nets("antigravity-manager"))
        self.docker.fail_connect = None
        new_process = module.Recovery(self.docker, self.journal)
        self.assertTrue(new_process.restore_bridge())
        self.assert_connected()
        self.assertFalse(self.journal.exists())

    def test_interrupted_transaction_is_recovered_from_journal(self):
        self.recovery.save_pending([("peer-id", 0), ("app-id", -1)])
        for name in (module.APP, "antigravity-manager"):
            self.docker.nets(name).pop(module.BRIDGE)
        self.assertTrue(module.Recovery(self.docker, self.journal).restore_bridge())
        self.assert_connected()
        self.assertFalse(self.journal.exists())

    def test_removed_container_record_does_not_block_replacement_recovery(self):
        self.recovery.save_pending([("removed-peer-id", 0), ("removed-app-id", -1)])
        self.assertTrue(self.recovery.restore_bridge())
        self.assert_connected()
        self.assertFalse(self.journal.exists())

    def test_daemon_failure_never_discards_pending_cleanup(self):
        self.recovery.save_pending([("peer-id", 0)])
        def unavailable(*args):
            raise subprocess.CalledProcessError(1, args, stderr="Cannot connect to Docker daemon")
        self.recovery.command = unavailable
        with self.assertRaisesRegex(RuntimeError, "cleanup pending"):
            self.recovery.restore_pending()
        self.assertTrue(self.journal.exists())

    def test_inspect_failure_for_existing_container_keeps_pending_cleanup(self):
        self.recovery.save_pending([("peer-id", 0)])
        def failed_inspect(*args):
            if args[0] == "inspect":
                raise subprocess.CalledProcessError(1, args)
            return self.docker(*args)
        self.recovery.command = failed_inspect
        with self.assertRaisesRegex(RuntimeError, "cleanup pending"):
            self.recovery.restore_pending()
        self.assertTrue(self.journal.exists())

    def test_legacy_grok_with_alternate_network_is_supported(self):
        peer = self.docker.containers.pop("antigravity-manager")
        peer["Name"] = "/grok2api"
        peer["NetworkSettings"]["Networks"]["grok2api_internal"] = peer["NetworkSettings"]["Networks"].pop(module.APP_NETWORK)
        self.docker.containers["grok2api"] = peer
        self.assertTrue(self.recovery.restore_bridge())
        self.assertEqual(self.docker.nets(module.APP)[module.BRIDGE]["IPAddress"], module.ADDRESS)
        self.assertIn("grok2api_internal", self.docker.nets("grok2api"))


if __name__ == "__main__":
    unittest.main()
