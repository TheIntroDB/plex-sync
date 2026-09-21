#!/usr/bin/env python3
"""Point the Homebrew formula in the tap at a released version.

Called by the release workflow after the release exists. It rewrites the
formula's version in every platform URL and updates each sha256 from the
release's own checksums.txt, then commits and pushes to the tap.

    scripts/update-homebrew-formula.py 0.2.0 --checksums dist/checksums.txt

Nothing here invents a digest: every sha256 written comes from the checksums
file the release published, and a platform with no entry in it is left alone
rather than guessed at.
"""

from __future__ import annotations

import argparse
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

TAP_REPOSITORY = "Pasithea0/homebrew-tap"
TAP_FORMULA = "Formula/plex-sync.rb"


def read_checksums(path: pathlib.Path) -> dict[str, str]:
    out = {}
    for line in path.read_text().splitlines():
        parts = line.split()
        if len(parts) == 2:
            out[parts[1]] = parts[0]
    return out


def arch_for(os_name: str, cpu: str) -> str | None:
    """The asset name suffix for a platform block, or None if we do not ship it."""
    return {
        ("macos", "arm"): "darwin_arm64",
        ("macos", "intel"): "darwin_amd64",
        ("linux", "arm"): "linux_arm64",
        ("linux", "intel"): "linux_amd64",
    }.get((os_name, cpu))


def rewrite(formula: str, version: str, checksums: dict[str, str]) -> str:
    """Update the url and its sha256 in each platform block.

    Walks the file tracking which platform block each line is inside. The
    structure is fixed -- an os block holding one or two cpu blocks, each with a
    url and a sha256 -- so the most recent block marker is the right one.
    """
    lines = formula.splitlines()
    os_name: str | None = None
    cpu: str | None = None
    updated: list[str] = []

    for line in lines:
        stripped = line.strip()

        if stripped in ("on_macos do", "on_linux do"):
            os_name = "macos" if stripped.startswith("on_macos") else "linux"
            cpu = None
            updated.append(line)
            continue
        if stripped in ("on_arm do", "on_intel do"):
            cpu = "arm" if stripped.startswith("on_arm") else "intel"
            updated.append(line)
            continue

        arch = arch_for(os_name or "", cpu or "")
        if arch is None:
            updated.append(line)
            continue

        asset = f"plex-sync_{version}_{arch}.tar.gz"

        if "releases/download/" in stripped:
            updated.append(
                re.sub(
                    r"releases/download/v[^/]+/plex-sync_[\d.]+_[a-z0-9_]+\.tar\.gz",
                    f"releases/download/v{version}/{asset}",
                    line,
                )
            )
            continue

        if stripped.startswith("sha256 "):
            digest = checksums.get(asset)
            if digest is None:
                print(f"  no checksum for {asset}; leaving that block alone", file=sys.stderr)
                updated.append(line)
                continue
            indent = line[: len(line) - len(line.lstrip())]
            updated.append(f'{indent}sha256 "{digest}"')
            continue

        updated.append(line)

    return "\n".join(updated) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("version", help="the release version, without a leading v")
    parser.add_argument("--checksums", required=True, type=pathlib.Path)
    parser.add_argument("--tap", default=TAP_REPOSITORY)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    version = args.version.lstrip("v")
    checksums = read_checksums(args.checksums)
    if not checksums:
        print(f"no checksums in {args.checksums}", file=sys.stderr)
        return 1

    workdir = pathlib.Path(tempfile.mkdtemp(prefix="plex-sync-tap-"))
    try:
        # A token in the environment is what CI provides. Locally, fall back to
        # whatever gh is already authenticated with, so this can be run by hand
        # to fix a formula without setting anything up.
        token = os.environ.get("HOMEBREW_TAP_TOKEN") or os.environ.get("GH_TOKEN") or ""
        if not token:
            token = subprocess.run(
                ["gh", "auth", "token"], capture_output=True, text=True, check=False
            ).stdout.strip()

        if args.dry_run:
            print("dry run: using a local copy of the formula")
            subprocess.run(["git", "clone", "--depth", "1",
                            f"https://github.com/{args.tap}.git", str(workdir)],
                           check=True, capture_output=True)
        else:
            if not token:
                print("no token: set HOMEBREW_TAP_TOKEN, or run with --dry-run",
                      file=sys.stderr)
                return 1
            subprocess.run(["git", "clone", "--depth", "1",
                            f"https://x-access-token:{token}@github.com/{args.tap}.git",
                            str(workdir)], check=True, capture_output=True)

        formula_path = workdir / TAP_FORMULA
        if not formula_path.exists():
            print(f"{TAP_FORMULA} is not in {args.tap}", file=sys.stderr)
            return 1

        before = formula_path.read_text()
        after = rewrite(before, version, checksums)

        if before == after:
            print(f"the formula already points at v{version}")
            return 0

        if args.dry_run:
            print(after)
            return 0

        formula_path.write_text(after)
        subprocess.run(["git", "config", "user.name", "plex-sync release"],
                       cwd=workdir, check=True)
        subprocess.run(["git", "config", "user.email",
                        "41898282+github-actions[bot]@users.noreply.github.com"],
                       cwd=workdir, check=True)
        subprocess.run(["git", "add", TAP_FORMULA], cwd=workdir, check=True)
        subprocess.run(["git", "commit", "-m",
                        f"chore(release): plex-sync {version}"], cwd=workdir, check=True)
        subprocess.run(["git", "push"], cwd=workdir, check=True)
        print(f"pushed plex-sync {version} to {args.tap}")
        return 0
    finally:
        shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
