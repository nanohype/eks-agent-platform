#!/usr/bin/env python3
"""Does a change touch anything the eval-runner image is built from?

The ci.yaml eval-runner-image job builds the image for both release platforms
under QEMU, smoke-tests it and scans it. A pull request that cannot change the
image skips that work, and this script makes the call: it lists the changed
paths that are image inputs, and its exit status carries the verdict the way
`git diff --quiet` does.

An image input is

  * a path under a workspace package the image bundles: eval-runner and every
    package it depends on, read from the pnpm workspace graph with the selector
    the Dockerfile builds with, so a new workspace dependency is in scope
    without an edit here;
  * a root file the build reads (ROOT_INPUTS), or a file that defines the job
    building the image (JOB_INPUTS).

The failure that matters is the quiet one. A scope narrower than the image lets
a breaking change through with nothing built, the job reports success, and the
break surfaces as a release that cannot publish. So a workspace graph that does
not resolve eval-runner is not an empty scope: it is exit 3, and the job fails.

Exit status:

  0  no changed path is an image input
  1  at least one is, so the image has to be built
  2  usage error
  3  the scope could not be decided: git or pnpm missing, a call to either
     that did not finish in time or could not be started, or the workspace
     graph did not resolve

--base defaults to HEAD and --head to the working tree, which then includes
untracked files, so with no arguments the answer covers uncommitted work. CI
passes the merge commit's first parent and the merge commit.

    scripts/check-eval-runner-image-scope.py [--base REV] [--head REV] [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary, run_tool  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The package the image bundles, selected with its dependencies. The same
# selector as packages/eval-runner/Dockerfile's build step: the scope is the set
# of packages that build compiles.
IMAGE_PACKAGE = "@eks-agent/eval-runner"
SELECTOR = f"{IMAGE_PACKAGE}..."
IMAGE_PACKAGE_DIR = "packages/eval-runner/"

# Root files the build stage reads beyond the package directories: pnpm installs
# from the manifest, lockfile and workspace file, and every package tsconfig
# extends tsconfig.base.json. Matched as whole paths, so a nested file of the
# same name, such as an example's package.json, is not one of them.
ROOT_INPUTS = (
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "tsconfig.base.json",
)

# The job that builds and checks the image: its workflow, the composite action
# it runs on each platform, and this decision. A trailing slash matches the
# directory.
JOB_INPUTS = (
    ".github/workflows/ci.yaml",
    ".github/actions/eval-runner-image-check/",
    "scripts/check-eval-runner-image-scope.py",
)


def matches(path: str, entry: str) -> bool:
    """An entry ending in "/" is a directory and matches what is under it; any
    other entry matches only that exact path."""
    return path.startswith(entry) if entry.endswith("/") else path == entry


def image_inputs(changed: list[str], packages: list[str]) -> list[str]:
    """The changed paths that are image inputs, in the order given."""
    scope = (*packages, *ROOT_INPUTS, *JOB_INPUTS)
    return [p for p in changed if any(matches(p, entry) for entry in scope)]


def package_dirs(listing: str) -> list[str] | None:
    """Repo-relative directories, each ending in "/", from `pnpm ls --json`
    output. None when the listing does not include eval-runner itself: pnpm
    prints nothing in a tree with no workspace, and a listing without the image's
    own package is not a scope."""
    try:
        listed = json.loads(listing)
    except json.JSONDecodeError:
        return None
    if not isinstance(listed, list):
        return None
    root = ROOT.resolve()
    dirs: list[str] = []
    for pkg in listed:
        try:
            rel = pathlib.Path(pkg["path"]).resolve().relative_to(root)
        except (KeyError, TypeError, ValueError):
            continue
        dirs.append(f"{rel.as_posix()}/")
    return dirs if IMAGE_PACKAGE_DIR in dirs else None


def workspace_package_dirs() -> list[str]:
    """eval-runner and every workspace package it depends on. Exits 3 if the
    graph does not resolve."""
    r = run_tool(["pnpm", "--filter", SELECTOR, "ls", "--depth", "-1", "--json"], timeout=120, cwd=ROOT)
    dirs = package_dirs(r.stdout) if r.returncode == 0 else None
    if dirs is None:
        print(
            f"check-eval-runner-image-scope: `pnpm --filter {SELECTOR} ls` did not resolve "
            f"{IMAGE_PACKAGE_DIR} (exit {r.returncode}), so the image's packages are unknown and "
            "no scope can be decided. An empty scope here would build nothing for every change.\n"
            f"  stdout: {r.stdout.strip()[:300]}\n  stderr: {r.stderr.strip()[:300]}",
            file=sys.stderr,
        )
        sys.exit(EXIT_CANNOT_EVALUATE)
    return dirs


def git_lines(*args: str) -> list[str]:
    r = run_tool(["git", *args], timeout=120, cwd=ROOT)
    if r.returncode != 0:
        print(
            f"check-eval-runner-image-scope: `git {' '.join(args)}` exited {r.returncode}: "
            f"{r.stderr.strip()[:300]}",
            file=sys.stderr,
        )
        sys.exit(EXIT_CANNOT_EVALUATE)
    return [ln for ln in r.stdout.splitlines() if ln]


def changed_paths(base: str, head: str | None) -> list[str]:
    # --no-renames reports a moved file as a deletion plus an addition. With
    # rename detection only the new path is listed, so a file moved out of an
    # image package would read as a change outside the image.
    if head is not None:
        return git_lines("diff", "--name-only", "--no-renames", base, head)
    tracked = git_lines("diff", "--name-only", "--no-renames", base)
    untracked = git_lines("ls-files", "--others", "--exclude-standard")
    return sorted(set(tracked) | set(untracked))


def self_test() -> int:
    """The matching rules against a fixed graph, the reading of pnpm's listing,
    and the exit a hung or unstartable tool call takes, without running git or
    pnpm."""
    packages = ["packages/eval-runner/", "packages/core/", "packages/pricing/"]
    cases = [
        ("packages/pricing/src/data/bedrock-pricing.json", True, "a file in a dependency package"),
        ("packages/eval-runner/Dockerfile", True, "the image's own package"),
        ("pnpm-lock.yaml", True, "a root build input"),
        (".github/actions/eval-runner-image-check/action.yml", True, "the job's composite action"),
        ("docs/runbook.md", False, "documentation"),
        ("packages/client/src/index.ts", False, "a workspace package the image does not bundle"),
        ("packages/eval-runner-extras/src/index.ts", False, "a sibling whose name extends a package's"),
        ("examples/blank-tenant/package.json", False, "a nested file named like a root input"),
    ]
    failures = []
    for path, want, what in cases:
        got = bool(image_inputs([path], packages))
        if got != want:
            verdict = "an image input" if got else "outside the image"
            failures.append(f"{path} ({what}) was classified {verdict}")
    if image_inputs([], packages):
        failures.append("an empty change was classified as touching the image")

    root = ROOT.resolve()
    listing = json.dumps(
        [{"name": n, "path": str(root / d)} for n, d in (
            (IMAGE_PACKAGE, "packages/eval-runner"),
            ("@eks-agent/pricing", "packages/pricing"),
        )]
    )
    if package_dirs(listing) != ["packages/eval-runner/", "packages/pricing/"]:
        failures.append(f"a resolved listing read as {package_dirs(listing)!r}")
    unresolved = {
        "no output, as pnpm prints in a tree with no workspace": "",
        "an empty listing": "[]",
        "a listing without eval-runner": json.dumps([{"path": str(root / "packages/pricing")}]),
    }
    for what, text in unresolved.items():
        if package_dirs(text) is not None:
            failures.append(f"{what} was read as a scope rather than as no answer")

    # A tool call that decides nothing must exit 3, never 1: CI reads 1 as "build".
    undecided = {
        "a call that outlives its timeout": ([sys.executable, "-c", "import time; time.sleep(30)"], 0.5),
        "a binary that cannot be started": ([str(ROOT / "scripts" / "no-such-tool")], 5),
    }
    for what, (cmd, timeout) in undecided.items():
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                run_tool(cmd, timeout)
        except SystemExit as e:
            if e.code != EXIT_CANNOT_EVALUATE:
                failures.append(f"{what} exited {e.code}, not {EXIT_CANNOT_EVALUATE}")
        except Exception as e:  # noqa: BLE001 - the escape itself is the finding
            failures.append(f"{what} raised {type(e).__name__}, which exits 1 and reads as a build")
        else:
            failures.append(f"{what} returned a result instead of exiting {EXIT_CANNOT_EVALUATE}")

    if failures:
        for f in failures:
            print(f"self-test: {f}", file=sys.stderr)
        return 1
    print(
        f"✓ check-eval-runner-image-scope self-test: {len(cases)} paths classified as expected, "
        f"{len(unresolved)} unresolved graphs refused, and {len(undecided)} undecided tool calls "
        f"exit {EXIT_CANNOT_EVALUATE}"
    )
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default="HEAD", help="revision the change is measured from (default HEAD)")
    ap.add_argument("--head", help="revision the change is measured to (default: the working tree)")
    ap.add_argument("--list", action="store_true", help="print the scope and every changed path's classification")
    ap.add_argument("--self-test", action="store_true", help="prove known paths classify as expected and an unresolved graph is refused")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    require_binary("git", "list the paths the change touches")
    require_binary("pnpm", "read the workspace packages the image bundles")

    packages = workspace_package_dirs()
    changed = changed_paths(args.base, args.head)
    inputs = image_inputs(changed, packages)

    if args.list:
        print(f"scope: workspace packages {', '.join(packages)}")
        print(f"scope: root inputs {', '.join(ROOT_INPUTS)}")
        print(f"scope: job inputs {', '.join(JOB_INPUTS)}")
    for p in changed:
        if p in inputs:
            print(f"  in scope: {p}")
        elif args.list:
            print(f"  out of scope: {p}")

    span = f"{args.base}..{args.head}" if args.head else f"{args.base} and the working tree"
    if inputs:
        print(f"build: {len(inputs)} of {len(changed)} path(s) changed between {span} are image inputs")
        return 1
    print(f"nothing to build: none of the {len(changed)} path(s) changed between {span} is an image input")
    return 0


if __name__ == "__main__":
    sys.exit(main())
