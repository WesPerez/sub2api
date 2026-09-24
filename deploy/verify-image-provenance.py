#!/usr/bin/env python3
"""Fail-closed structural validator for BuildKit SLSA provenance + OCI index binding.

This checks GitHub Actions build metadata and image/attestation content binding
from imagetools output. It does NOT perform cryptographic signature verification.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any


DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
SLSA_PREDICATE = "https://slsa.dev/provenance/v1"
IN_TOTO_LAYER = "application/vnd.in-toto+json"
OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
DOCKER_MANIFEST = "application/vnd.docker.distribution.manifest.v2+json"
ATT_REF_TYPE = "attestation-manifest"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Fail-closed structural checks for BuildKit SLSA provenance and OCI "
            "index/attestation binding. Not signature verification."
        )
    )
    parser.add_argument("--provenance", required=True, help="imagetools {{json .Provenance}} file")
    parser.add_argument(
        "--manifest",
        required=True,
        help="imagetools {{json .}} inspect file or OCI index JSON",
    )
    parser.add_argument(
        "--image-digest",
        required=True,
        help="Expected top-level index/manifest digest (sha256:...)",
    )
    parser.add_argument(
        "--attestation",
        required=True,
        help="Raw attestation manifest JSON containing the SLSA layer descriptor",
    )
    parser.add_argument(
        "--statement",
        required=True,
        help="Raw in-toto statement blob referenced by the SLSA layer descriptor",
    )
    parser.add_argument("--repository", required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--workflow-name", required=True)
    parser.add_argument("--workflow-path", required=True)
    parser.add_argument("--job", required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--server-url", default="https://github.com")
    return parser.parse_args()


def load_json(path: str) -> Any:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def extract_slsa(data: dict[str, Any]) -> dict[str, Any]:
    """Accept top-level SLSA (confirmed on real debug) or linux/amd64-keyed map."""
    if isinstance(data.get("SLSA"), dict):
        return data["SLSA"]
    platform = data.get("linux/amd64")
    if isinstance(platform, dict) and isinstance(platform.get("SLSA"), dict):
        return platform["SLSA"]
    if len(data) == 1:
        only = next(iter(data.values()))
        if isinstance(only, dict) and isinstance(only.get("SLSA"), dict):
            return only["SLSA"]
    raise ValueError("provenance JSON missing SLSA object")


def is_attestation_entry(entry: dict[str, Any]) -> bool:
    ann = entry.get("annotations") or {}
    return ann.get("vnd.docker.reference.type") == ATT_REF_TYPE


def is_linux_amd64(entry: dict[str, Any]) -> bool:
    platform = entry.get("platform") or {}
    return platform.get("os") == "linux" and platform.get("architecture") == "amd64"


def is_image_manifest_media_type(media_type: str) -> bool:
    return media_type in (OCI_MANIFEST, DOCKER_MANIFEST, "")


def resolve_index(path: str, image_digest: str) -> dict[str, Any]:
    raw = Path(path).read_bytes()
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"manifest is not valid JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError("manifest JSON must be an object")

    if isinstance(data.get("manifest"), dict) and isinstance(
        (data.get("manifest") or {}).get("manifests"), list
    ):
        index = data["manifest"]
        digest = str(index.get("digest") or "")
    elif isinstance(data.get("manifests"), list):
        index = data
        digest = str(data.get("digest") or "")
        if not digest:
            digest = "sha256:" + hashlib.sha256(raw).hexdigest()
    else:
        raise ValueError(
            "manifest must be imagetools inspect JSON with .manifest.manifests "
            "or an OCI index with manifests"
        )

    if not DIGEST_RE.fullmatch(image_digest):
        raise ValueError("image-digest must be sha256 plus 64 lowercase hex chars")
    if not DIGEST_RE.fullmatch(digest):
        raise ValueError(f"top-level index digest is invalid: {digest!r}")
    if digest != image_digest:
        raise ValueError(f"top-level index digest {digest!r} != image-digest {image_digest!r}")

    media_type = str(index.get("mediaType") or "")
    if media_type and media_type != OCI_INDEX:
        raise ValueError(f"top-level mediaType must be OCI index, got {media_type!r}")
    return index


def bind_index(index: dict[str, Any]) -> tuple[str, str]:
    manifests = index.get("manifests")
    if not isinstance(manifests, list) or not manifests:
        raise ValueError("index manifests must be a non-empty list")

    images: list[dict[str, Any]] = []
    attestations: list[dict[str, Any]] = []
    for entry in manifests:
        if not isinstance(entry, dict):
            raise ValueError("index manifest entry must be an object")
        digest = str(entry.get("digest") or "")
        if not DIGEST_RE.fullmatch(digest):
            raise ValueError(f"manifest entry digest invalid: {digest!r}")
        if is_attestation_entry(entry):
            attestations.append(entry)
        else:
            images.append(entry)

    if len(images) != 1:
        raise ValueError(f"want exactly one image manifest, found {len(images)}")
    image = images[0]
    if not is_linux_amd64(image):
        raise ValueError("image manifest platform must be linux/amd64")
    image_mt = str(image.get("mediaType") or "")
    if image_mt and not is_image_manifest_media_type(image_mt):
        raise ValueError(f"image manifest mediaType unsupported: {image_mt!r}")

    if len(attestations) != 1:
        raise ValueError(f"want exactly one attestation-manifest, found {len(attestations)}")
    attestation = attestations[0]
    ann = attestation.get("annotations") or {}
    ref_digest = str(ann.get("vnd.docker.reference.digest") or "")
    image_digest = str(image.get("digest") or "")
    if not DIGEST_RE.fullmatch(ref_digest):
        raise ValueError("attestation vnd.docker.reference.digest missing/invalid")
    if ref_digest != image_digest:
        raise ValueError(
            "attestation vnd.docker.reference.digest does not point at linux/amd64 image manifest"
        )
    return image_digest, str(attestation.get("digest") or "")


def validate_attestation_layers(path: str, expected_manifest_digest: str) -> str:
    raw = Path(path).read_bytes()
    actual_manifest_digest = "sha256:" + hashlib.sha256(raw).hexdigest()
    if actual_manifest_digest != expected_manifest_digest:
        raise ValueError(
            f"attestation manifest digest {actual_manifest_digest!r} != index descriptor "
            f"{expected_manifest_digest!r}"
        )
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"attestation is not valid UTF-8 JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError("attestation JSON must be an object")

    media_type = str(data.get("mediaType") or "")
    if media_type and media_type not in (OCI_MANIFEST, DOCKER_MANIFEST):
        raise ValueError(f"attestation mediaType unsupported: {media_type!r}")

    layers = data.get("layers")
    if not isinstance(layers, list) or not layers:
        raise ValueError("attestation layers must be a non-empty list")

    slsa_layers = []
    for layer in layers:
        if not isinstance(layer, dict):
            raise ValueError("attestation layer must be an object")
        mt = str(layer.get("mediaType") or "")
        pred = str((layer.get("annotations") or {}).get("in-toto.io/predicate-type") or "")
        digest = str(layer.get("digest") or "")
        if mt == IN_TOTO_LAYER and pred == SLSA_PREDICATE:
            if not DIGEST_RE.fullmatch(digest):
                raise ValueError(f"SLSA provenance layer digest invalid: {digest!r}")
            slsa_layers.append(layer)
    if len(slsa_layers) != 1:
        raise ValueError(
            "attestation must include exactly one in-toto layer with "
            f"predicate-type {SLSA_PREDICATE}; found {len(slsa_layers)}"
        )
    return str(slsa_layers[0]["digest"])


def validate_statement(
    path: str,
    statement_digest: str,
    image_manifest_digest: str,
    slsa: dict[str, Any],
) -> None:
    raw = Path(path).read_bytes()
    actual_digest = "sha256:" + hashlib.sha256(raw).hexdigest()
    if actual_digest != statement_digest:
        raise ValueError(
            f"statement blob digest {actual_digest!r} != attestation layer {statement_digest!r}"
        )
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"statement is not valid UTF-8 JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError("statement JSON must be an object")
    if data.get("_type") != "https://in-toto.io/Statement/v1":
        raise ValueError("statement _type must be https://in-toto.io/Statement/v1")
    if data.get("predicateType") != SLSA_PREDICATE:
        raise ValueError(f"statement predicateType must be {SLSA_PREDICATE}")
    predicate = data.get("predicate")
    if not isinstance(predicate, dict) or predicate != slsa:
        raise ValueError("statement predicate does not exactly match imagetools SLSA provenance")

    subjects = data.get("subject")
    if not isinstance(subjects, list) or not subjects:
        raise ValueError("statement subject must be a non-empty list")
    expected_hex = image_manifest_digest.removeprefix("sha256:")
    for subject in subjects:
        if not isinstance(subject, dict) or not isinstance(subject.get("name"), str) or not subject["name"]:
            raise ValueError("each statement subject must have a non-empty name")
        digest = subject.get("digest")
        if not isinstance(digest, dict) or digest.get("sha256") != expected_hex:
            raise ValueError("every statement subject sha256 must bind the linux/amd64 image manifest")


def validate_slsa_metadata(args: argparse.Namespace, slsa: dict[str, Any]) -> re.Match[str]:
    build = slsa.get("buildDefinition") or {}
    if not isinstance(build, dict):
        raise ValueError("SLSA.buildDefinition must be an object")
    internal = build.get("internalParameters") or {}
    if not isinstance(internal, dict):
        raise ValueError("SLSA internalParameters must be an object")
    root = (((build.get("externalParameters") or {}).get("request") or {}).get("root") or {})
    if not isinstance(root, dict):
        raise ValueError("SLSA externalParameters.request.root must be an object")
    vcs = ((root.get("request") or {}).get("args")) or {}
    if not isinstance(vcs, dict):
        raise ValueError("SLSA root.request.args must be an object")
    details = slsa.get("runDetails") or {}
    if not isinstance(details, dict):
        raise ValueError("SLSA.runDetails must be an object")
    builder = ((details.get("builder") or {}).get("id")) or ""

    workflow_ref = f"{args.repository}/{args.workflow_path}@{args.ref}"
    expected = {
        "github_repository": args.repository,
        "github_workflow": args.workflow_name,
        "github_workflow_ref": workflow_ref,
        "github_workflow_sha": args.revision,
        "github_job": args.job,
        "github_ref": args.ref,
    }
    errors = [
        f"{key}={internal.get(key)!r} want {value!r}"
        for key, value in expected.items()
        if internal.get(key) != value
    ]
    if internal.get("github_event_name") not in ("push", "workflow_dispatch"):
        errors.append("github_event_name is not push/workflow_dispatch")
    if vcs.get("vcs:revision") != args.revision:
        errors.append("vcs:revision does not match revision")
    if vcs.get("vcs:source") != f"https://github.com/{args.repository}":
        errors.append("vcs:source does not match repository")

    builder_pattern = re.compile(
        re.escape(f"{args.server_url}/{args.repository}/actions/runs/")
        + r"([0-9]+)/attempts/([0-9]+)"
    )
    match = builder_pattern.fullmatch(str(builder))
    if not match:
        errors.append("builder id does not bind a GitHub Actions run/attempt")
    if errors:
        raise ValueError("; ".join(errors))
    return match


def main() -> int:
    args = parse_args()
    if not SHA_RE.fullmatch(args.revision):
        raise ValueError("revision must be a lowercase 40-character SHA")
    if not REPO_RE.fullmatch(args.repository):
        raise ValueError("repository must be owner/name")
    if not args.workflow_path.startswith(".github/workflows/"):
        raise ValueError("workflow path must be under .github/workflows")
    if not DIGEST_RE.fullmatch(args.image_digest):
        raise ValueError("image-digest must be sha256 plus 64 lowercase hex chars")

    provenance = load_json(args.provenance)
    if not isinstance(provenance, dict):
        raise ValueError("provenance JSON must be an object")
    slsa = extract_slsa(provenance)
    match = validate_slsa_metadata(args, slsa)

    index = resolve_index(args.manifest, args.image_digest)
    image_manifest_digest, attestation_manifest_digest = bind_index(index)

    statement_digest = validate_attestation_layers(
        args.attestation,
        attestation_manifest_digest,
    )
    validate_statement(
        args.statement,
        statement_digest,
        image_manifest_digest,
        slsa,
    )

    json.dump(
        {
            "status": "verified",
            "verification_kind": "structural-not-signature",
            "publisher_run_id": match.group(1),
            "publisher_run_attempt": match.group(2),
            "repository": args.repository,
            "revision": args.revision,
            "workflow_name": args.workflow_name,
            "workflow_path": args.workflow_path,
            "job": args.job,
            "ref": args.ref,
            "image_digest": args.image_digest,
            "image_manifest_digest": image_manifest_digest,
            "attestation_manifest_digest": attestation_manifest_digest,
            "attestation_manifest_content_checked": True,
            "attestation_layer_shape_checked": True,
            "statement_digest": statement_digest,
            "statement_subject_checked": True,
        },
        sys.stdout,
        sort_keys=True,
    )
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, json.JSONDecodeError, TypeError, KeyError) as exc:
        print(f"provenance verification failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
