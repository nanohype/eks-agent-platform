#!/usr/bin/env python3
"""Every version pinned outside a manifest Renovate parses must have a customManager.

Renovate reads go.mod, package.json, Dockerfile `FROM` lines and workflow `uses:`
refs on its own. It does not read a version a workflow step installs itself, an
action input naming a tool release, or a Makefile variable. Those pins are
invisible to it, and an invisible pin does not announce that it has aged — it
just quietly stops being current.

The pins that age worst are the security tools. A stale scanner reports a clean
tree, which is indistinguishable from a clean tree.

WHY THIS IS AN ASSERTION AND NOT A COMMENT

A comment listing which files carry versions is true when written and has no
mechanism keeping it true. Someone adds a pinned tool to a workflow, the comment
still describes the old set, and nothing fails. So the classification is executed
instead: this walks the files that CAN carry an unmanaged pin, extracts every
pin, and requires each one to be matched by a customManager regex in
renovate.json. A new pin with no manager fails the build.

The manager regexes are applied as written, so this also catches a manager whose
regex has drifted from the file it claims to match — a customManager that
matches nothing is the same silent nothing as no manager at all.

    scripts/check-version-pins.py [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
RENOVATE = ROOT / "renovate.json"

# Where an unmanaged pin can hide, and the shape it takes there.
#
# Deliberately NOT a list of every file with a number in it: the question is
# which pins Renovate cannot see. go.mod, package.json, `uses:` SHAs and
# Dockerfile FROM digests are all natively managed and are excluded for that
# reason rather than because they carry no version.
PIN_PATTERNS: list[tuple[str, re.Pattern[str], str]] = [
    (
        ".github/workflows",
        re.compile(r"^\s*(?P<name>[A-Z][A-Z0-9_]*_VERSION):\s*\"?(?P<value>[^\"\s]+)\"?\s*$", re.M),
        "a tool version the step installs itself",
    ),
    (
        ".github/workflows",
        re.compile(r"^\s*(?P<name>[a-z_]+_version):\s*\"?(?P<value>[^\"\s]+)\"?\s*$", re.M),
        "a setup-action input naming a tool release",
    ),
    (
        "operators/Makefile",
        re.compile(r"^(?P<name>[A-Z][A-Z0-9_]*_VERSION)\s*\?=\s*(?P<value>\S+)", re.M),
        "a Makefile tool version",
    ),
    (
        "sandbox-worker/Dockerfile",
        re.compile(r"^ARG (?P<name>[A-Z][A-Z0-9_]*_VERSION)=(?P<value>\S+)", re.M),
        "a Dockerfile build-arg version",
    ),
]

# Inputs that are versions of a LANGUAGE RUNTIME resolved from a manifest the
# repo already pins, so they are not independent pins. Named individually rather
# than pattern-excluded, so adding one is a decision.
NOT_A_PIN = {"node-version", "python-version", "go-version", "node_version", "go_version"}


def files_for(spec: str) -> list[pathlib.Path]:
    p = ROOT / spec
    if p.is_dir():
        return sorted(list(p.glob("*.yaml")) + list(p.glob("*.yml")))
    return [p] if p.is_file() else []


def discovered_pins() -> list[tuple[pathlib.Path, str, str, str]]:
    """(file, pin name, value, why it counts) for every unmanaged-capable pin."""
    out = []
    for spec, pattern, why in PIN_PATTERNS:
        found = files_for(spec)
        if not found:
            sys.exit(
                f"check-version-pins: {spec} matched no file. Finding no pins and looking "
                "nowhere produce the same silence, so this is an error rather than a pass."
            )
        for f in found:
            text = f.read_text(encoding="utf-8")
            for m in pattern.finditer(text):
                if m.group("name") in NOT_A_PIN:
                    continue
                out.append((f, m.group("name"), m.group("value"), why))
    return out


def manager_regexes() -> list[tuple[str, re.Pattern[str]]]:
    if not RENOVATE.is_file():
        sys.exit("check-version-pins: renovate.json is missing; nothing manages any pin")
    cfg = json.loads(RENOVATE.read_text(encoding="utf-8"))
    managers = cfg.get("customManagers", [])
    if not managers:
        sys.exit(
            "check-version-pins: renovate.json declares no customManagers, so every pin outside "
            "a natively-parsed manifest is unwatched."
        )
    out = []
    for mgr in managers:
        name = mgr.get("depNameTemplate", "<unnamed>")
        for ms in mgr.get("matchStrings", []):
            # Renovate's matchStrings use the JavaScript named-group syntax
            # `(?<name>...)`; Python's re spells it `(?P<name>...)`. Translating
            # rather than requiring the config to be written twice keeps
            # renovate.json the single copy — Renovate is the consumer that
            # matters, and this check reads what Renovate will read.
            translated = re.sub(r"\(\?<(?![=!])", "(?P<", ms)
            try:
                out.append((name, re.compile(translated, re.M)))
            except re.error as e:
                sys.exit(f"check-version-pins: customManager {name} has an uncompilable matchString: {e}")
    return out


def self_test() -> int:
    """Prove the matcher can report a pin as unmanaged.

    Without this the check could match everything — a regex list that always hit
    would report full coverage, which is the failure this file exists to catch.
    """
    fake_pin = "TOTALLY_UNMANAGED_VERSION: 9.9.9"
    if any(rx.search(fake_pin) for _n, rx in manager_regexes()):
        print("self-test: a pin no manager declares was reported as managed", file=sys.stderr)
        return 1
    # And a pin that IS managed must be seen, or the check passes vacuously.
    real = 'GITLEAKS_VERSION: "8.30.1"'
    if not any(rx.search(real) for _n, rx in manager_regexes()):
        print("self-test: a managed pin was not matched by any manager regex", file=sys.stderr)
        return 1
    print("✓ check-version-pins self-test: the matcher distinguishes a managed pin from an unmanaged one")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print every pin found and the manager covering it")
    ap.add_argument("--self-test", action="store_true", help="prove the matcher can report a pin unmanaged")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    pins = discovered_pins()
    if not pins:
        print(
            "check-version-pins: no pins discovered at all. The patterns have stopped matching "
            "the files they describe; that is a broken check, not a clean tree.",
            file=sys.stderr,
        )
        return 1

    managers = manager_regexes()
    unmanaged = []
    covered: list[tuple[str, str, str]] = []

    for f, name, value, why in pins:
        line = f"{name}: {value}"
        alt = f"{name}={value}"
        alt2 = f"{name} ?= {value}"
        owner = None
        for mgr_name, rx in managers:
            if rx.search(line) or rx.search(alt) or rx.search(alt2) or rx.search(f"ARG {alt}"):
                owner = mgr_name
                break
        if owner:
            covered.append((str(f.relative_to(ROOT)), line, owner))
        else:
            unmanaged.append((str(f.relative_to(ROOT)), name, value, why))

    # A manager matching nothing is the same silent nothing as no manager.
    owners_used = {o for _f, _l, o in covered}
    idle = [n for n, _rx in managers if n not in owners_used]

    if args.list:
        for f, line, owner in covered:
            print(f"  {f}: {line}  ←  {owner}")

    problems = False
    if unmanaged:
        problems = True
        print(f"\n{len(unmanaged)} version pin(s) no Renovate customManager watches:\n", file=sys.stderr)
        for f, name, value, why in unmanaged:
            print(f"  - {f}: {name}={value} ({why})", file=sys.stderr)
        print(
            "\nRenovate reads go.mod, package.json, Dockerfile FROM digests and workflow `uses:` refs\n"
            "on its own; it cannot see these. Add a customManager to renovate.json, or move the pin\n"
            "somewhere natively managed.",
            file=sys.stderr,
        )
    if idle:
        problems = True
        print(f"\n{len(idle)} customManager(s) matched no pin: {', '.join(sorted(set(idle)))}", file=sys.stderr)
        print(
            "A manager whose regex matches nothing watches nothing, and reads in review as coverage.\n"
            "Either the pin it named is gone (delete the manager) or its regex has drifted from the file.",
            file=sys.stderr,
        )

    if problems:
        return 1

    print(f"✓ {len(covered)} version pins outside a natively-parsed manifest, every one Renovate-managed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
