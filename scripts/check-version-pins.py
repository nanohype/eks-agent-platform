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

The manager regexes are applied as written, and only to the files the manager's
managerFilePatterns select, so this also catches a manager whose regex has
drifted from the file it claims to match, and one whose file patterns do not
reach the file at all — a pin moved into a file Renovate does not read for
that manager is unwatched however well the regex matches its text. A
customManager that matches nothing is the same silent nothing as no manager.

LOCK FILE MAINTENANCE BELONGS TO THE LOCKFILE

One pnpm-lock.yaml serves every package.json in the workspace, and Renovate
raises a lock file maintenance update for each of those package files. A
packageRule that selects one of them by file name, and does not leave the
lockFileMaintenance update type out, moves a refresh of the WHOLE lockfile onto
that rule's branch and schedule, beside the workspace-wide refresh: the same
file refreshed twice, by two pull requests on two clocks. So a packageRule that
selects a workspace package.json through matchFileNames names its update types,
and lockFileMaintenance is not one of them.

    scripts/check-version-pins.py [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import fnmatch
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
RENOVATE = ROOT / "renovate.json"
WORKSPACE = ROOT / "pnpm-workspace.yaml"

# The update type Renovate gives a lockfile refresh, and the manager that owns
# a pnpm workspace's package.json files.
LOCKFILE_UPDATE = "lockFileMaintenance"
WORKSPACE_MANAGER = "npm"

# Where an unmanaged pin can hide, and the shape it takes there.
#
# Deliberately NOT a list of every file with a number in it: the question is
# which pins Renovate cannot see. go.mod, package.json, `uses:` SHAs and
# Dockerfile FROM digests are all natively managed and are excluded for that
# reason rather than because they carry no version.
#
# Each entry names the files it reads and every one has to match a file: a
# shape searched for nowhere finds nothing, and that silence reads as a pass.
# A directory is read recursively for YAML, which is how a composite action's
# action.yml under .github/actions/ is covered beside the workflows that call it.
ACTION_YAML = (".github/workflows", ".github/actions")
DOCKERFILES = ("sandbox-worker/Dockerfile", "packages/eval-runner/Dockerfile")

PIN_PATTERNS: list[tuple[tuple[str, ...], re.Pattern[str], str]] = [
    (
        ACTION_YAML,
        re.compile(r"^\s*(?P<name>[A-Z][A-Z0-9_]*_VERSION):\s*\"?(?P<value>[^\"\s]+)\"?\s*$", re.M),
        "a tool version the step installs itself",
    ),
    (
        ACTION_YAML,
        re.compile(r"^\s*(?P<name>[a-z_]+_version):\s*\"?(?P<value>[^\"\s]+)\"?\s*$", re.M),
        "a setup-action input naming a tool release",
    ),
    (
        ACTION_YAML,
        re.compile(r"^\s*(?P<name>[a-z]+-release):\s*[\"']?(?P<value>[^\"'\s]+)[\"']?\s*$", re.M),
        "an installer-action input naming the tool release it installs",
    ),
    (
        ACTION_YAML,
        re.compile(
            r"#\s*renovate:[^\n]*depName=(?P<name>\S+)[^\n]*\n\s*version:\s*\"?(?P<value>[^\"\s]+)\"?",
            re.M,
        ),
        "a setup-action version input, named by its Renovate marker",
    ),
    (
        ("operators/Makefile",),
        re.compile(r"^(?P<name>[A-Z][A-Z0-9_]*_VERSION)\s*\?=\s*(?P<value>\S+)", re.M),
        "a Makefile tool version",
    ),
    (
        DOCKERFILES,
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
        return sorted(list(p.rglob("*.yaml")) + list(p.rglob("*.yml")))
    return [p] if p.is_file() else []


def discovered_pins() -> list[tuple[pathlib.Path, str, str, str]]:
    """(file, pin name, value, why it counts) for every unmanaged-capable pin."""
    out = []
    for specs, pattern, why in PIN_PATTERNS:
        found = []
        for spec in specs:
            matched = files_for(spec)
            if not matched:
                sys.exit(
                    f"check-version-pins: {spec} matched no file. Finding no pins and looking "
                    "nowhere produce the same silence, so this is an error rather than a pass."
                )
            found.extend(matched)
        for f in found:
            text = f.read_text(encoding="utf-8")
            for m in pattern.finditer(text):
                if m.group("name") in NOT_A_PIN:
                    continue
                out.append((f, m.group("name"), m.group("value"), why))
    return out


# Renovate's managerFilePatterns entry for a regex: `/pattern/` with optional
# flags. Its other form, a glob, has matching rules this file does not
# reimplement, so a glob is refused rather than approximated.
FILE_PATTERN = re.compile(r"^/(?P<body>.+)/(?P<flags>[a-z]*)$")


def file_patterns(name: str, mgr: dict) -> list[re.Pattern[str]]:
    """The paths a manager reads, as compiled regexes over repo-relative paths."""
    raw = mgr.get("managerFilePatterns") or []
    if not raw:
        sys.exit(
            f"check-version-pins: customManager {name} declares no managerFilePatterns, so Renovate "
            "applies it to no file and every pin it names is unwatched."
        )
    out = []
    for fp in raw:
        m = FILE_PATTERN.match(fp)
        if not m:
            sys.exit(
                f"check-version-pins: customManager {name} has managerFilePatterns entry {fp!r}, "
                "which is not a /regex/. Write it as one, so this check reads the files Renovate does."
            )
        flags = re.I if "i" in m.group("flags") else 0
        try:
            out.append(re.compile(m.group("body"), flags))
        except re.error as e:
            sys.exit(f"check-version-pins: customManager {name} has an uncompilable managerFilePatterns entry: {e}")
    return out


def manages(patterns: list[re.Pattern[str]], path: pathlib.Path) -> bool:
    rel = path.relative_to(ROOT).as_posix()
    return any(fp.search(rel) for fp in patterns)


def manager_regexes() -> list[tuple[str, re.Pattern[str], list[re.Pattern[str]]]]:
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
        files = file_patterns(name, mgr)
        for ms in mgr.get("matchStrings", []):
            # Renovate's matchStrings use the JavaScript named-group syntax
            # `(?<name>...)`; Python's re spells it `(?P<name>...)`. Translating
            # rather than requiring the config to be written twice keeps
            # renovate.json the single copy — Renovate is the consumer that
            # matters, and this check reads what Renovate will read.
            translated = re.sub(r"\(\?<(?![=!])", "(?P<", ms)
            try:
                out.append((name, re.compile(translated, re.M), files))
            except re.error as e:
                sys.exit(f"check-version-pins: customManager {name} has an uncompilable matchString: {e}")
    return out


def workspace_manifests() -> list[str]:
    """Every package.json pnpm reads as a workspace member, repo-relative.

    The members are the root package.json and the directories pnpm-workspace.yaml's
    `packages:` globs name. A workspace read as empty is an error: no member found
    and no member looked for produce the same silence.
    """
    if not WORKSPACE.is_file():
        sys.exit(
            "check-version-pins: pnpm-workspace.yaml is missing, so which package.json files "
            "share the lockfile is unknown"
        )
    globs: list[str] = []
    in_packages = False
    for line in WORKSPACE.read_text(encoding="utf-8").splitlines():
        if re.match(r"^packages:\s*$", line):
            in_packages = True
            continue
        if not in_packages:
            continue
        item = re.match(r"""^\s+-\s+["']?([^"'\s#]+)["']?\s*(#.*)?$""", line)
        if item:
            globs.append(item.group(1).rstrip("/"))
        elif line.strip() and not line.lstrip().startswith("#"):
            break
    include = [g for g in globs if not g.startswith("!")]
    exclude = [g[1:] for g in globs if g.startswith("!")]
    if not include:
        sys.exit(
            "check-version-pins: pnpm-workspace.yaml names no packages: globs, so no workspace "
            "package.json was examined. That is a broken read, not a clean workspace."
        )
    found = {"package.json"} if (ROOT / "package.json").is_file() else set()
    for pattern in include:
        for manifest in ROOT.glob(f"{pattern}/package.json"):
            member = manifest.parent.relative_to(ROOT).as_posix()
            if "node_modules" in member.split("/") or any(fnmatch.fnmatchcase(member, x) for x in exclude):
                continue
            found.add(f"{member}/package.json")
    return sorted(found)


def file_name_selector(label: str, entries: list[str]):
    """A predicate over repo-relative paths: does this matchFileNames list select one?

    Renovate reads an entry as an exact path, a /regex/ or a glob. The first two
    are matched here as Renovate matches them. A glob is refused rather than
    approximated, as file_patterns() refuses one, so this check never passes a
    rule it could not read.
    """
    exact: set[str] = set()
    regexes: list[re.Pattern[str]] = []
    for entry in entries:
        m = FILE_PATTERN.match(entry)
        if m:
            flags = re.I if "i" in m.group("flags") else 0
            try:
                regexes.append(re.compile(m.group("body"), flags))
            except re.error as e:
                sys.exit(f"check-version-pins: packageRules{label} has an uncompilable matchFileNames entry: {e}")
        elif any(c in entry for c in "*?[]{}!"):
            sys.exit(
                f"check-version-pins: packageRules{label} has the matchFileNames glob {entry!r}. "
                "Write it as an exact path or a /regex/, so this check reads the files Renovate does."
            )
        else:
            exact.add(entry)
    return lambda rel: rel in exact or any(rx.search(rel) for rx in regexes)


def lockfile_splits(cfg: dict, manifests: list[str]) -> tuple[int, list[tuple[str, list[str]]]]:
    """How many packageRules select a workspace package.json by file name, and
    (rule, the files it selects) for each one that also takes lock file maintenance.

    A rule another manager owns never sees a package.json update, and one whose
    matchUpdateTypes leave lockFileMaintenance out never sees the lockfile's.
    """
    selecting = 0
    splits = []
    for i, rule in enumerate(cfg.get("packageRules", [])):
        entries = rule.get("matchFileNames")
        if not entries:
            continue
        managers = rule.get("matchManagers")
        if managers is not None and WORKSPACE_MANAGER not in managers:
            continue
        label = f"[{i}]" + (f" ({rule['groupName']})" if rule.get("groupName") else "")
        selects = file_name_selector(label, entries)
        hit = [m for m in manifests if selects(m)]
        if not hit:
            continue
        selecting += 1
        types = rule.get("matchUpdateTypes")
        if types is None or LOCKFILE_UPDATE in types:
            splits.append((label, hit))
    return selecting, splits


def self_test() -> int:
    """Prove the matcher can report a pin as unmanaged.

    Without this the check could match everything — a regex list that always hit
    would report full coverage, which is the failure this file exists to catch.
    """
    managers = manager_regexes()
    fake_pin = "TOTALLY_UNMANAGED_VERSION: 9.9.9"
    if any(rx.search(fake_pin) for _n, rx, _f in managers):
        print("self-test: a pin no manager declares was reported as managed", file=sys.stderr)
        return 1
    # And a pin that IS managed must be seen, or the check passes vacuously.
    real = 'GITLEAKS_VERSION: "8.30.1"'
    owners = [files for _n, rx, files in managers if rx.search(real)]
    if not owners:
        print("self-test: a managed pin was not matched by any manager regex", file=sys.stderr)
        return 1
    # The same text in a file outside the manager's managerFilePatterns is a pin
    # Renovate never reads, so the file half has to be able to say no as well.
    workflow = ROOT / ".github" / "workflows" / "security.yaml"
    elsewhere = ROOT / "operators" / "Makefile"
    if not any(manages(files, workflow) for files in owners):
        print("self-test: the managing regex's file patterns do not reach a workflow", file=sys.stderr)
        return 1
    if any(manages(files, elsewhere) for files in owners):
        print("self-test: a workflow-only manager was reported as reading operators/Makefile", file=sys.stderr)
        return 1
    # The lockfile half reads the real workspace, and has to be able to say both
    # yes and no about a rule.
    manifests = workspace_manifests()
    pricing = "packages/pricing/package.json"
    if pricing not in manifests or "package.json" not in manifests:
        print(f"self-test: the workspace read found {manifests}, without {pricing} or the root package.json", file=sys.stderr)
        return 1
    cases = [
        ({"matchFileNames": [pricing], "groupName": "g"}, True, "a file-matched rule naming no update types"),
        ({"matchFileNames": [pricing], "matchUpdateTypes": [LOCKFILE_UPDATE]}, True, "a file-matched rule naming lockFileMaintenance"),
        ({"matchFileNames": ["/^packages/pricing//"]}, True, "a /regex/ entry selecting a workspace package"),
        ({"matchFileNames": [pricing], "matchUpdateTypes": ["minor", "patch"]}, False, "a rule leaving lockFileMaintenance out"),
        ({"matchFileNames": ["packages/eval-runner/Dockerfile"]}, False, "a rule selecting no workspace package.json"),
        ({"matchFileNames": [pricing], "matchManagers": ["dockerfile"]}, False, "a rule another manager owns"),
    ]
    for rule, want, what in cases:
        if bool(lockfile_splits({"packageRules": [rule]}, manifests)[1]) != want:
            verdict = "passed" if want else "reported as taking lock file maintenance"
            print(f"self-test: {what} was {verdict}", file=sys.stderr)
            return 1
    print(
        "✓ check-version-pins self-test: the matcher distinguishes a managed pin from an unmanaged "
        "one, and a manager's file patterns from the files outside them; the lockfile check reports "
        f"a file-matched rule that takes lock file maintenance and passes {sum(not w for _r, w, _ in cases)} "
        "that cannot"
    )
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
    # Which MANAGERS fired, tracked by identity. A manager naming its dependency
    # with an inline (?<depName>) group records an owner that is not its own
    # template name, so comparing names reports a live manager as idle.
    matched_managers: set[int] = set()
    unmanaged = []
    covered: list[tuple[str, str, str]] = []

    for f, name, value, why in pins:
        line = f"{name}: {value}"
        text = f.read_text(encoding="utf-8")
        owner = None
        for mgr_idx, (mgr_name, rx, files) in enumerate(managers):
            if not manages(files, f):
                continue
            for m in rx.finditer(text):
                gd = m.groupdict()
                if gd.get("currentValue") != value:
                    continue
                # An inline (?<depName>) group names the dependency where no
                # depNameTemplate does.
                owner = gd.get("depName") or (mgr_name if mgr_name != "<unnamed>" else name)
                matched_managers.add(mgr_idx)
                break
            if owner:
                break
        if owner:
            covered.append((str(f.relative_to(ROOT)), line, owner))
        else:
            unmanaged.append((str(f.relative_to(ROOT)), name, value, why))

    # A manager matching nothing is the same silent nothing as no manager.
    idle = [n for i, (n, _rx, _f) in enumerate(managers) if i not in matched_managers]

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
            "on its own; it cannot see these. Add a customManager to renovate.json whose\n"
            "managerFilePatterns reach the file, or move the pin somewhere natively managed.",
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

    manifests = workspace_manifests()
    selecting, splits = lockfile_splits(json.loads(RENOVATE.read_text(encoding="utf-8")), manifests)
    if splits:
        problems = True
        print(f"\n{len(splits)} packageRule(s) take lock file maintenance for a workspace package.json:\n", file=sys.stderr)
        for label, hit in splits:
            print(f"  - packageRules{label}: {', '.join(hit)}", file=sys.stderr)
        print(
            "\npnpm-lock.yaml belongs to the whole workspace, so a rule that selects one package.json\n"
            "and takes lockFileMaintenance refreshes every package's pins on its own branch and\n"
            "schedule, beside the workspace-wide refresh. Give the rule matchUpdateTypes that\n"
            "leave lockFileMaintenance out.",
            file=sys.stderr,
        )

    if problems:
        return 1

    print(f"✓ {len(covered)} version pins outside a natively-parsed manifest, every one Renovate-managed")
    print(
        f"✓ {selecting} packageRule(s) select one of the {len(manifests)} workspace package.json files "
        "by name, and none takes lock file maintenance"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
