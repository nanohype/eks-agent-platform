#!/usr/bin/env python3
"""A chart whose packaged content changes must move its version.

An OCI tag is mutable. Pushing `operator` 0.6.3 a second time replaces the bytes
every cluster already resolved, so a chart edited without a version bump gets
swapped underneath running clusters with nothing anywhere to say so. The release
job refuses to do that — for each chart the published version is absent
(publish), byte-identical (skip), or different (fail).

That guard is correct and it fires in the wrong place. It runs on a `charts-v`
tag, which is *after* the merge, so the defect does not present as "this PR may
not merge" — it presents as "every future chart release is blocked," discovered
by whoever next tries to ship something unrelated. It has happened twice:
`charts/tenant` at 0.4.0, and then `charts/operator` at 0.6.3, where the
`values.schema.json` gate and a README correcting an IRSA claim to Pod Identity
both landed under a version that was already published without them. For the
whole time in between, the schema existed in source and in no artifact, while
eks-gitops pinned 0.6.3 and every cluster resolved the copy that had none.

So this asks the same question at the merge boundary, where the answer can still
prevent the commit.

Two modes, split by whether the answer is a function of the commit:

  default      offline, blocking. Package each chart at the merge base and at
               HEAD and compare. Content moved and version did not -> fail.
               Needs no registry, no credentials, no network.

  --published  live, scheduled. Package each chart at HEAD and compare against
               what the registry actually serves at the declared version. This
               catches drift that is already on main — which the merge-base
               comparison structurally cannot see, because a PR that does not
               touch a drifted chart is not wrong about it.

Both compare **packaged** trees, not the working tree. `helm package` reorders
Chart.yaml's keys and drops its comments, so a comment-only edit changes the
source and changes nothing that ships; requiring a bump for one would be wrong,
and the self-test holds that line with a case that must stay green.

Usage:
    scripts/check-chart-version-bump.py                  # against origin/main
    scripts/check-chart-version-bump.py --base <ref>
    scripts/check-chart-version-bump.py --published
    scripts/check-chart-version-bump.py --self-test
"""

from __future__ import annotations

import pathlib
import re
import shutil
import subprocess
import argparse
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary
import tarfile
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
CHARTS = ROOT / "charts"
REGISTRY = "oci://ghcr.io/nanohype/eks-agent-platform/charts"

# `1.2.3`, `1.2.3-rc.1`, `1.2.3+build.5`. The numeric core orders; the suffixes
# only have to round-trip, since this gate never has to decide whether rc.2
# follows rc.1 — a suffix change with the same core still counts as a bump.
#
# helm itself rejects a non-semver `version` at package time, so in practice this
# always matches. It is not treated as "then it cannot happen": a version this
# gate cannot order is one it must refuse to judge, because the alternative is
# waving the chart through on a comparison it never made.
SEMVER = re.compile(r"^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$")



# tarfile's `filter=` argument — which refuses members that would escape the
# destination — arrives in Python 3.12. Passing it unconditionally makes this
# gate unrunnable on anything older, and a gate that cannot run is a gate that
# cannot reject. The extraction stays filtered wherever the runtime offers it;
# below 3.12 the members are checked directly rather than dropping the guard,
# because the guard is the point.
def _safe_extractall(tf, dest) -> None:
    import os
    import sys as _sys

    if _sys.version_info >= (3, 12):
        tf.extractall(dest, filter="data")
        return
    base = os.path.abspath(dest)
    for member in tf.getmembers():
        target = os.path.abspath(os.path.join(base, member.name))
        if not (target == base or target.startswith(base + os.sep)):
            raise RuntimeError(f"refusing tar member escaping the destination: {member.name!r}")
        if member.issym() or member.islnk():
            raise RuntimeError(f"refusing link member in a chart archive: {member.name!r}")
    tf.extractall(dest)  # noqa: S202 — members validated above


def run(*args: str, cwd: pathlib.Path | None = None) -> str:
    proc = subprocess.run(args, capture_output=True, text=True, cwd=cwd)
    if proc.returncode != 0:
        raise RuntimeError(f"`{' '.join(args)}` exited {proc.returncode}\n{proc.stderr.strip()}")
    return proc.stdout


