#!/usr/bin/env python3
"""Exercise image evidence across primary CI and deleted standby repositories."""

import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch
import zipfile

DEPLOY = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(DEPLOY))
import ci_evidence as evidence

spec = importlib.util.spec_from_file_location("ci_image", DEPLOY / "verify-ci-image.py")
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)
REPO = "owner/sub2api"
SHA = "a" * 40
DIGEST = "sha256:" + "b" * 64


class Fixture:
    def __init__(self, backup=False):
        self.source = {"repository": REPO, "repository_id": 31, "run_id": 123,
                       "run_attempt": 1, "sha": SHA, "ref": image.REF,
                       "event": "push", "workflow_path": image.WORKFLOW}
        self.builder = dict(self.source)
        if backup:
            self.builder.update(repository="standby/ci-sub2api", repository_id=32,
                                run_id=456, event="workflow_dispatch",
                                ref="refs/heads/ci-fallback/run-123-attempt-1")
        self.meta = {"schema_version": 1, "source_run_id": "123", "revision": SHA,
                     "repository": REPO, "image": "ghcr.io/" + REPO,
                     "digest": DIGEST, "immutable_tag": "debug-sha-" + SHA,
                     "publisher_run_id": str(self.builder["run_id"]),
                     "publisher_run_attempt": "1", "reused_existing": False}
        if backup:
            self.meta.update(publisher_repository=self.builder["repository"],
                             publisher_ref=self.builder["ref"], publisher_source_run_id="123")
        self.run = {"id": 123, "head_sha": SHA, "head_branch": "debug", "path": image.WORKFLOW,
                    "status": "completed", "conclusion": "failure" if backup else "success",
                    "event": "push", "run_attempt": 1,
                    "repository": {"id": 31}, "head_repository": {"id": 31}}
        self.jobs = [{"name": "publish", "conclusion": self.run["conclusion"]}]
        if backup:
            self.jobs[0].update(runner_id=0, steps=[],
                               check_run_url=f"https://api.github.com/repos/{REPO}/check-runs/5")
        self.receipt = {"schema_version": 1, "kind": "ci-fallback-receipt",
                        "source": self.source, "builder": self.builder,
                        "verified_jobs": [{"name": "publish", "conclusion": "success"}],
                        "allowed_skipped_jobs": [], "artifacts": []}
        self.release = {"id": 81, "draft": True, "target_commitish": SHA,
                        "author": {"id": 11}, "tag_name": "ci-fallback-123",
                        "body": "Verified standby run: standby/ci-sub2api#456"}
        self.github = Mock(api=self.api, pages=self.pages, download=self.download)
        self.seal()

    def seal(self):
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as out:
            out.writestr("debug-image.json", json.dumps(self.meta))
        self.data = stream.getvalue()
        self.digest = evidence.digest(self.data)
        self.receipt["artifacts"] = [{"name": "debug-image-metadata", "id": 61,
                                     "filename": "debug-image-metadata.zip",
                                     "digest": self.digest, "size": len(self.data)}]

    def api(self, path):
        if path == f"repos/{REPO}":
            return {"id": 31, "owner": {"id": 11}}
        if path in (f"repos/{REPO}/actions/runs/123", f"repos/{REPO}/actions/runs/123/attempts/1"):
            return self.run
        raise AssertionError("unexpected API access: " + path)

    def pages(self, path, key=None):
        if path.endswith("/jobs"):
            return self.jobs
        if path.endswith("/annotations"):
            return [{"message": evidence.BILLING_PREFIX}]
        if path.endswith("/artifacts"):
            return [{"id": 61, "name": "debug-image-metadata", "expired": False, "digest": self.digest}]
        if path.endswith("/releases"):
            return [self.release]
        if path.endswith("/assets"):
            raw = json.dumps(self.receipt).encode()
            return [{"id": 71, "name": "debug-image-metadata.zip", "digest": self.digest,
                     "size": len(self.data)}, {"id": 72, "name": "ci-fallback-receipt.json",
                     "digest": evidence.digest(raw), "size": len(raw)}]
        raise AssertionError("unexpected paginated access: " + path)

    def download(self, path, *, binary=False):
        if path.endswith("/72"):
            return json.dumps(self.receipt).encode()
        if path.endswith("/61/zip") or path.endswith("/71"):
            return self.data
        raise AssertionError("unexpected download: " + path)

    def read(self):
        return image.debug_evidence(self.github, REPO, 123, SHA, DIGEST)

    def provenance(self):
        repo, ref = self.builder["repository"], self.builder["ref"]
        return {"SLSA": {"buildDefinition": {"internalParameters": {
            "github_repository": repo, "github_ref": ref, "github_workflow_sha": SHA,
            "github_workflow_ref": f"{repo}/{image.WORKFLOW}@{ref}", "github_job": "publish"}},
            "runDetails": {"builder": {"id": f"https://github.com/{repo}/actions/runs/"
                                      f"{self.builder['run_id']}/attempts/1"}}}}


