#!/bin/sh
# Stable deployment entry point; implementation belongs to project operations.
exec /opt/sub2api-operations/operations/tasks/sub2api-network-attachment/restore-network.sh "$@"
