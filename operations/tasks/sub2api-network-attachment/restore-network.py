#!/usr/bin/env python3
"""Restore the production bridge contract without recreating containers."""

import json
import os
from pathlib import Path
import signal
import subprocess
import time
import urllib.request


APP = "sub2api-prod"
APP_NETWORK = "sub2api-prod_default"
BRIDGE = "bridge"
ADDRESS = "172.17.0.2"
PEER_NETWORKS = {
    "antigravity-manager": {APP_NETWORK},
    "grok2api": {APP_NETWORK, "grok2api_internal"},
}


class UnsafeConflict(RuntimeError):
    pass


def log(message):
    print(f"[sub2api-network-restore] {message}", flush=True)


def docker(*args):
    result = subprocess.run(
        ["docker", *args], text=True, capture_output=True, timeout=15, check=True
    )
    return result.stdout


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


class Recovery:
    def __init__(self, command=docker, journal="/var/lib/sub2api-network-restore/pending.json"):
        self.command = command
        self.journal = Path(journal)

    def save_pending(self, attachments):
        self.journal.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        temporary = self.journal.with_suffix(".tmp")
        with temporary.open("w") as output:
            json.dump(attachments, output)
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(self.journal)
        # Persist both the transaction and a newly created journal directory
        # before changing Docker's persisted network attachments.
        sync_directory(self.journal.parent)
        sync_directory(self.journal.parent.parent)

    def restore_pending(self):
        if not self.journal.exists():
            return
        failures = []
        for container_id, priority in json.loads(self.journal.read_text()):
            for attempt in range(3):
                try:
                    try:
                        networks = self.networks(container_id)
                    except subprocess.CalledProcessError:
                        # A rollout may have replaced this exact container since
                        # the interrupted transaction. Require a successful Docker
                        # inventory before treating the old ID as removed.
                        existing = self.command("container", "ls", "--all", "--quiet", "--no-trunc",
                                                "--filter", f"id={container_id}").splitlines()
                        if container_id not in existing:
                            log(f"discarding completed cleanup for removed container {container_id[:12]}")
                            break
                        raise
                    if BRIDGE not in networks:
                        self.command("network", "connect", "--gw-priority", str(priority),
                                     BRIDGE, container_id)
                    if BRIDGE not in self.networks(container_id):
                        raise RuntimeError("bridge attachment still missing")
                    break
                except (subprocess.SubprocessError, OSError, ValueError, RuntimeError) as error:
                    if attempt == 2:
                        failures.append(f"{container_id[:12]}: {error}")
                    else:
                        time.sleep(0.2)
        if failures:
            raise RuntimeError("bridge cleanup pending: " + "; ".join(failures))
        self.journal.unlink()
        sync_directory(self.journal.parent)

    def inspect(self, container):
        return json.loads(self.command("inspect", container))[0]

    def networks(self, container):
        return self.inspect(container)["NetworkSettings"]["Networks"]

    def start(self, container):
        state = self.inspect(container)["State"]
        if state.get("Paused"):
            raise UnsafeConflict(f"{container} was explicitly paused")
        if state["Status"] not in ("running", "restarting"):
            self.command("start", container)

    def restore_bridge(self):
        # Finish any previous interrupted transaction before declaring success.
        self.restore_pending()
        app = self.inspect(APP)
        networks = app["NetworkSettings"]["Networks"]
        if app["State"]["Status"] != "running":
            return False
        if not networks.get(APP_NETWORK, {}).get("IPAddress"):
            raise UnsafeConflict(f"{APP} is missing its Compose application network")
        endpoint = networks.get(BRIDGE, {})
        if endpoint.get("IPAddress") == ADDRESS and endpoint.get("IPPrefixLen") == 16:
            return True

        bridge = json.loads(self.command("network", "inspect", BRIDGE))[0]
        occupants = [
            (container_id, entry["Name"])
            for container_id, entry in (bridge.get("Containers") or {}).items()
            if entry["IPv4Address"] == f"{ADDRESS}/16"
            and container_id != app["Id"]
        ]
        peer = None
        if occupants:
            peer_id, name = occupants[0]
            if name not in PEER_NETWORKS:
                raise UnsafeConflict(f"refusing to displace unexpected bridge owner {name}")
            peer = self.inspect(peer_id)
            peer_nets = peer["NetworkSettings"]["Networks"]
            peer_bridge = peer_nets.get(BRIDGE, {})
            dynamic = not any((peer_bridge.get("IPAMConfig") or {}).values())
            alternate = any(
                peer_nets.get(net, {}).get("IPAddress") for net in PEER_NETWORKS[name]
            )
            if (peer["State"]["Status"] != "running" or peer["State"].get("Paused")
                    or not alternate or not dynamic
                    or peer_bridge.get("IPAddress") != ADDRESS
                    or peer_bridge.get("Links") or peer_bridge.get("Aliases")
                    or peer_bridge.get("DriverOpts")):
                raise UnsafeConflict(f"{name} does not match the verified dynamic-network layout")
            log(f"reassigning {name}'s dynamic bridge address; retaining its application network")

        # Record candidates before disconnecting: a CLI timeout can occur after
        # Docker has already applied the change. Always inspect during cleanup.
        restore = []
        if peer:
            restore.append((peer["Id"], peer_bridge.get("GwPriority", 0)))
        if endpoint:
            restore.append((app["Id"], endpoint.get("GwPriority", -1)))
        self.save_pending(restore)
        try:
            if endpoint:
                self.command("network", "disconnect", BRIDGE, app["Id"])
            if peer:
                self.command("network", "disconnect", BRIDGE, peer["Id"])
            self.command("network", "connect", "--gw-priority", "-1", BRIDGE, app["Id"])
            fixed = self.networks(app["Id"]).get(BRIDGE, {})
            return fixed.get("IPAddress") == ADDRESS and fixed.get("IPPrefixLen") == 16
        finally:
            self.restore_pending()

    def healthy(self, container):
        state = self.inspect(container)["State"]
        return state["Status"] == "running" and state.get("Health", {}).get("Status") == "healthy"

    def ready(self):
        app = self.inspect(APP)
        networks = app["NetworkSettings"]["Networks"]
        bridge = networks.get(BRIDGE, {})
        if (not self.healthy(APP) or not networks.get(APP_NETWORK, {}).get("IPAddress")
                or bridge.get("IPAddress") != ADDRESS or bridge.get("IPPrefixLen") != 16):
            return False
        self.command("exec", APP, "nc", "-z", "-w", "3", "172.17.0.1", "10834")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open("http://127.0.0.1:13080/health", timeout=5) as response:
            return response.status == 200 and json.load(response).get("status") == "ok"

    def run(self):
        deadline = time.monotonic() + 180

        def wait_for(description, action):
            last_error = "not ready"
            while time.monotonic() < deadline:
                try:
                    if action():
                        return
                except UnsafeConflict:
                    raise
                except (subprocess.SubprocessError, OSError, ValueError, RuntimeError) as error:
                    last_error = str(error)
                time.sleep(1)
            raise RuntimeError(f"timed out waiting for {description}: {last_error}")

        wait_for("Docker", lambda: self.command("info", "--format", "{{.ServerVersion}}"))
        wait_for("unfinished bridge cleanup", lambda: self.restore_pending() or True)
        for dependency in ("sub2api-prod-postgres", "sub2api-prod-redis"):
            def dependency_ready():
                self.start(dependency)
                return self.healthy(dependency)
            wait_for(dependency, dependency_ready)

        def network_ready():
            self.start(APP)
            return self.restore_bridge()

        wait_for("production bridge", network_ready)
        wait_for("application health and host proxy", self.ready)
        log(f"{APP} owns {ADDRESS}/16; dependencies, health and host proxy verified")


def interrupted(signum, _frame):
    # Let the bridge cleanup run on systemd stop/timeout as well as exceptions.
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        Recovery().run()
    except Exception as error:
        log(f"FAILED: {error}")
        raise SystemExit(1)
