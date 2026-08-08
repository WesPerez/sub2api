#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/deploy/verify-image-provenance.py"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT

REV="1234567890abcdef1234567890abcdef12345678"
REPO="WesPerez/sub2api"
INDEX_DIGEST="sha256:1111111111111111111111111111111111111111111111111111111111111111"
IMAGE_MANIFEST_DIGEST="sha256:2222222222222222222222222222222222222222222222222222222222222222"
ATTEST_MANIFEST_DIGEST=""

write_provenance() {
  jq -n --arg revision "$REV" --arg repo "$REPO" '
    {SLSA:{buildDefinition:{externalParameters:{request:{root:{request:{args:{
      "vcs:revision":$revision,"vcs:source":("https://github.com/"+$repo)
    }}}}},internalParameters:{
      github_repository:$repo,
      github_workflow:"Docker Branch Images",
      github_workflow_ref:($repo+"/.github/workflows/docker-branch.yml@refs/heads/debug"),
      github_workflow_sha:$revision,
      github_job:"publish",
      github_ref:"refs/heads/debug",
      github_event_name:"push"
    }},runDetails:{builder:{id:("https://github.com/"+$repo+"/actions/runs/1001/attempts/2")}}}}
  ' > "$1"
}

write_manifest() {
  local dest="$1" attest_digest="${2:-$ATTEST_MANIFEST_DIGEST}"
  jq -n \
    --arg index "$INDEX_DIGEST" \
    --arg image "$IMAGE_MANIFEST_DIGEST" \
    --arg attest "$attest_digest" \
    '
    {
      name: "ghcr.io/example/sub2api:debug-sha",
      manifest: {
        schemaVersion: 2,
        mediaType: "application/vnd.oci.image.index.v1+json",
        digest: $index,
        size: 1234,
        manifests: [
          {
            mediaType: "application/vnd.oci.image.manifest.v1+json",
            digest: $image,
            size: 100,
            platform: {architecture: "amd64", os: "linux"}
          },
          {
            mediaType: "application/vnd.oci.image.manifest.v1+json",
            digest: $attest,
            size: 200,
            annotations: {
              "vnd.docker.reference.type": "attestation-manifest",
              "vnd.docker.reference.digest": $image
            },
            platform: {architecture: "unknown", os: "unknown"}
          }
        ]
      }
    }
    ' > "$dest"
}

write_statement() {
  local dest="$1" provenance="$2"
  jq -n --slurpfile provenance "$provenance" --arg image "${IMAGE_MANIFEST_DIGEST#sha256:}" '
    {
      _type: "https://in-toto.io/Statement/v1",
      predicateType: "https://slsa.dev/provenance/v1",
      subject: [
        {name:"pkg:docker/ghcr.io/wesperez/sub2api@debug?platform=linux%2Famd64",digest:{sha256:$image}},
        {name:"pkg:docker/ghcr.io/wesperez/sub2api@debug-sha?platform=linux%2Famd64",digest:{sha256:$image}}
      ],
      predicate: $provenance[0].SLSA
    }
  ' > "$dest"
}

write_attestation() {
  local dest="$1" statement="$2" layer
  layer="sha256:$(sha256sum "$statement" | awk '{print $1}')"
  jq -n \
    --arg layer "$layer" \
    '
    {
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      artifactType: "application/vnd.docker.attestation.manifest.v1+json",
      layers: [
        {
          mediaType: "application/vnd.in-toto+json",
          digest: $layer,
          size: 50,
          annotations: {"in-toto.io/predicate-type": "https://slsa.dev/provenance/v1"}
        }
      ]
    }
    ' > "$1"
}

run_verify() {
  local prov="$1" manifest="$2" attestation="$3" statement="$4"
  shift 4
  python3 "$SCRIPT" \
    --provenance "$prov" \
    --manifest "$manifest" \
    --image-digest "$INDEX_DIGEST" \
    --attestation "$attestation" \
    --statement "$statement" \
    --repository "$REPO" \
    --revision "$REV" \
    --workflow-name "Docker Branch Images" \
    --workflow-path .github/workflows/docker-branch.yml \
    --job publish \
    --ref refs/heads/debug \
    --server-url https://github.com \
    "$@"
}

