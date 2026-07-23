#!/usr/bin/env bash
# Contract tests for release/promotion workflows.
# Pure bash/grep/awk + Python stdlib only (no PyYAML dependency).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKER_WF="${ROOT_DIR}/.github/workflows/docker-branch.yml"
PROMOTE_WF="${ROOT_DIR}/.github/workflows/promote-debug-image.yml"
BACKEND_CI="${ROOT_DIR}/.github/workflows/backend-ci.yml"
PROVENANCE_SCRIPT="${ROOT_DIR}/deploy/verify-image-provenance.py"
FAIL=0

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAIL=1; }

need_file() {
  if [[ ! -f "$1" ]]; then
    fail "missing file $1"
    return 1
  fi
  pass "found $(basename "$1")"
}

file_contains() {
  local file="$1" pattern="$2" msg="$3"
  if grep -Eq -- "$pattern" "$file"; then
    pass "$msg"
  else
    fail "$msg (pattern: $pattern)"
  fi
}

need_file "$DOCKER_WF" || true
need_file "$PROMOTE_WF" || true
need_file "$BACKEND_CI" || true
need_file "$PROVENANCE_SCRIPT" || true

# Verifier must require content binding flags and refuse signature claims.
file_contains "$PROVENANCE_SCRIPT" '--manifest' "verifier accepts --manifest"
file_contains "$PROVENANCE_SCRIPT" '--image-digest' "verifier accepts --image-digest"
file_contains "$PROVENANCE_SCRIPT" 'attestation-manifest' "verifier checks attestation-manifest binding"
file_contains "$PROVENANCE_SCRIPT" 'attestation manifest digest' "verifier hashes attestation manifest bytes against index descriptor"
file_contains "$PROVENANCE_SCRIPT" 'attestation_manifest_content_checked' "verifier reports attestation manifest content binding"
file_contains "$PROVENANCE_SCRIPT" 'structural-not-signature' "verifier labels checks as structural not signature"
if grep -Eqi 'signature verified|cosign|sigstore verify' "$PROVENANCE_SCRIPT"; then
  fail "verifier must not claim cryptographic signature verification"
else
  pass "verifier does not claim signature verification"
fi

file_contains "$DOCKER_WF" '[[:space:]]+- debug' "docker-branch lists debug branch"
if grep -Eq '[[:space:]]+- mine[[:space:]]*$' "$DOCKER_WF"; then
  fail "docker-branch must not list mine under push branches"
else
  pass "docker-branch does not push-build mine"
fi
file_contains "$DOCKER_WF" 'workflow_dispatch' "docker-branch allows workflow_dispatch"
file_contains "$DOCKER_WF" 'refs/heads/debug' "docker-branch validates debug ref"
file_contains "$DOCKER_WF" 'cancel-in-progress:[[:space:]]*false' "docker-branch cancel-in-progress false"
file_contains "$DOCKER_WF" 'needs:[[:space:]]*validate' "verify/build depend on validate"
file_contains "$DOCKER_WF" 'needs:[[:space:]]*\[verify, build\]' "publish needs verify and build (failed CI never publishes)"
file_contains "$DOCKER_WF" 'uses:[[:space:]]*\./\.github/workflows/backend-ci\.yml' "verify calls backend-ci"
file_contains "$BACKEND_CI" 'workflow-release-contract-test\.sh' "backend-ci runs workflow-release-contract-test.sh"
file_contains "$BACKEND_CI" 'verify-image-provenance-test\.sh' "backend-ci runs provenance verifier tests"
file_contains "$BACKEND_CI" 'make test-unit' "backend-ci runs unit tests"
file_contains "$BACKEND_CI" 'make test-integration' "backend-ci runs integration tests"

if python3 - "$DOCKER_WF" "$PROMOTE_WF" "$BACKEND_CI" <<'PY'
import re, sys
from pathlib import Path

def load(path):
    return Path(path).read_text(encoding="utf-8")

def job_blocks(text):
    m = re.search(r"^jobs:\n(.*)\Z", text, re.M | re.S)
    if not m:
        return {}
    body = m.group(1)
    parts = re.split(r"^(?=  [A-Za-z0-9_-]+:)", body, flags=re.M)
    out = {}
    for p in parts:
        p = p.strip("\n")
        if not p.strip():
            continue
        name = p.split(":", 1)[0].strip()
        out[name] = p
    return out

