"""Validate, package, and publish a manually created GitHub SemVer release.

Uses only Python's standard library plus git, Go, and (for publication) gh.
Packaging never creates a tag or release and never changes the user's notes.
"""

import argparse
import datetime as dt
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


# SemVer 2.0.0, with an optional conventional v tag prefix. Numeric prerelease
# identifiers cannot contain leading zeroes; build identifiers may contain them.
NUMBER = r"(?:0|[1-9][0-9]*)"
PRERELEASE = rf"(?:{NUMBER}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
SEMVER = re.compile(
    rf"v?{NUMBER}\.{NUMBER}\.{NUMBER}"
    rf"(?:-{PRERELEASE}(?:\.{PRERELEASE})*)?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)


def validate_tag(tag):
    if not SEMVER.fullmatch(tag):
        raise ValueError("release tag must be SemVer, for example v1.2.3 or v1.2.3-rc.1")
    return tag


def command(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def validate_source(tag, expected_commit=None, main_ref="refs/remotes/origin/main"):
    validate_tag(tag)
    source = command("git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}")
    if expected_commit and source != expected_commit:
        raise ValueError("release tag moved or does not match the triggering commit")
    if command("git", "rev-parse", "HEAD") != source:
        raise ValueError("checkout does not match the release tag")
    result = subprocess.run(
        ["git", "merge-base", "--is-ancestor", source, main_ref], check=False
    )
    if result.returncode != 0:
        raise ValueError("release commit must belong to main; create the release tag with Target: main")
    return source


def archive_name(tag):
    return f"mailme_{validate_tag(tag)}_darwin_arm64.tar.gz"


def digest(path):
    checksum = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            checksum.update(chunk)
    return checksum.hexdigest()


def write_archive(source, destination, epoch):
    # Normalize ownership/timestamps and gzip headers for reproducible packages.
    def normalize(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ""
        info.mtime = epoch
        info.pax_headers = {}
        info.mode = 0o755 if info.isdir() or Path(info.name).name == "mailme" else 0o644
        return info

    with destination.open("wb") as output:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as archive:
                for path in [source, *sorted(source.rglob("*"))]:
                    archive.add(
                        path,
                        arcname=str(path.relative_to(source.parent)),
                        recursive=False,
                        filter=normalize,
                    )


def package(tag, source_commit, date, output):
    validate_tag(tag)
    if not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        raise ValueError("commit must be a full Git SHA")
    if command("git", "rev-parse", "HEAD") != source_commit:
        raise ValueError("package source does not match the requested commit")
    dirty = subprocess.run(
        ["git", "diff", "--quiet", "HEAD", "--"], check=False
    ).returncode
    if dirty or command("git", "ls-files", "--others", "--exclude-standard"):
        raise ValueError("package source must be a clean checkout of the release commit")
    when = dt.datetime.fromisoformat(date.replace("Z", "+00:00"))
    if when.tzinfo is None:
        raise ValueError("build date must include a timezone")
    when = when.astimezone(dt.timezone.utc)
    date = when.isoformat(timespec="seconds").replace("+00:00", "Z")
    epoch = int(when.timestamp())
    if epoch < 0 or epoch > 0xFFFFFFFF:
        raise ValueError("build date cannot be represented in gzip")
    output = Path(output).resolve()
    if output.exists() and any(output.iterdir()):
        raise ValueError("package output directory must be empty")
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="mailme-release-") as staging:
        root = Path(staging) / archive_name(tag).removesuffix(".tar.gz")
        root.mkdir(mode=0o755)
        binary = root / "mailme"
        env = dict(os.environ, CGO_ENABLED="0", GOOS="darwin", GOARCH="arm64")
        flags = f"-s -w -X main.version={tag} -X main.commit={source_commit} -X main.buildDate={date}"
        subprocess.run(
            ["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", flags,
             "-o", str(binary), "./cmd/mailme"],
            env=env,
            check=True,
        )
        binary.chmod(0o755)
        if platform.system() == "Darwin" and platform.machine() == "arm64":
            reported = command(str(binary), "version")
            if (f"mailme {tag} " not in reported
                    or source_commit not in reported or date not in reported):
                raise ValueError("packaged binary did not report the expected version metadata")
        for name in ["LICENSE", "README.md"]:
            shutil.copyfile(name, root / name)
            (root / name).chmod(0o644)
        (root / "contrib").mkdir(mode=0o755)
        plist = root / "contrib" / "com.mailme.bridge.plist.example"
        shutil.copyfile(Path("contrib") / plist.name, plist)
        plist.chmod(0o644)
        metadata = {
            "version": tag,
            "commit": source_commit,
            "date": date,
            "go_version": command("go", "version"),
            "os": "darwin",
            "arch": "arm64",
        }
        (root / "BUILD.json").write_text(
            json.dumps(metadata, indent=2) + "\n", encoding="utf-8"
        )
        (root / "BUILD.json").chmod(0o644)
        archive = output / archive_name(tag)
        write_archive(root, archive, epoch)
        (output / "SHA256SUMS").write_text(
            f"{digest(archive)}  {archive.name}\n", encoding="utf-8"
        )


def publish(tag, release_id, repo, output):
    validate_tag(tag)
    if (not str(release_id).isdigit()
            or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo)):
        raise ValueError("invalid GitHub release ID or repository")
    output = Path(output).resolve()
    archive = output / archive_name(tag)
    checksums = output / "SHA256SUMS"
    expected = f"{digest(archive)}  {archive.name}\n"
    if checksums.read_text(encoding="utf-8") != expected:
        raise ValueError("release archive failed checksum verification")
    release = json.loads(command("gh", "api", f"repos/{repo}/releases/{release_id}"))
    if release["tag_name"] != tag or release["draft"]:
        raise ValueError("release is no longer published with the expected tag")
    if release.get("immutable"):
        raise ValueError("published-release asset uploads require mutable releases; this release is immutable")
    existing = {asset["name"]: asset for asset in release["assets"]}
    missing = []
    # Check everything before uploading: retries may skip identical assets, but
    # published version contents must never be silently replaced with new bytes.
    for path in [archive, checksums]:
        asset = existing.get(path.name)
        if asset:
            if (asset.get("state") != "uploaded"
                    or asset.get("digest") != f"sha256:{digest(path)}"):
                raise ValueError(
                    f"existing asset {path.name} differs or cannot be verified; "
                    "refusing to overwrite it"
                )
        else:
            missing.append(str(path))
    if missing:
        subprocess.run(["gh", "release", "upload", tag, *missing, "--repo", repo], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    validate = commands.add_parser("validate")
    validate.add_argument("--tag", required=True)
    validate.add_argument("--expected-commit", required=True)
    build = commands.add_parser("package")
    build.add_argument("--tag", required=True)
    build.add_argument("--commit", required=True)
    build.add_argument("--date", required=True)
    build.add_argument("--output", default="dist")
    upload = commands.add_parser("publish")
    upload.add_argument("--tag", required=True)
    upload.add_argument("--release-id", required=True)
    upload.add_argument("--repo", required=True)
    upload.add_argument("--output", default="dist")
    args = parser.parse_args()
    if args.action == "validate":
        print(f"commit={validate_source(args.tag, args.expected_commit)}")
    elif args.action == "package":
        package(args.tag, args.commit, args.date, args.output)
    else:
        publish(args.tag, args.release_id, args.repo, args.output)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"release: {error}")