write_provenance "$TMP/good-prov.json"
write_statement "$TMP/good-statement.json" "$TMP/good-prov.json"
write_attestation "$TMP/good-attest.json" "$TMP/good-statement.json"
ATTEST_MANIFEST_DIGEST="sha256:$(sha256sum "$TMP/good-attest.json" | awk '{print $1}')"
write_manifest "$TMP/good-manifest.json"

run_verify "$TMP/good-prov.json" "$TMP/good-manifest.json" "$TMP/good-attest.json" "$TMP/good-statement.json" \
  | jq -e '.publisher_run_id=="1001"
      and .publisher_run_attempt=="2"
      and .verification_kind=="structural-not-signature"
      and .image_digest=="'"$INDEX_DIGEST"'"
      and .image_manifest_digest=="'"$IMAGE_MANIFEST_DIGEST"'"
      and .attestation_manifest_digest=="'"$ATTEST_MANIFEST_DIGEST"'"
      and .attestation_manifest_content_checked==true
      and .attestation_layer_shape_checked==true
      and .statement_subject_checked==true
      and (.statement_digest|test("^sha256:[0-9a-f]{64}$"))' >/dev/null
echo "PASS provenance happy with manifest+attestation+statement binding"

jq '.SLSA.buildDefinition.internalParameters.github_workflow="Other"' "$TMP/good-prov.json" > "$TMP/bad-workflow.json"
if run_verify "$TMP/bad-workflow.json" "$TMP/good-manifest.json" "$TMP/good-attest.json" "$TMP/good-statement.json" >/dev/null 2>&1; then exit 1; fi
echo "PASS provenance wrong workflow rejected"

jq '.SLSA.buildDefinition.externalParameters.request.root.request.args["vcs:revision"]="ffffffffffffffffffffffffffffffffffffffff"' \
  "$TMP/good-prov.json" > "$TMP/bad-revision.json"
if run_verify "$TMP/bad-revision.json" "$TMP/good-manifest.json" "$TMP/good-attest.json" "$TMP/good-statement.json" >/dev/null 2>&1; then exit 1; fi
echo "PASS provenance wrong revision rejected"

jq '.SLSA.runDetails.builder.id="https://example.invalid/run/1"' "$TMP/good-prov.json" > "$TMP/bad-builder.json"
if run_verify "$TMP/bad-builder.json" "$TMP/good-manifest.json" "$TMP/good-attest.json" "$TMP/good-statement.json" >/dev/null 2>&1; then exit 1; fi
echo "PASS provenance wrong builder rejected"

if python3 "$SCRIPT" \
  --provenance "$TMP/good-prov.json" \
    --manifest "$TMP/good-manifest.json" \
    --image-digest "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
    --attestation "$TMP/good-attest.json" --statement "$TMP/good-statement.json" \
  --repository "$REPO" --revision "$REV" \
  --workflow-name "Docker Branch Images" --workflow-path .github/workflows/docker-branch.yml \
  --job publish --ref refs/heads/debug --server-url https://github.com >/dev/null 2>&1; then
  exit 1
fi
echo "PASS index digest mismatch rejected"

jq '.manifest.manifests[1].annotations["vnd.docker.reference.digest"]="sha256:9999999999999999999999999999999999999999999999999999999999999999"' \
  "$TMP/good-manifest.json" > "$TMP/bad-ref.json"
if run_verify "$TMP/good-prov.json" "$TMP/bad-ref.json" "$TMP/good-attest.json" "$TMP/good-statement.json" >/dev/null 2>&1; then exit 1; fi
echo "PASS attestation reference digest mismatch rejected"

jq '.manifest.manifests=[.manifest.manifests[0]]' "$TMP/good-manifest.json" > "$TMP/no-attest.json"
if run_verify "$TMP/good-prov.json" "$TMP/no-attest.json" "$TMP/good-attest.json" "$TMP/good-statement.json" >/dev/null 2>&1; then exit 1; fi
echo "PASS missing attestation-manifest rejected"

