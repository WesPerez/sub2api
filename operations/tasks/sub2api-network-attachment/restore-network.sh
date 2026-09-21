#!/bin/sh
set -eu

# Serialize with both network recovery and the guarded image rollout.
exec 8>/run/lock/sub2api-upgrade.lock
flock -n 8 || exit 0
exec 9>/run/sub2api-prod-network.lock
flock -n 9 || exit 0

exec /usr/bin/python3 /opt/sub2api-operations/operations/tasks/sub2api-network-attachment/restore-network.py