def chart_version(chart_dir: pathlib.Path) -> str:
    """Read `version` the way helm does, so a malformed Chart.yaml fails here."""
    out = run("helm", "show", "chart", str(chart_dir))
    for line in out.splitlines():
        if line.startswith("version:"):
            return line.split(":", 1)[1].strip().strip("\"'")
    raise RuntimeError(f"{chart_dir.name}/Chart.yaml declares no version")


def packaged_tree(chart_dir: pathlib.Path, dest: pathlib.Path) -> pathlib.Path:
    """Package the chart and extract it — the bytes a consumer would receive."""
    dest.mkdir(parents=True, exist_ok=True)
    run("helm", "package", str(chart_dir), "-d", str(dest))
    tgzs = sorted(dest.glob("*.tgz"))
    if len(tgzs) != 1:
        raise RuntimeError(f"expected one .tgz in {dest}, found {len(tgzs)}")
    extracted = dest / "x"
    extracted.mkdir(exist_ok=True)
    with tarfile.open(tgzs[0]) as tf:
        _safe_extractall(tf, extracted)
    roots = [p for p in extracted.iterdir() if p.is_dir()]
    if len(roots) != 1:
        raise RuntimeError(f"expected one chart root in {extracted}, found {len(roots)}")
    return roots[0]


def tree_diff(left: pathlib.Path, right: pathlib.Path, lname: str, rname: str) -> list[str]:
    """Every path that differs between two extracted charts, by content."""
    lf = {p.relative_to(left): p for p in left.rglob("*") if p.is_file()}
    rf = {p.relative_to(right): p for p in right.rglob("*") if p.is_file()}
    out = []
    for rel in sorted(set(lf) | set(rf), key=str):
        if rel not in rf:
            out.append(f"only in {lname}: {rel}")
        elif rel not in lf:
            out.append(f"only in {rname}: {rel}")
        elif lf[rel].read_bytes() != rf[rel].read_bytes():
            out.append(f"content differs: {rel}")
    return out


def version_moved_forward(old: str, new: str) -> tuple[bool, str]:
    """A bump must go up. Republishing under an older version is the same hazard
    wearing a different hat — it overwrites bytes some cluster already has."""
    if old == new:
        return False, f"version is still {new}"
    mo, mn = SEMVER.match(old), SEMVER.match(new)
    if not mo or not mn:
        unparseable = old if not mo else new
        return False, f"version {unparseable!r} is not semver, so this gate cannot order it"
    if tuple(int(g) for g in mn.groups()[:3]) < tuple(int(g) for g in mo.groups()[:3]):
        return False, f"version moved BACKWARDS, {old} -> {new}"
    return True, f"{old} -> {new}"


def base_chart(ref: str, name: str, dest: pathlib.Path) -> pathlib.Path | None:
    """Materialize charts/<name> as it was at `ref`. None if it did not exist."""
    listed = subprocess.run(
        ["git", "ls-tree", "--name-only", ref, f"charts/{name}/"],
        capture_output=True, text=True, cwd=ROOT,
    )
    if listed.returncode != 0 or not listed.stdout.strip():
        return None
    dest.mkdir(parents=True, exist_ok=True)
    archive = subprocess.run(
        ["git", "archive", ref, "--", f"charts/{name}"],
        capture_output=True, cwd=ROOT,
    )
    if archive.returncode != 0:
        raise RuntimeError(f"git archive {ref} charts/{name} failed")
    tar_path = dest / "base.tar"
    tar_path.write_bytes(archive.stdout)
    with tarfile.open(tar_path) as tf:
        _safe_extractall(tf, dest)
    return dest / "charts" / name


def registry_chart(name: str, version: str, dest: pathlib.Path) -> pathlib.Path | None:
    """Pull the published chart. None if the registry has no such version."""
    dest.mkdir(parents=True, exist_ok=True)
    pull = subprocess.run(
        ["helm", "pull", f"{REGISTRY}/{name}", "--version", version, "-d", str(dest)],
        capture_output=True, text=True,
    )
    if pull.returncode != 0:
        err = pull.stderr
        if any(s in err for s in ("not found", "NAME_UNKNOWN", "MANIFEST_UNKNOWN", "denied")):
            return None
        # An unrecognized registry error must not read as "absent" — that is the
        # one path where this gate would report a clean bill of health for a
        # question it never got to ask.
        raise RuntimeError(f"could not read {name} {version} from the registry:\n{err.strip()}")
    tgzs = sorted(dest.glob("*.tgz"))
    extracted = dest / "x"
    extracted.mkdir(exist_ok=True)
    with tarfile.open(tgzs[0]) as tf:
        _safe_extractall(tf, extracted)
    return extracted / name