jq '.manifest.manifests += [{
  mediaType:"application/vnd.oci.image.manifest.v1+json",
  digest:"sha256:5555555555555555555555555555555555555555555555555555555555555555",
  size:1,
  platform:{architecture:"arm64", os:"linux"}
}]' "$TMP/good-manifest.json" > "$TMP/extra-platform.json"
if run_verify "$TMP/good-prov.json" "$TMP/extra-platform.json" "$TMP/good-attest.json" "$TMP/good-statement.json" >/dev/null 2>&1; then exit 1; fi
echo "PASS extra image platform rejected"

jq '.layers[0].annotations["in-toto.io/predicate-type"]="https://spdx.dev/Document"' \
  "$TMP/good-attest.json" > "$TMP/bad-layer.json"
BAD_LAYER_ATTEST_DIGEST="sha256:$(sha256sum "$TMP/bad-layer.json" | awk '{print $1}')"
write_manifest "$TMP/bad-layer-manifest.json" "$BAD_LAYER_ATTEST_DIGEST"
if run_verify "$TMP/good-prov.json" "$TMP/bad-layer-manifest.json" "$TMP/bad-layer.json" "$TMP/good-statement.json" >/dev/null 2>&1; then
  exit 1
fi
echo "PASS attestation without SLSA layer rejected"

cp "$TMP/good-attest.json" "$TMP/tampered-attestation.json"
printf ' ' >> "$TMP/tampered-attestation.json"
if run_verify "$TMP/good-prov.json" "$TMP/good-manifest.json" "$TMP/tampered-attestation.json" "$TMP/good-statement.json" >/dev/null 2>&1; then
  exit 1
fi
echo "PASS attestation manifest blob digest mismatch rejected"

jq '.subject[0].digest.sha256="9999999999999999999999999999999999999999999999999999999999999999"' \
  "$TMP/good-statement.json" > "$TMP/bad-subject.json"
write_attestation "$TMP/bad-subject-attest.json" "$TMP/bad-subject.json"
BAD_SUBJECT_ATTEST_DIGEST="sha256:$(sha256sum "$TMP/bad-subject-attest.json" | awk '{print $1}')"
write_manifest "$TMP/bad-subject-manifest.json" "$BAD_SUBJECT_ATTEST_DIGEST"
if run_verify "$TMP/good-prov.json" "$TMP/bad-subject-manifest.json" "$TMP/bad-subject-attest.json" "$TMP/bad-subject.json" >/dev/null 2>&1; then
  exit 1
fi
echo "PASS attestation subject mismatch rejected"

jq '.predicate.buildDefinition.internalParameters.github_job="other"' \
  "$TMP/good-statement.json" > "$TMP/bad-predicate.json"
write_attestation "$TMP/bad-predicate-attest.json" "$TMP/bad-predicate.json"
BAD_PREDICATE_ATTEST_DIGEST="sha256:$(sha256sum "$TMP/bad-predicate-attest.json" | awk '{print $1}')"
write_manifest "$TMP/bad-predicate-manifest.json" "$BAD_PREDICATE_ATTEST_DIGEST"
if run_verify "$TMP/good-prov.json" "$TMP/bad-predicate-manifest.json" "$TMP/bad-predicate-attest.json" "$TMP/bad-predicate.json" >/dev/null 2>&1; then
  exit 1
fi
echo "PASS statement predicate mismatch rejected"

cp "$TMP/good-statement.json" "$TMP/tampered-statement.json"
printf ' ' >> "$TMP/tampered-statement.json"
if run_verify "$TMP/good-prov.json" "$TMP/good-manifest.json" "$TMP/good-attest.json" "$TMP/tampered-statement.json" >/dev/null 2>&1; then
  exit 1
fi
echo "PASS statement blob digest mismatch rejected"

if python3 "$SCRIPT" --provenance "$TMP/good-prov.json" --manifest "$TMP/good-manifest.json" \
  --image-digest "$INDEX_DIGEST" --attestation "$TMP/good-attest.json" \
  --repository "$REPO" --revision "$REV" \
  --workflow-name "Docker Branch Images" --workflow-path .github/workflows/docker-branch.yml \
  --job publish --ref refs/heads/debug >/dev/null 2>&1; then
  exit 1
fi
echo "PASS missing statement rejected"