def job_has_packages_write(block):
    return bool(re.search(r"(?m)^    permissions:[\s\S]*?packages:\s*write", block))

errors = []
docker = load(sys.argv[1])
promote = load(sys.argv[2])
backend = load(sys.argv[3])
jobs = job_blocks(docker)
backend_jobs = job_blocks(backend)

for required in ("validate", "verify", "build", "publish"):
    if required not in jobs:
        errors.append(f"docker-branch missing job {required}")

for required in ("test-unit", "test-integration"):
    if required not in backend_jobs:
        errors.append(f"backend-ci missing job {required}")

if "test-unit" in backend_jobs and "test-integration" in backend_jobs:
    unit = backend_jobs["test-unit"]
    integration = backend_jobs["test-integration"]
    if backend.count("make test-unit") != 1 or "make test-unit" not in unit:
        errors.append("backend-ci must run make test-unit exactly once in test-unit")
    if backend.count("make test-integration") != 1 or "make test-integration" not in integration:
        errors.append("backend-ci must run make test-integration exactly once in test-integration")
    if "make test-integration" in unit or "make test-unit" in integration:
        errors.append("unit and integration commands must remain in separate jobs")
    for name, block in (("test-unit", unit), ("test-integration", integration)):
        if re.search(r"(?m)^    needs:", block):
            errors.append(f"backend-ci {name} must remain independently runnable for parallel execution")
        if "actions/setup-go@v6" not in block or "go-version-file: backend/go.mod" not in block:
            errors.append(f"backend-ci {name} must use the pinned Go toolchain setup")
        if "cache: true" not in block or "cache-dependency-path: backend/go.sum" not in block:
            errors.append(f"backend-ci {name} must retain the Go module/build cache")

if "build" in jobs:
    b = jobs["build"]
    if job_has_packages_write(b):
        errors.append("build job must not have packages: write")
    if re.search(r"docker/login-action", b):
        errors.append("build job must not login to registry")
    if re.search(r"(?m)^\s+push:\s*true\s*$", b):
        errors.append("build job must not push: true")
    if "type=gha" not in b or "mode=max" not in b:
        errors.append("build job must use type=gha mode=max cache")
    if "scope=docker-branch-debug-${{ github.sha }}" not in b:
        errors.append("unverified parallel build may write only its candidate-SHA cache scope")
    if "cache-from: type=gha,scope=docker-branch-debug-trusted" not in b:
        errors.append("parallel build must read the last verify-gated trusted cache")

if "publish" in jobs:
    p = jobs["publish"]
    if not job_has_packages_write(p):
        errors.append("publish job must have packages: write")
    if not re.search(r"needs:\s*\[verify,\s*build\]", p):
        errors.append("publish must need verify and build")
    if "debug-sha-" not in p:
        errors.append("publish must push debug-sha-<40sha> style tag")
    if "provenance: mode=max" not in p:
        errors.append("published immutable image must enable max provenance")
    if "scope=docker-branch-debug-${{ github.sha }}" not in p:
        errors.append("verify-gated publish must consume the candidate-SHA cache")
    provenance_pos = p.find("Verify immutable image SLSA provenance")
    trusted_step_pos = p.find("Promote verified candidate cache to trusted")
    trusted_writes = [
        match.start()
        for match in re.finditer(
            r"cache-to:\s*type=gha,mode=max,scope=docker-branch-debug-trusted",
            p,
        )
    ]
    if provenance_pos < 0 or trusted_step_pos < 0 or not (provenance_pos < trusted_step_pos):
        errors.append("trusted cache promotion must run only after immutable image provenance verification")
    if len(trusted_writes) != 1 or trusted_writes[0] < trusted_step_pos:
        errors.append("trusted cache must be written exactly once by the post-provenance cache promotion step")
    if "imagetools create" not in p:
        errors.append("publish must carbon-copy floating tags via imagetools create")
    if "verify-image-provenance.py" not in p or "publisher_run_id" not in p or "reused_existing" not in p:
        errors.append("existing debug-sha recovery must require trusted SLSA provenance and record publisher run")
    if p.count("--manifest") < 2 or p.count("--image-digest") < 2:
        errors.append("all publish provenance checks must bind imagetools manifest JSON and image digest")
    if "--attestation" not in p or "attestation-manifest" not in p:
        errors.append("publish provenance checks must fetch and bind the attestation manifest")
    if p.count("--statement") < 2 or p.count("/blobs/${STATEMENT_DIGEST}") < 2:
        errors.append("all publish provenance checks must download and bind the in-toto statement blob")
    if p.count('service=ghcr.io') < 2:
        errors.append("all publish GHCR token requests must bind service=ghcr.io")
    if p.count('curl -fsS --location') < 2:
        errors.append("all publish statement blob downloads must follow GHCR redirects")
    if "reused_existing != 'true'" not in p and 'reused_existing != "true"' not in p:
        errors.append("trusted cache promotion must skip when reused_existing=true (cache-only rebuild not proven equivalent)")
    if "CHECK_REF" not in docker or "CHECK_REF_NAME" not in docker:
        errors.append("docker-branch must compare github.ref/ref_name via env vars, not direct shell interpolation")
    if re.search(r'\[\s*"\$\{\{\s*github\.(ref|ref_name)\s*\}\}"\s*!=', docker):
        errors.append("docker-branch must not compare github.ref/ref_name via direct interpolation")
    if "--format '{{json .}}'" not in p or ".Manifest.Digest" in p:
        errors.append("publish must parse structured imagetools JSON")

