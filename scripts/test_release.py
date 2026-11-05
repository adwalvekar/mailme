import contextlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

import release


@contextlib.contextmanager
def working_directory(path):
    previous = Path.cwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(previous)


class VersionTests(unittest.TestCase):
    def test_semver_and_optional_v(self):
        for tag in ["v0.0.0", "1.2.3", "v1.2.3-rc.1", "v1.2.3-alpha-1+000.sha-abc", "1.2.3-0"]:
            with self.subTest(tag=tag):
                self.assertEqual(release.validate_tag(tag), tag)
        for tag in ["latest", "v1.2", "v01.2.3", "1.2.3-01", "1.2.3-alpha.01", "1.2.3+", "1.2.3+build..id", "v1.2.3\n", "1.2.3;echo", "../1.2.3", "1.2.٣"]:
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release.validate_tag(tag)


class SourceTests(unittest.TestCase):
    def test_main_ancestry_annotated_tag_and_moved_tag(self):
        with tempfile.TemporaryDirectory() as directory, working_directory(directory):
            def git(*args):
                return subprocess.check_output(["git", *args], text=True, stderr=subprocess.DEVNULL).strip()
            git("init", "-b", "main")
            git("config", "user.name", "Release Test")
            git("config", "user.email", "release-test@example.invalid")
            git("config", "commit.gpgsign", "false")
            git("config", "tag.gpgsign", "false")
            git("commit", "--allow-empty", "-m", "base")
            base = git("rev-parse", "HEAD")
            git("update-ref", "refs/remotes/origin/main", base)
            git("tag", "-a", "v1.0.0", "-m", "release")
            self.assertEqual(release.validate_source("v1.0.0", base), base)
            git("checkout", "-b", "feature")
            git("commit", "--allow-empty", "-m", "unmerged feature")
            git("tag", "v1.1.0")
            with self.assertRaisesRegex(ValueError, "belong to main"):
                release.validate_source("v1.1.0")
            git("tag", "-f", "v1.0.0")
            with self.assertRaisesRegex(ValueError, "moved"):
                release.validate_source("v1.0.0", base)
            git("checkout", "main")
            git("commit", "--allow-empty", "-m", "later main commit")
            later = git("rev-parse", "HEAD")
            git("update-ref", "refs/remotes/origin/main", later)
            git("checkout", base)
            git("tag", "v1.0.1")
            self.assertEqual(release.validate_source("v1.0.1", base), base)
            git("checkout", later)
            with self.assertRaisesRegex(ValueError, "checkout"):
                release.validate_source("v1.0.1", base)


class ArchiveTests(unittest.TestCase):
    def test_archive_is_reproducible_and_executable(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "mailme_v1.2.3_darwin_arm64"
            root.mkdir()
            (root / "mailme").write_bytes(b"synthetic binary")
            (root / "README.md").write_text("instructions", encoding="utf-8")
            first, second = Path(directory) / "first.tar.gz", Path(directory) / "second.tar.gz"
            release.write_archive(root, first, 1700000000)
            (root / "README.md").chmod(0o600)
            os.utime(root / "mailme", (1800000000, 1800000000))
            release.write_archive(root, second, 1700000000)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with tarfile.open(first) as archive:
                binary = archive.getmember(root.name + "/mailme")
                self.assertEqual(binary.mode, 0o755)
                self.assertEqual(binary.mtime, 1700000000)
                self.assertEqual(binary.uid, 0)
                self.assertEqual(archive.extractfile(binary).read(), b"synthetic binary")


class PublicationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name).resolve()
        self.tag = "v1.2.3"
        self.archive = self.directory / release.archive_name(self.tag)
        self.archive.write_bytes(b"tested package")
        self.checksums = self.directory / "SHA256SUMS"
        self.checksums.write_text(f"{release.digest(self.archive)}  {self.archive.name}\n", encoding="utf-8")
        self.release = {"tag_name": self.tag, "draft": False, "assets": []}

    def upload(self):
        release.publish(self.tag, "42", "owner/mailme", self.directory)

    @mock.patch.object(release.subprocess, "run")
    @mock.patch.object(release, "command")
    def test_upload_and_resume_without_overwriting(self, command, run):
        command.side_effect = lambda *args: json.dumps(self.release)
        self.upload()
        args = run.call_args.args[0]
        self.assertEqual(args[:4], ["gh", "release", "upload", self.tag])
        self.assertNotIn("--clobber", args)
        self.release["assets"] = [{"name": self.archive.name, "state": "uploaded", "digest": "sha256:" + release.digest(self.archive)}]
        run.reset_mock()
        self.upload()
        self.assertNotIn(str(self.archive), run.call_args.args[0])
        self.assertIn(str(self.checksums), run.call_args.args[0])
        self.release["assets"].append({"name": self.checksums.name, "state": "uploaded", "digest": "sha256:" + release.digest(self.checksums)})
        run.reset_mock()
        self.upload()
        run.assert_not_called()

    @mock.patch.object(release.subprocess, "run")
    @mock.patch.object(release, "command")
    def test_refuses_changed_assets_or_corrupted_checksums(self, command, run):
        command.side_effect = lambda *args: json.dumps(self.release)
        self.release["assets"] = [{"name": "SHA256SUMS", "state": "uploaded", "digest": "sha256:incorrect"}]
        with self.assertRaisesRegex(ValueError, "refusing to overwrite"):
            self.upload()
        run.assert_not_called()
        self.archive.write_bytes(b"modified after testing")
        with self.assertRaisesRegex(ValueError, "checksum verification"):
            self.upload()

    @mock.patch.object(release, "command")
    def test_refuses_unpublished_or_retargeted_releases(self, command):
        for change in [{"draft": True}, {"tag_name": "v2.0.0"}, {"immutable": True}]:
            command.return_value = json.dumps(dict(self.release, **change))
            with self.assertRaises(ValueError):
                self.upload()


if __name__ == "__main__":
    unittest.main()
