#!/usr/bin/python3
"""Preserve the existing deployment path without a second implementation."""
import os
import sys

if __name__ == "__main__":
    os.execv("/usr/bin/python3", ["/usr/bin/python3",
             "/opt/sub2api-operations/operations/tasks/sub2api-network-attachment/restore-network.py",
             *sys.argv[1:]])
