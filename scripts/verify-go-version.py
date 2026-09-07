#!/usr/bin/env python3

# SPDX-FileCopyrightText: 2026 Michał Iwanicki <iwanicki92@gmail.com>
#
# SPDX-License-Identifier: Apache-2.0

"""Fail if pre-commit's pinned Go version doesn't match go.mod's `go` directive."""
import re
import sys
from pathlib import Path

import yaml

GO_MOD = Path("go.mod")
PRECOMMIT_CONFIG = Path(".pre-commit-config.yaml")


def go_mod_version() -> str:
    """Return go version from go.mod"""
    text = GO_MOD.read_text()
    match = re.search(r"^go (\d+\.\d+(?:\.\d+)?)", text, re.MULTILINE)
    if not match:
        sys.exit(f"error: no 'go' directive found in {GO_MOD}")
    return match.group(1)


def pinned_versions() -> list[tuple[str, str]]:
    """Return list of all golang hooks and their pinned version (or "")"""
    config = yaml.safe_load(PRECOMMIT_CONFIG.read_text())
    versions = []
    for repo in config.get("repos", []):
        for hook in repo.get("hooks", []):
            if hook.get("language") == "golang":
                versions.append((str(hook["id"]), str(hook.get("language_version", ""))))
    return versions


def main() -> int:
    mod_version = go_mod_version()
    versions = pinned_versions()

    if not versions:
        return 0

    mismatched = {hook: v for hook, v in versions if not v.startswith(mod_version)}
    for hook, version in mismatched.items():
        print(f'error: {hook} hook pins Go version to "{version}" but '
              f'go.mod uses Go "{mod_version}"'
        )
    if mismatched:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
