#!/usr/bin/env python3
"""Assemble a platform release tree: binary, scripts/, docs/, README, LICENSE, BUILD.txt, SHA256SUMS.txt."""

from __future__ import annotations

import argparse
import hashlib
import os
import pathlib
import shutil
import stat
import sys


def _sha256_file(path: pathlib.Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _write_inner_checksums(root: pathlib.Path) -> None:
    files = sorted(p for p in root.rglob("*") if p.is_file() and p.name != "SHA256SUMS.txt")
    lines = []
    for p in files:
        rel = p.relative_to(root).as_posix()
        lines.append(f"{_sha256_file(p)}  {rel}\n")
    (root / "SHA256SUMS.txt").write_text("".join(lines), encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True, help="Rust target triple")
    parser.add_argument(
        "--package",
        required=True,
        choices=("macos-arm64", "windows-x64"),
        help="Release package id",
    )
    parser.add_argument("--version", required=True, help="Release version without v prefix")
    parser.add_argument(
        "--commit",
        default=os.environ.get("GITHUB_SHA", "unknown"),
        help="Git commit SHA for BUILD.txt",
    )
    parser.add_argument("--binary-root", default="target", help="Cargo target directory")
    parser.add_argument("--out-root", default="artifact", help="Output parent directory")
    args = parser.parse_args()

    is_windows = args.package == "windows-x64"
    bin_name = "remote-au.exe" if is_windows else "remote-au"
    src_bin = pathlib.Path(args.binary_root) / args.target / "release" / bin_name
    if not src_bin.is_file():
        print(f"missing release binary: {src_bin}", file=sys.stderr)
        return 1

    out = pathlib.Path(args.out_root) / f"remote-au-{args.package}"
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    dest_bin = out / bin_name
    shutil.copy2(src_bin, dest_bin)
    if not is_windows:
        dest_bin.chmod(dest_bin.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)

    scripts_src = "scripts/windows" if is_windows else "scripts/macos"
    shutil.copytree(scripts_src, out / "scripts")
    shutil.copytree("docs", out / "docs")
    shutil.copy2("README.md", out / "README.md")
    shutil.copy2("LICENSE", out / "LICENSE")
    (out / "BUILD.txt").write_text(
        f"commit={args.commit}\ntarget={args.target}\nversion={args.version}\n",
        encoding="utf-8",
    )
    _write_inner_checksums(out)
    print(f"packaged {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