top = docker.split("jobs:", 1)[0]
if re.search(r"(?m)^permissions:[\s\S]*packages:\s*write", top):
    errors.append("docker-branch top-level must not set packages: write (only publish job)")
if "actions: write" in docker:
    errors.append("docker-branch must not grant actions: write")

if "promote" not in job_blocks(promote):
    errors.append("promote-debug-image missing promote job")
else:
    p = job_blocks(promote)["promote"]
    if not promote.startswith("name: Promote Debug Image\n"):
        errors.append("promote workflow name must match verifier exactly")
    for key in (
        "expected_revision",
        "source_digest",
        "source_run_id",
        "verification_evidence_sha256",
    ):
        if key not in promote:
            errors.append(f"promote missing input {key}")
    on = promote.split("jobs:", 1)[0]
    if "workflow_dispatch" not in on:
        errors.append("promote must be workflow_dispatch only")
    if re.search(r"(?m)^  push:", on):
        errors.append("promote must not trigger on push")
    if "cancel-in-progress: false" not in promote:
        errors.append("promote concurrency cancel-in-progress must be false")
    if "refs/heads/mine" not in promote:
        errors.append("promote must require selected ref mine")
    if "origin/mine" not in p or "origin/debug" not in p:
        errors.append("promote must check origin/mine and origin/debug")
    if p.count("git fetch origin mine debug") < 3:
        errors.append("promote must check heads early, before writes, and immediately before floating mine")
    if "head_branch" not in p or "head_sha" not in p:
        errors.append("promote must validate source run head_branch/head_sha")
    if 'data.get("event") not in ("push", "workflow_dispatch")' not in p or 'str(data.get("id"))' not in p:
        errors.append("promote must validate source run id and allowed event")
    if ".github/workflows/docker-branch.yml" not in p:
        errors.append("promote must require source run path docker-branch.yml")
    if "imagetools create" not in p:
        errors.append("promote must carbon-copy with imagetools create")
    if "mine-sha-" not in p:
        errors.append("promote must write mine-sha tags")
    pos_sha = p.find("Carbon-copy mine-sha")
    pos_short = p.find("Carbon-copy mine short-sha")
    pos_float = p.find("Carbon-copy floating mine tag")
    if min(pos_sha, pos_short, pos_float) < 0 or not (pos_sha < pos_short < pos_float):
        errors.append("promote tag order must be mine-sha, mine-<12>, mine last")
    if "different digest" not in p:
        errors.append("promote must fail when mine-sha exists with different digest")
    if "promotion.json" not in p:
        errors.append("promote must write promotion.json")
    for field in ("schema_version", "promotion_run_id", "source_run_id", "publisher_run_id", "publisher_run_attempt", "reused_existing_debug_image", "revision", "source_digest", "target_digest", "verification_evidence_sha256", "evidence_binding_mode", "tags"):
        if f'"{field}"' not in p:
            errors.append(f"promotion.json payload must include {field}")
    if "recorded-hash-production-apply-verifies-local-file" not in p:
        errors.append("promotion receipt must state that production apply verifies the local evidence file")
    if "umask 077" not in p and "0o600" not in p:
        errors.append("promote must create promotion.json with restrictive mode")
    if "build-push-action" in p:
        errors.append("promote must not build images")
    if re.search(r"ref\.name[=:] ?mine", p) or "image.ref.name=mine" in p:
        errors.append("promote must not claim ref.name=mine")
    if "extractall" in p:
        errors.append("promote must not use unsafe zip extractall")
    if "--format '{{json .}}'" not in p or ".Manifest.Digest" in p or ".Image.Config.Labels" in p:
        errors.append("promote must parse structured imagetools JSON")
    pre_float = p.find("Remote head check immediately before floating mine")
    float_write = p.find("Carbon-copy floating mine tag")
    receipt = p.find("Write promotion.json artifact")
    if min(pre_float, float_write, receipt) < 0 or not (pre_float < float_write < receipt):
        errors.append("promote must recheck heads before floating mine, verify it, then write receipt")