def check_against_base(base_ref: str, work: pathlib.Path) -> int:
    try:
        merge_base = run("git", "merge-base", base_ref, "HEAD", cwd=ROOT).strip()
    except RuntimeError as exc:
        print(f"FAIL  cannot resolve a merge base against {base_ref} — the comparison")
        print(f"      would have no left-hand side, so it would examine nothing.\n{exc}")
        return 1

    charts = sorted(p for p in CHARTS.iterdir() if (p / "Chart.yaml").is_file())
    if not charts:
        print(f"FAIL  no charts found under {CHARTS.relative_to(ROOT)} — nothing was examined.")
        return 1

    failures, checked, added = [], [], []
    for chart in charts:
        name = chart.name
        version = chart_version(chart)
        base_dir = base_chart(merge_base, name, work / f"base-{name}")
        if base_dir is None:
            added.append(f"{name} {version}")
            continue
        base_version = chart_version(base_dir)
        head_tree = packaged_tree(chart, work / f"head-pkg-{name}")
        base_tree = packaged_tree(base_dir, work / f"base-pkg-{name}")
        diffs = tree_diff(base_tree, head_tree, "base", "HEAD")
        if not diffs:
            checked.append(f"{name} {version} (packaged content unchanged)")
            continue
        moved, how = version_moved_forward(base_version, version)
        if moved:
            checked.append(f"{name} {how}, {len(diffs)} packaged file(s) changed")
        else:
            failures.append((name, version, how, diffs))

    if failures:
        print(f"FAIL  {len(failures)} chart(s) changed without a version bump:\n")
        for name, version, how, diffs in failures:
            print(f"  {name} — {how}")
            for d in diffs[:10]:
                print(f"      {d}")
            if len(diffs) > 10:
                print(f"      ... and {len(diffs) - 10} more")
            print(
                f"\n      Fix: bump charts/{name}/Chart.yaml. Publishing these bytes under "
                f"{version}\n      would replace what clusters already resolved at that version.\n"
            )
        return 1

    for line in checked + [f"{a} (new chart)" for a in added]:
        print(f"  ok  {line}")
    print(f"\nOK    {len(checked) + len(added)} chart(s) examined against {merge_base[:12]}; "
          f"every content change carries a version bump.")
    return 0


def check_against_registry(work: pathlib.Path) -> int:
    charts = sorted(p for p in CHARTS.iterdir() if (p / "Chart.yaml").is_file())
    if not charts:
        print(f"FAIL  no charts found under {CHARTS.relative_to(ROOT)} — nothing was examined.")
        return 1

    failures, lines = [], []
    for chart in charts:
        name = chart.name
        version = chart_version(chart)
        published = registry_chart(name, version, work / f"reg-{name}")
        if published is None:
            lines.append(f"{name} {version} — not yet published")
            continue
        head_tree = packaged_tree(chart, work / f"head-pkg-{name}")
        diffs = tree_diff(published, head_tree, "registry", "source")
        if diffs:
            failures.append((name, version, diffs))
        else:
            lines.append(f"{name} {version} — matches the registry")

    if failures:
        print(f"FAIL  {len(failures)} chart(s) differ from what the registry serves "
              f"at their declared version:\n")
        for name, version, diffs in failures:
            print(f"  {name} {version}")
            for d in diffs[:10]:
                print(f"      {d}")
            if len(diffs) > 10:
                print(f"      ... and {len(diffs) - 10} more")
            print(f"\n      The next charts-v tag will refuse to publish {name}. Bump it.\n")
        return 1

    for line in lines:
        print(f"  ok  {line}")
    print(f"\nOK    {len(lines)} chart(s) checked against the registry; source and published agree.")
    return 0


CHART_YAML = """apiVersion: v2
name: probe
description: a chart used only by this script's self-test
type: application
version: {version}
appVersion: "1.0.0"
{comment}"""