class ImageEvidenceTests(unittest.TestCase):
    def test_vendored_consumer_matches_recorded_source(self):
        source = json.loads((DEPLOY / "ci-evidence-source.json").read_text())
        self.assertEqual(hashlib.sha256((DEPLOY / "ci_evidence.py").read_bytes()).hexdigest(),
                         source["sha256"])

    def test_primary_and_standby_keep_distinct_source_and_publisher(self):
        for backup in (False, True):
            with self.subTest(backup=backup):
                f = Fixture(backup)
                result = f.read()
                self.assertEqual(result["source"]["run_id"], 123)
                self.assertEqual(result["source"]["conclusion"], "failure" if backup else "success")
                self.assertEqual(result["publisher"]["run_id"], "456" if backup else "123")
                self.assertEqual(result["publisher"]["repository"], f.builder["repository"])

    def test_substituted_metadata_is_rejected_on_both_routes(self):
        for backup in (False, True):
            for change in ({"source_run_id": "999"}, {"revision": "c" * 40},
                           {"digest": "sha256:" + "c" * 64}, {"immutable_tag": "debug"},
                           {"image": "ghcr.io/other/sub2api"}, {"repository": "other/sub2api"},
                           {"publisher_run_id": "999"}, {"publisher_run_attempt": "2"},
                           {"publisher_repository": "other/sub2api"}, {"reused_existing": "false"}):
                with self.subTest(backup=backup, change=change):
                    f = Fixture(backup)
                    f.meta.update(change)
                    f.seal()
                    with self.assertRaises(evidence.EvidenceError):
                        f.read()

    def test_archive_tamper_is_rejected_before_metadata(self):
        f = Fixture(True)
        f.data = f.data[:-1] + b"x"
        with self.assertRaisesRegex(evidence.EvidenceError, "digest mismatch"):
            f.read()

    def test_standby_receipt_cannot_hide_test_failure_or_skipped_publish(self):
        for runner, skipped in ((7, False), (0, True)):
            f = Fixture(True)
            f.jobs[0]["runner_id"] = runner
            if skipped:
                f.receipt["verified_jobs"][0]["conclusion"] = "skipped"
                f.receipt["allowed_skipped_jobs"] = ["publish"]
            with self.assertRaises(evidence.EvidenceError):
                f.read()

    def test_rerun_invalidates_an_older_standby_receipt(self):
        f = Fixture(True)
        f.run["run_attempt"] = 2
        with self.assertRaisesRegex(evidence.EvidenceError, "source mismatch"):
            f.read()

    def test_existing_image_publisher_is_bound_to_original_ci(self):
        for backup in (False, True):
            with self.subTest(backup=backup):
                f = Fixture(backup)
                result = image.resolve_publisher(f.github, REPO, SHA, DIGEST, f.provenance())
                self.assertEqual(result["source_run_id"], "123")
                self.assertEqual(result["run_id"], "456" if backup else "123")

    def test_provenance_hints_cannot_substitute_publisher(self):
        f = Fixture(True)
        for field, value in (("github_workflow_sha", "c" * 40), ("github_job", "build"),
                             ("github_ref", "refs/heads/debug")):
            provenance = copy.deepcopy(f.provenance())
            provenance["SLSA"]["buildDefinition"]["internalParameters"][field] = value
            with self.assertRaises(evidence.EvidenceError):
                image.resolve_publisher(f.github, REPO, SHA, DIGEST, provenance)


class DownloadContractTests(unittest.TestCase):
    def test_accept_header_matches_artifact_endpoint(self):
        cases = [("repos/owner/app/actions/artifacts/61/zip", True, False),
                 ("repos/owner/app/releases/assets/71", True, True),
                 ("repos/owner/app/actions/runs/123", False, False)]
        for path, binary, octet in cases:
            with self.subTest(path=path):
                process = Mock(stdout=io.BytesIO(b"payload"))
                process.wait.return_value = 0
                process.poll.return_value = 0
                with patch.object(evidence.subprocess, "Popen", return_value=process) as popen:
                    self.assertEqual(evidence.GitHub().download(path, binary=binary), b"payload")
                self.assertEqual("Accept: application/octet-stream" in popen.call_args.args[0], octet)


if __name__ == "__main__":
    unittest.main()