# Hard gate: the source run must emit a unique metadata artifact that binds its
# immutable registry digest, and promotion must download and verify it.
if "debug-image-metadata" not in docker or "debug-image.json" not in docker:
    errors.append("publish must upload debug-image-metadata/debug-image.json")
if len(re.findall(r"(?m)^\s+name:\s*debug-image-metadata\s*$", docker)) != 1:
    errors.append("publish must define exactly one debug-image-metadata artifact")
for field in ("schema_version", "source_run_id", "publisher_run_id", "publisher_run_attempt", "reused_existing", "revision", "digest", "immutable_tag", "image"):
    if field not in docker:
        errors.append(f"debug-image.json payload must include {field}")
if "0o600" not in docker and "umask 077" not in docker:
    errors.append("debug-image.json must be written with restrictive mode")
if "debug-image-metadata" not in promote:
    errors.append("promote must download debug-image-metadata from source_run_id")
if "len(arts) != 1" not in promote:
    errors.append("promote must require exactly one debug-image-metadata artifact")
if "expired" not in promote:
    errors.append("promote must reject expired debug-image-metadata")
if "total_count" not in promote or "uniqueness cannot be proven" not in promote:
    errors.append("promote must fail if artifact pagination prevents uniqueness proof")
if "debug-image.json does not bind" not in promote:
    errors.append("promote must verify debug-image.json fields against inputs")
if promote.count("verify-image-provenance.py") < 1 or "META_PUBLISHER_RUN_ID" not in promote:
    errors.append("promote must verify source image SLSA provenance matches metadata publisher")
if promote.count("--manifest") < 1 or promote.count("--image-digest") < 1:
    errors.append("promote provenance check must bind imagetools manifest JSON and source digest")
if "--attestation" not in promote or "attestation-manifest" not in promote:
    errors.append("promote must fetch and bind the attestation manifest")
if "--statement" not in promote or "/blobs/${STATEMENT_DIGEST}" not in promote:
    errors.append("promote must download and bind the in-toto statement blob")
if "service=ghcr.io" not in promote:
    errors.append("promote GHCR token request must bind service=ghcr.io")
if "curl -fsS --location" not in promote:
    errors.append("promote statement blob download must follow GHCR redirects")
if "CHECK_REF" not in promote or "CHECK_REF_NAME" not in promote:
    errors.append("promote must compare github.ref/ref_name via env vars, not direct shell interpolation")
if re.search(r'\[\s*"\$\{\{\s*github\.(ref|ref_name)\s*\}\}"\s*!=', promote):
    errors.append("promote must not compare github.ref/ref_name via direct ${{ }} interpolation")
if "overwrite: true" not in docker or "overwrite: true" not in promote:
    errors.append("reruns must atomically replace same-name artifacts instead of creating ambiguous duplicates")
for regex_literal in ("^[0-9a-f]{40}$", "^sha256:[0-9a-f]{64}$", "^[0-9]+$", "^[0-9a-f]{64}$"):
    if regex_literal not in promote:
        errors.append(f"promote missing strict input regex {regex_literal}")

if errors:
    for e in errors:
        print("FAIL:", e)
    sys.exit(1)
print("PASS: docker-branch and promote structural contracts")
PY
then
  pass "structural python contracts"
else
  fail "structural python contracts"
fi

if [[ "$FAIL" -ne 0 ]]; then
  echo "workflow-release-contract-test: FAILED" >&2
  exit 1
fi
echo "workflow-release-contract-test: OK"
