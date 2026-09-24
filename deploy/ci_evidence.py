#!/usr/bin/env python3
"""Read an exact JSON artifact from primary CI or its preserved standby receipt.

This stdlib-only consumer never dispatches, publishes, retags, or deploys. Cloud
consumers may vendor this file; the installed gci command uses the same source.
"""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import re
import stat
import subprocess
import sys
import threading
import zipfile


REPO = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\Z")
SHA = re.compile(r"[0-9a-f]{40}\Z")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
MAX_DOWNLOAD = 4 * 1024 * 1024
MAX_DOCUMENT = 1024 * 1024
BILLING_PREFIX = (
    "The job was not started because recent account payments have failed "
    "or your spending limit needs to be increased."
)


class EvidenceError(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise EvidenceError(message)


def positive(value):
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def document(data):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "Duplicate JSON field")
            result[key] = value
        return result
    result = json.loads(data, object_pairs_hook=unique)
    require(isinstance(result, dict), "Expected a JSON object")
    return result


class GitHub:
    """Use the current primary read credentials, without exposing their value."""

    def download(self, path, *, binary=False):
        require(path.startswith("repos/") and ".." not in path, "Invalid API path")
        args = ["/usr/bin/gh", "api", "--hostname", "github.com", path]
        # Actions archives are JSON API redirect endpoints; octet-stream returns
        # HTTP 415 there. Release assets require octet-stream to return bytes.
        action_archive = re.fullmatch(r"repos/[^/]+/[^/]+/actions/artifacts/[0-9]+/zip", path)
        if binary and not action_archive:
            args += ["-H", "Accept: application/octet-stream"]
        process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   env={**os.environ, "GH_PROMPT_DISABLED": "1"})
        timer = threading.Timer(90, process.kill)
        timer.start()
        try:
            data = process.stdout.read(MAX_DOWNLOAD + 1)
            require(len(data) <= MAX_DOWNLOAD, "Evidence download exceeds 4 MiB")
            require(process.wait(timeout=10) == 0, "GitHub evidence request failed: " + path.split("?")[0])
            return data
        finally:
            timer.cancel()
            if process.poll() is None:
                process.kill()
                process.wait()
            process.stdout.close()

    def api(self, path):
        return json.loads(self.download(path))

    def pages(self, path, key=None):
        rows = []
        for page in range(1, 101):
            value = self.api(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
            batch = value[key] if key else value
            require(isinstance(batch, list), "Invalid paginated evidence response")
            rows.extend(batch)
            if len(batch) < 100:
                return rows
        raise EvidenceError("Evidence list exceeds pagination limit")


def billing_blocked(github, repository, run):
    if run.get("status") != "completed" or run.get("conclusion") != "failure":
        return False
    attempt = int(run.get("run_attempt", 1))
    jobs = github.pages(
        f"repos/{repository}/actions/runs/{run['id']}/attempts/{attempt}/jobs", "jobs"
    )
    failed = [job for job in jobs if job.get("conclusion") == "failure"]
    if not failed or any(job.get("conclusion") not in ("failure", "skipped") for job in jobs):
        return False
    for job in failed:
        if job.get("runner_id") or job.get("runner_name") or job.get("steps"):
            return False
        expected = f"https://api.github.com/repos/{repository}/check-runs/"
        check = job.get("check_run_url", "")
        if not check.startswith(expected) or not check[len(expected):].isdigit():
            return False
        annotations = github.pages(check.removeprefix("https://api.github.com/") + "/annotations")
        if not any(str(item.get("message", "")).startswith(BILLING_PREFIX) for item in annotations):
            return False
    return True


def check_jobs(jobs, required_jobs, allowed=()):
    require(isinstance(jobs, list) and jobs, "Missing verified jobs")
    names = [job["name"] for job in jobs]
    require(len(names) == len(set(names)), "Ambiguous verified job names")
    require(all(job.get("conclusion") == "success" or (
        job.get("conclusion") == "skipped" and job["name"] in allowed) for job in jobs),
        "A required CI job did not succeed")
    require(all(any(job["name"] == name and job.get("conclusion") == "success" for job in jobs)
                for name in required_jobs), "A required CI job is missing or skipped")


def unpack(data, filename):
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        files = [entry for entry in archive.infolist() if not entry.is_dir()]
        require(len(files) == 1 and files[0].filename == filename,
                "Artifact must contain exactly the requested JSON file")
        entry = files[0]
        require(not stat.S_ISLNK(entry.external_attr >> 16) and not (entry.flag_bits & 1),
                "Artifact file cannot be a symlink or encrypted")
        require(0 < entry.file_size <= MAX_DOCUMENT, "Artifact JSON exceeds size limit")
        return document(archive.read(entry))


def checked_download(github, path, expected, size=None):
    require(isinstance(expected, str) and DIGEST.fullmatch(expected), "Missing artifact SHA-256")
    if size is not None:
        require(positive(size) and size <= MAX_DOWNLOAD, "Evidence asset exceeds size limit")
    data = github.download(path, binary=True)
    require(digest(data) == expected, "Evidence asset digest mismatch")
    if size is not None:
        require(len(data) == size, "Evidence asset size mismatch")
    return data


def read_artifact(github, repository, run_id, workflow, ref, sha, artifact, filename, required_jobs=()):
    require(REPO.fullmatch(repository) and positive(run_id) and SHA.fullmatch(sha), "Invalid source identity")
    require(re.fullmatch(r"\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml", workflow), "Invalid workflow path")
    require(ref.startswith("refs/heads/") and len(ref) > 11, "Expected an explicit source branch")
    require(re.fullmatch(r"[A-Za-z0-9_.-]+\.json", filename), "Expected a single JSON filename")
    metadata = github.api(f"repos/{repository}")
    run = github.api(f"repos/{repository}/actions/runs/{run_id}")
    expected = {"id": run_id, "head_sha": sha, "head_branch": ref.split("/", 2)[2],
                "path": workflow, "status": "completed"}
    require(all(run.get(key) == value for key, value in expected.items()), "Source run identity/status mismatch")
    require(run.get("repository", {}).get("id") == metadata["id"] and
            run.get("head_repository", {}).get("id") == metadata["id"], "Source repository identity changed")
    require(run.get("event") in ("push", "workflow_dispatch"), "Unsupported source event")
    require(positive(run.get("run_attempt")) and positive(metadata["id"]), "Missing source run attempt or repository ID")
    source = {"repository": repository, "repository_id": metadata["id"], "run_id": run_id,
              "run_attempt": run["run_attempt"], "sha": sha, "ref": ref, "event": run["event"],
              "workflow_path": workflow}
    route = "primary"
    receipt_digest = None
    inputs_digest = None
    if run.get("conclusion") == "success":
        jobs = github.pages(f"repos/{repository}/actions/runs/{run_id}/attempts/{run['run_attempt']}/jobs", "jobs")
        # Sub2API has no intentional skipped job on these two workflows.
        check_jobs(jobs, required_jobs)
        builder = dict(source)
        artifacts = github.pages(f"repos/{repository}/actions/runs/{run_id}/artifacts", "artifacts")
        matches = [item for item in artifacts if item.get("name") == artifact]
        require(len(matches) == 1 and not matches[0].get("expired"), "Missing, expired or duplicate CI artifact")
        item = matches[0]
        require(positive(item["id"]), "Invalid artifact ID")
        data = checked_download(github, f"repos/{repository}/actions/artifacts/{item['id']}/zip", item.get("digest"))
        artifact_digest = item["digest"]
    else:
        require(billing_blocked(github, repository, run), "Primary failure is not a pure billing rejection")
        route = "backup"
        matches = [item for item in github.pages(f"repos/{repository}/releases")
                   if item.get("tag_name") == f"ci-fallback-{run_id}"]
        require(len(matches) == 1, "Exactly one preserved standby release is required")
        release = matches[0]
        require(release.get("draft") is True and release.get("target_commitish") == sha and
                release.get("author", {}).get("id") == metadata["owner"]["id"],
                "Preserved release identity, owner or revision changed")
        assets = github.pages(f"repos/{repository}/releases/{release['id']}/assets")
        receipts = [item for item in assets if item.get("name") == "ci-fallback-receipt.json"]
        require(len(receipts) == 1, "Missing or ambiguous standby receipt")
        receipt_asset = receipts[0]
        raw = checked_download(github, f"repos/{repository}/releases/assets/{receipt_asset['id']}",
                               receipt_asset.get("digest"), receipt_asset.get("size"))
        receipt_digest = digest(raw)
        receipt = document(raw)
        require(receipt.get("schema_version") == 1 and receipt.get("kind") == "ci-fallback-receipt",
                "Unsupported standby receipt")
        recorded = receipt["source"]
        require(all(recorded.get(key) == value for key, value in source.items()), "Standby receipt source mismatch")
        inputs_digest = recorded.get("dispatch_inputs_sha256")
        require(inputs_digest is None or (isinstance(inputs_digest, str) and
                re.fullmatch(r"[0-9a-f]{64}", inputs_digest)), "Invalid dispatch input digest")
        builder = receipt["builder"]
        require(REPO.fullmatch(builder.get("repository", "")) and builder["repository"] != repository and
                positive(builder.get("repository_id")) and builder["repository_id"] != metadata["id"] and
                positive(builder.get("run_id")) and positive(builder.get("run_attempt")), "Invalid standby builder identity")
        require(builder.get("sha") == sha and builder.get("workflow_path") == workflow and
                builder.get("event") == "workflow_dispatch" and builder.get("ref") ==
                f"refs/heads/ci-fallback/run-{run_id}-attempt-{run['run_attempt']}", "Standby builder revision/ref mismatch")
        require(f"Verified standby run: {builder['repository']}#{builder['run_id']}" in release.get("body", ""),
                "Preserved release does not identify this builder")
        check_jobs(receipt["verified_jobs"], required_jobs, receipt.get("allowed_skipped_jobs", []))
        preserved = receipt["artifacts"]
        require(isinstance(preserved, list) and len(assets) == len(preserved) + 1,
                "Preserved artifact set changed")
        names = [item["filename"] for item in preserved]
        require(len(names) == len(set(names)) and "ci-fallback-receipt.json" not in names,
                "Ambiguous preserved filenames")
        for item in preserved:
            actual = [asset for asset in assets if asset.get("name") == item["filename"]]
            require(len(actual) == 1 and all(actual[0].get(key) == item.get(key) for key in ("digest", "size")),
                    "Preserved asset inventory mismatch")
        matches = [item for item in preserved if item.get("name") == artifact]
        require(len(matches) == 1, "Missing or ambiguous preserved artifact")
        item = matches[0]
        asset = next(asset for asset in assets if asset["name"] == item["filename"])
        data = checked_download(github, f"repos/{repository}/releases/assets/{asset['id']}", item["digest"], item["size"])
        artifact_digest = item["digest"]
    return {"route": route, "source": {**source, "conclusion": run["conclusion"]}, "builder": builder,
            "artifact": unpack(data, filename), "artifact_digest": artifact_digest,
            "receipt_digest": receipt_digest, "dispatch_inputs_sha256": inputs_digest}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--run-id", required=True, type=int)
    parser.add_argument("--workflow", required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--artifact", required=True)
    parser.add_argument("--file", required=True)
    parser.add_argument("--require-job", action="append", default=[])
    args = parser.parse_args(argv)
    try:
        result = read_artifact(GitHub(), args.repo, args.run_id, args.workflow, args.ref,
                               args.sha, args.artifact, args.file, args.require_job)
        print(json.dumps(result, sort_keys=True))
        return 0
    except (EvidenceError, OSError, ValueError, KeyError, TypeError, zipfile.BadZipFile,
            subprocess.TimeoutExpired) as exc:
        print("CI evidence verification failed: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