def _write_chart(root: pathlib.Path, spec: dict) -> pathlib.Path:
    d = root / "probe"
    (d / "templates").mkdir(parents=True, exist_ok=True)
    (d / "Chart.yaml").write_text(
        CHART_YAML.format(version=spec["version"], comment=spec.get("comment", ""))
    )
    (d / "values.yaml").write_text("replicas: 1\n")
    (d / "templates" / "cm.yaml").write_text(spec["body"])
    for rel, content in spec.get("extra", {}).items():
        (d / rel).write_text(content)
    return d


def self_test() -> int:
    """Seven cases. Three must stay GREEN — a gate that fails everything is not a
    gate. The comment-only case is what proves the comparison is of packaged bytes
    rather than of the working tree, and the added-file case is the shape the real
    defect took: values.schema.json appeared under an already-published version."""
    a = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"
    b = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"
    cases = [
        ("identical content, same version",
         {"version": "1.0.0", "body": a}, {"version": "1.0.0", "body": a}, False),
        ("template changed, same version",
         {"version": "1.0.0", "body": a}, {"version": "1.0.0", "body": b}, True),
        ("template changed, version bumped",
         {"version": "1.0.0", "body": a}, {"version": "1.0.1", "body": b}, False),
        ("Chart.yaml comment only, same version",
         {"version": "1.0.0", "body": a},
         {"version": "1.0.0", "body": a,
          "comment": "# helm drops this on package, so nothing that ships changed\n"}, False),
        ("template changed, version moved backwards",
         {"version": "1.0.1", "body": a}, {"version": "1.0.0", "body": b}, True),
        ("file added, same version",
         {"version": "1.0.0", "body": a},
         {"version": "1.0.0", "body": a, "extra": {"values.schema.json": '{"type":"object"}\n'}}, True),
        ("content changed, prerelease bump",
         {"version": "1.0.0", "body": a}, {"version": "1.0.1-rc.1", "body": b}, False),
    ]

    failures = []
    for label, old_spec, new_spec, want_fail in cases:
        ov, nv = old_spec["version"], new_spec["version"]
        with tempfile.TemporaryDirectory() as td:
            tmp = pathlib.Path(td)
            old = _write_chart(tmp / "old", old_spec)
            new = _write_chart(tmp / "new", new_spec)
            old_tree = packaged_tree(old, tmp / "oldpkg")
            new_tree = packaged_tree(new, tmp / "newpkg")
            diffs = tree_diff(old_tree, new_tree, "old", "new")
            moved, _ = version_moved_forward(ov, nv)
            got_fail = bool(diffs) and not moved
            mark = "ok  " if got_fail == want_fail else "BAD "
            state = "fails" if got_fail else "passes"
            print(f"  {mark}{label} -> {state}")
            if got_fail != want_fail:
                failures.append(f"{label}: expected {'fail' if want_fail else 'pass'}, got {state}")

    if failures:
        print(f"\nFAIL  self-test: {len(failures)} case(s) behaved wrongly")
        for f in failures:
            print(f"  - {f}")
        return 1
    greens = sum(1 for c in cases if not c[3])
    print(f"\nOK    self-test: all {len(cases)} cases behaved as specified "
          f"({greens} of them must stay green, and did).")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--self-test", action="store_true", help="prove the diff comparison can fail")
    ap.add_argument("--published", action="store_true", help="compare against the published chart index")
    ap.add_argument("--base", help="git ref to diff against")
    args = ap.parse_args()

    # This gate held its own inline check for helm, which named the tool but
    # exited 1 — the code a finding about the tree uses — and ran before
    # parse_args, so --help failed without a binary it does not need to print
    # usage. Both are the shared assertion's job: exit 3, after argument
    # handling, and git asserted too rather than left to raise from the first
    # call.
    require_binary("helm", "render the chart whose version this compares across refs")
    require_binary("git", "read the base ref this compares the working tree against")

    if args.self_test:
        return self_test()
    with tempfile.TemporaryDirectory() as td:
        work = pathlib.Path(td)
        if args.published:
            return check_against_registry(work)
        base = "origin/main"
        if args.base:
            base = args.base
        return check_against_base(base, work)


if __name__ == "__main__":
    sys.exit(main())
