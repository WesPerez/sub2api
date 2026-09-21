#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec /usr/bin/python3 -m unittest discover -s "${SCRIPT_DIR}/tests" -p 'test_*.py' -v
