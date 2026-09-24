#!/usr/bin/env python3
"""Bind Sub2API image metadata to verified primary/standby CI evidence."""

import argparse
import json
import re
import sys
from pathlib import Path

from ci_evidence import EvidenceError, GitHub, check_jobs, read_artifact, require


WORKFLOW = ".github/workflows/docker-branch.yml"
REF = "refs/heads/debug"


def publisher_metadata(meta, repository):
    return {
        "repository": meta.get("publisher_repository", repository),
        "ref": meta.get("publisher_ref", REF),
        "run_id": str(meta.get("publisher_run_id", "")),
        "run_attempt": str(meta.get("publisher_run_attempt", "")),
    }


def validate_debug(result, repository, run_id, sha, image_digest=None):
    meta = result["artifact"]
    require(meta.get("schema_version") == 1, "Unsupported debug image metadata")
    require(str(meta.get("source_run_id")) == str(run_id) and meta.get("revision") == sha,
            "Debug metadata source run/revision mismatch")
    require(meta.get("repository") == repository and
            meta.get("image") == "ghcr.io/" + repository.lower() and
            meta.get("immutable_tag") == "debug-sha-" + sha, "Debug image/tag/repository mismatch")
    require(isinstance(meta.get("digest"), str) and
            re.fullmatch(r"sha256:[0-9a-f]{64}", meta["digest"]), "Invalid debug image digest")
    require(image_digest is None or meta["digest"] == image_digest, "Debug image digest mismatch")
    require(isinstance(meta.get("reused_existing"), bool), "Missing image reuse decision")
    publisher = publisher_metadata(meta, repository)
    require(all(re.fullmatch(r"[1-9][0-9]*", publisher[key]) for key in ("run_id", "run_attempt")),
            "Invalid publisher run/attempt")
    builder = result["builder"]
    if not meta["reused_existing"]:
        require(all(str(builder[key]) == publisher[key] for key in publisher),
                "Fresh image publisher differs from verified CI builder")
    else:
        # Reuse is attested by the successful publish job, which independently
        # checks the original publisher before reissuing this metadata.
        require(publisher["repository"] == repository and publisher["ref"] == REF or
                re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", publisher["repository"]) and
                publisher["repository"] != repository and
                re.fullmatch(r"refs/heads/ci-fallback/run-[1-9][0-9]*-attempt-[1-9][0-9]*", publisher["ref"]),
                "Invalid reused publisher repository/ref")
    result["publisher"] = publisher
    return result


def debug_evidence(github, repository, run_id, sha, image_digest=None):
    result = read_artifact(github, repository, run_id, WORKFLOW, REF, sha,
                           "debug-image-metadata", "debug-image.json", ["publish"])
    return validate_debug(result, repository, run_id, sha, image_digest)


def resolve_publisher(github, repository, sha, image_digest, provenance):
    # SLSA fields are discovery hints until independently bound to GitHub CI.
    slsa = provenance.get("SLSA") or (provenance.get("linux/amd64") or {}).get("SLSA")
    require(isinstance(slsa, dict), "Missing SLSA publisher hints")
    internal = slsa["buildDefinition"]["internalParameters"]
    builder_repo, builder_ref = internal["github_repository"], internal["github_ref"]
    url = slsa["runDetails"]["builder"]["id"]
    match = re.fullmatch(re.escape(f"https://github.com/{builder_repo}/actions/runs/") +
                         r"([1-9][0-9]*)/attempts/([1-9][0-9]*)", url)
    require(match is not None, "Invalid provenance builder URL")
    run_id, attempt = map(int, match.groups())
    require(internal.get("github_workflow_sha") == sha and
            internal.get("github_workflow_ref") == f"{builder_repo}/{WORKFLOW}@{builder_ref}" and
            internal.get("github_job") == "publish", "Publisher workflow identity mismatch")
    publisher = {"repository": builder_repo, "ref": builder_ref,
                 "run_id": str(run_id), "run_attempt": str(attempt)}
    if builder_repo == repository:
        require(builder_ref == REF, "Primary publisher must be debug")
        metadata = github.api(f"repos/{repository}")
        run = github.api(f"repos/{repository}/actions/runs/{run_id}/attempts/{attempt}")
        expected = {"id": run_id, "run_attempt": attempt, "status": "completed", "conclusion": "success",
                    "head_sha": sha, "head_branch": "debug", "path": WORKFLOW}
        require(all(run.get(k) == v for k, v in expected.items()) and
                run.get("event") in ("push", "workflow_dispatch") and
                run.get("repository", {}).get("id") == metadata["id"] and
                run.get("head_repository", {}).get("id") == metadata["id"],
                "Original primary publisher did not successfully publish this revision")
        check_jobs(github.pages(f"repos/{repository}/actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs"),
                   ["publish"])
        publisher["source_run_id"] = str(run_id)
    else:
        source = re.fullmatch(r"refs/heads/ci-fallback/run-([1-9][0-9]*)-attempt-([1-9][0-9]*)", builder_ref)
        require(source is not None, "Unknown standby publisher ref")
        result = debug_evidence(github, repository, int(source[1]), sha, image_digest)
        require(result["route"] == "backup" and result["source"]["run_attempt"] == int(source[2]) and
                all(str(result["builder"][k]) == v for k, v in publisher.items()) and
                result["publisher"] == publisher,
                "Original standby publisher does not match its preserved receipt")
        publisher["source_run_id"] = source[1]
    return publisher


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("debug", "publisher"))
    parser.add_argument("--repo", required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--digest")
    parser.add_argument("--run-id", type=int)
    parser.add_argument("--provenance")
    args = parser.parse_args()
    try:
        require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo) and
                re.fullmatch(r"[0-9a-f]{40}", args.sha), "Invalid primary identity")
        github = GitHub()
        if args.mode == "debug":
            require(args.run_id is not None, "A source run ID is required")
            result = debug_evidence(github, args.repo, args.run_id, args.sha, args.digest)
        else:
            require(args.digest and args.provenance, "Publisher requires digest and provenance")
            result = resolve_publisher(github, args.repo, args.sha, args.digest,
                                       json.loads(Path(args.provenance).read_text()))
        print(json.dumps(result, sort_keys=True))
        return 0
    except (EvidenceError, KeyError, TypeError, ValueError, OSError) as exc:
        print("CI image verification failed: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
