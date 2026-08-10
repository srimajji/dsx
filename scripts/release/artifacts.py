#!/usr/bin/env python3
"""Create and verify deterministic DSX release manifests and archives."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import struct
import sys
import zipfile

SCHEMA = "dsx.release/v1"
PIN_RE = re.compile(r"^[a-z0-9.-]+(?::[0-9]+)?/[^\s@]+@sha256:[0-9a-f]{64}$")
SHA_RE = re.compile(r"^[0-9a-f]{64}$")
SEMVER_RE = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
FILES = ("bin/dsx", "bin/dsx-guest", "dsx.spdx.json")
SBOM_TOOL_VERSION = "1.29.0"


class ReleaseError(ValueError):
    pass


def digest(path: Path) -> tuple[str, int]:
    mode = path.lstat().st_mode
    if not stat.S_ISREG(mode) or path.is_symlink():
        raise ReleaseError(f"{path}: artifact must be a regular file, not a symlink")
    hasher = hashlib.sha256()
    size = 0
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            hasher.update(chunk)
            size += len(chunk)
    return hasher.hexdigest(), size


def require_metadata(version: str, commit: str, built_at: str) -> None:
    if not SEMVER_RE.fullmatch(version):
        raise ReleaseError("version must be a concrete SemVer release")
    if not COMMIT_RE.fullmatch(commit):
        raise ReleaseError("commit must be the full lowercase 40-hex Git object ID")
    try:
        parsed = dt.datetime.fromisoformat(built_at.replace("Z", "+00:00"))
    except ValueError as error:
        raise ReleaseError("built_at must be RFC3339 UTC") from error
    if parsed.tzinfo != dt.timezone.utc or parsed.microsecond:
        raise ReleaseError("built_at must be whole-second RFC3339 UTC")


def require_pin(name: str, value: str) -> None:
    if not PIN_RE.fullmatch(value) or value.startswith(("localhost/", "dsx.local/")):
        raise ReleaseError(f"{name} must be a published immutable registry reference ending in @sha256:<64 lowercase hex>")


def executable_format(path: Path, expected: str) -> None:
    data = path.read_bytes()[:64]
    if expected == "mach-o-arm64":
        if len(data) < 12 or data[:4] != b"\xcf\xfa\xed\xfe" or struct.unpack("<I", data[4:8])[0] != 0x0100000C:
            raise ReleaseError(f"{path}: expected 64-bit arm64 Mach-O")
    elif expected == "elf-arm64-static":
        if len(data) < 20 or data[:4] != b"\x7fELF" or data[4:6] != b"\x02\x01" or struct.unpack("<H", data[18:20])[0] != 183:
            raise ReleaseError(f"{path}: expected little-endian 64-bit AArch64 ELF")
        # A static Go guest has no PT_INTERP. Parse enough ELF64 program headers
        # to reject a dynamically linked candidate without executing Linux code.
        if len(data) >= 64:
            phoff = struct.unpack("<Q", data[32:40])[0]
            phentsize = struct.unpack("<H", data[54:56])[0]
            phnum = struct.unpack("<H", data[56:58])[0]
            with path.open("rb") as source:
                for index in range(phnum):
                    source.seek(phoff + index * phentsize)
                    header = source.read(4)
                    if len(header) != 4:
                        raise ReleaseError(f"{path}: truncated ELF program headers")
                    if struct.unpack("<I", header)[0] == 3:
                        raise ReleaseError(f"{path}: Linux guest contains a dynamic interpreter")
    else:
        raise ReleaseError(f"unsupported artifact format {expected!r}")
    mode = path.stat().st_mode
    if not stat.S_ISREG(mode) or mode & 0o111 == 0:
        raise ReleaseError(f"{path}: artifact is not an executable regular file")


def normalize_sbom(path: Path, version: str, built_at: str) -> None:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ReleaseError(f"invalid SPDX JSON from pinned Syft: {error}") from error
    if value.get("spdxVersion") != "SPDX-2.3":
        raise ReleaseError("Syft output must be SPDX 2.3 JSON")
    value["name"] = f"dsx-{version}-darwin-arm64"
    value["documentNamespace"] = f"https://github.com/srimajji/dsx/releases/{version}/sbom"
    creation = value.setdefault("creationInfo", {})
    creation["created"] = built_at
    if isinstance(value.get("packages"), list):
        value["packages"].sort(key=lambda item: (item.get("SPDXID", ""), item.get("name", "")))
    if isinstance(value.get("relationships"), list):
        value["relationships"].sort(key=lambda item: (
            item.get("spdxElementId", ""), item.get("relationshipType", ""), item.get("relatedSpdxElement", "")
        ))
    path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")


def make_manifest(args: argparse.Namespace) -> None:
    root = args.root.resolve()
    require_metadata(args.version, args.commit, args.built_at)
    if args.syft_version != SBOM_TOOL_VERSION:
        raise ReleaseError(f"SBOM generation requires pinned Syft {SBOM_TOOL_VERSION}")
    require_pin("agent image", args.agent_image)
    require_pin("browser image", args.browser_image)
    guest_digest, _ = digest(root / "bin/dsx-guest")
    if args.guest_sha256 != guest_digest:
        raise ReleaseError("guest SHA-256 does not match the packaged Linux helper")
    executable_format(root / "bin/dsx", "mach-o-arm64")
    executable_format(root / "bin/dsx-guest", "elf-arm64-static")
    records = []
    for relative in FILES:
        path = root / relative
        sha256, size = digest(path)
        records.append({
            "path": relative,
            "sha256": sha256,
            "size": size,
            "format": {"bin/dsx": "mach-o-arm64", "bin/dsx-guest": "elf-arm64-static"}.get(relative, "spdx-2.3-json"),
        })
    manifest = {
        "schema": SCHEMA,
        "artifact": "dsx-darwin-arm64",
        "build": {"version": args.version, "commit": args.commit, "built_at": args.built_at, "guest_sha256": guest_digest},
        "images": {"agent": args.agent_image, "browser": args.browser_image},
        "files": records,
        "sbom": {"format": "spdx-2.3-json", "tool": "syft", "version": args.syft_version},
        "release_policy": {"codesign": "Developer ID Application", "notarization": "required"},
    }
    args.output.write_text(json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")


def verify_manifest(args: argparse.Namespace) -> None:
    root = args.root.resolve()
    try:
        manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ReleaseError(f"invalid release manifest: {error}") from error
    if not isinstance(manifest, dict) or set(manifest) != {"schema", "artifact", "build", "images", "files", "sbom", "release_policy"}:
        raise ReleaseError("release manifest fields are invalid")
    if manifest.get("schema") != SCHEMA or manifest.get("artifact") != "dsx-darwin-arm64":
        raise ReleaseError("unsupported release manifest schema or artifact")
    build = manifest.get("build", {})
    if not isinstance(build, dict) or set(build) != {"version", "commit", "built_at", "guest_sha256"}:
        raise ReleaseError("release build metadata fields are invalid")
    require_metadata(build.get("version", ""), build.get("commit", ""), build.get("built_at", ""))
    if args.expected_version and build["version"] != args.expected_version:
        raise ReleaseError("release version does not match the expected version")
    if args.expected_commit and build["commit"] != args.expected_commit:
        raise ReleaseError("release commit does not match the expected commit")
    images = manifest.get("images", {})
    if not isinstance(images, dict) or set(images) != {"agent", "browser"}:
        raise ReleaseError("release image metadata fields are invalid")
    require_pin("agent image", images.get("agent", ""))
    require_pin("browser image", images.get("browser", ""))
    records = manifest.get("files")
    if (not isinstance(records, list) or any(not isinstance(item, dict) for item in records)
            or any(set(item) != {"path", "sha256", "size", "format"} for item in records)
            or [item.get("path") for item in records] != list(FILES)):
        raise ReleaseError("release manifest file set, fields, or ordering is invalid")
    for item in records:
        relative = item["path"]
        if PurePosixPath(relative).is_absolute() or ".." in PurePosixPath(relative).parts:
            raise ReleaseError("release manifest contains an unsafe path")
        expected_format = {"bin/dsx": "mach-o-arm64", "bin/dsx-guest": "elf-arm64-static", "dsx.spdx.json": "spdx-2.3-json"}[relative]
        if item["format"] != expected_format:
            raise ReleaseError(f"artifact format declaration mismatch: {relative}")
        path = root / relative
        observed, size = digest(path)
        if not SHA_RE.fullmatch(item.get("sha256", "")) or observed != item["sha256"] or size != item.get("size"):
            raise ReleaseError(f"artifact digest or size mismatch: {relative}")
        if relative == "bin/dsx":
            executable_format(path, "mach-o-arm64")
        elif relative == "bin/dsx-guest":
            executable_format(path, "elf-arm64-static")
            if observed != build.get("guest_sha256"):
                raise ReleaseError("guest helper does not match compiled release digest")
        elif relative == "dsx.spdx.json":
            try:
                sbom = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError) as error:
                raise ReleaseError(f"invalid packaged SPDX SBOM: {error}") from error
            if (sbom.get("spdxVersion") != "SPDX-2.3"
                    or sbom.get("name") != f"dsx-{build['version']}-darwin-arm64"
                    or sbom.get("creationInfo", {}).get("created") != build["built_at"]):
                raise ReleaseError("packaged SPDX SBOM metadata does not match the release")
    sbom_record = manifest.get("sbom")
    if (not isinstance(sbom_record, dict)
            or set(sbom_record) != {"format", "tool", "version"}
            or sbom_record.get("format") != "spdx-2.3-json"
            or sbom_record.get("tool") != "syft"
            or sbom_record.get("version") != SBOM_TOOL_VERSION):
        raise ReleaseError("release SBOM tool metadata is invalid")
    policy = manifest.get("release_policy", {})
    if policy != {"codesign": "Developer ID Application", "notarization": "required"}:
        raise ReleaseError("release signature/notarization policy is missing")
    if args.host_metadata:
        try:
            metadata = json.loads(args.host_metadata.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise ReleaseError(f"invalid host version metadata: {error}") from error
        expected = {
            "version": build["version"], "commit": build["commit"], "built_at": build["built_at"],
            "guest_sha256": build["guest_sha256"], "agent_image": images["agent"], "browser_image": images["browser"],
        }
        for key, value in expected.items():
            if metadata.get(key) != value:
                raise ReleaseError(f"host metadata mismatch: {key}")


def verify_security(args: argparse.Namespace) -> None:
    details = args.codesign_details.read_text(encoding="utf-8")
    if "Authority=Developer ID Application: " not in details:
        raise ReleaseError("candidate is unsigned, ad-hoc signed, or lacks a Developer ID Application authority")
    if "Timestamp=" not in details:
        raise ReleaseError("candidate signature has no trusted timestamp")
    try:
        notarization = json.loads(args.notarization_result.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ReleaseError(f"invalid notarization evidence: {error}") from error
    if notarization.get("status") != "Accepted" or not notarization.get("id"):
        raise ReleaseError("notarization evidence is not Accepted or has no submission ID")


def make_archive(args: argparse.Namespace) -> None:
    root = args.root.resolve()
    manifest = json.loads((root / "release-manifest.json").read_text(encoding="utf-8"))
    built_at = dt.datetime.fromisoformat(manifest["build"]["built_at"].replace("Z", "+00:00"))
    stamp = max((1980, 1, 1, 0, 0, 0), (built_at.year, built_at.month, built_at.day, built_at.hour, built_at.minute, built_at.second))
    entries = (*FILES, "release-manifest.json")
    with zipfile.ZipFile(args.output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for relative in entries:
            data = (root / relative).read_bytes()
            info = zipfile.ZipInfo(f"dsx-{manifest['build']['version']}-darwin-arm64/{relative}", stamp)
            info.create_system = 3
            info.external_attr = ((0o755 if relative.startswith("bin/") else 0o644) | stat.S_IFREG) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, data, compresslevel=9)


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser()
    sub = root.add_subparsers(dest="command", required=True)
    normalize = sub.add_parser("normalize-sbom")
    normalize.add_argument("--path", type=Path, required=True)
    normalize.add_argument("--version", required=True)
    normalize.add_argument("--built-at", required=True)
    manifest = sub.add_parser("manifest")
    manifest.add_argument("--root", type=Path, required=True)
    manifest.add_argument("--output", type=Path, required=True)
    manifest.add_argument("--version", required=True)
    manifest.add_argument("--commit", required=True)
    manifest.add_argument("--built-at", required=True)
    manifest.add_argument("--guest-sha256", required=True)
    manifest.add_argument("--agent-image", required=True)
    manifest.add_argument("--browser-image", required=True)
    manifest.add_argument("--syft-version", required=True)
    verify = sub.add_parser("verify")
    verify.add_argument("--root", type=Path, required=True)
    verify.add_argument("--manifest", type=Path, required=True)
    verify.add_argument("--host-metadata", type=Path)
    verify.add_argument("--expected-version")
    verify.add_argument("--expected-commit")
    security = sub.add_parser("verify-security")
    security.add_argument("--codesign-details", type=Path, required=True)
    security.add_argument("--notarization-result", type=Path, required=True)
    archive = sub.add_parser("archive")
    archive.add_argument("--root", type=Path, required=True)
    archive.add_argument("--output", type=Path, required=True)
    return root


def main() -> int:
    args = parser().parse_args()
    try:
        if args.command == "normalize-sbom":
            normalize_sbom(args.path, args.version, args.built_at)
        elif args.command == "manifest":
            make_manifest(args)
        elif args.command == "verify":
            verify_manifest(args)
        elif args.command == "verify-security":
            verify_security(args)
        else:
            make_archive(args)
    except (ReleaseError, OSError, KeyError, TypeError) as error:
        print(f"dsx release: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
