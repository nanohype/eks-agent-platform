#!/usr/bin/env python3
"""Does a change touch anything a released image is built from?

ci.yaml builds each image release.yaml publishes (eval-runner, operator,
sandbox-worker) for both release platforms from the pull request and scans it,
and smoke-tests eval-runner on each. A pull request that cannot change an image
skips that image's work, and this script makes the call: it lists the changed
paths that are an image's inputs, and its exit status carries the verdict the
way `git diff --quiet` does.

An image input is

  * for eval-runner, a path under a workspace package the image bundles:
    eval-runner and every package it depends on, read from the pnpm workspace
    graph with the selector the Dockerfile builds with, so a new workspace
    dependency is in scope without an edit here; and a root file the build
    reads (ROOT_INPUTS);
  * for operator and sandbox-worker, a path under the image's build context.
    The whole directory counts, including what its .dockerignore drops.
    Reading those rules here would be a second implementation of BuildKit's,
    and where the two disagree, the narrow side lets a change through
    unbuilt while the wide side only builds an image for nothing;
  * for every image, a file that defines the job building it: the workflow,
    the composite actions it runs, and this decision (JOB_INPUTS).

The failure that matters is the quiet one. A scope narrower than the image lets
a breaking change through with nothing built, the job reports success, and the
break surfaces as a release that cannot publish. So a workspace graph that does
not resolve eval-runner, or a build context with no Dockerfile in it, is not an
empty scope: it is exit 3, and the job fails.

--image decides one image, as each ci.yaml job does. Without it every image is
decided, and the exit status says whether any of them has to be built.

Exit status:

  0  no changed path is an input of the images decided
  1  at least one is, so an image has to be built
  2  usage error
  3  the scope could not be decided: git or pnpm missing, a call to either
     that did not finish in time or could not be started, a workspace graph
     that did not resolve, or a build context with no Dockerfile

--base defaults to HEAD and --head to the working tree, which then includes
untracked files, so with no arguments the answer covers uncommitted work. CI
passes the merge commit's first parent and the merge commit.

    scripts/check-image-scope.py [--image NAME] [--base REV] [--head REV] [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import contextlib
import dataclasses
import io
import json
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary, run_tool  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent


@dataclasses.dataclass(frozen=True)
class Image:
    """One image ci.yaml builds and release.yaml publishes.

    Exactly one of `context` and `selector` decides which package paths are
    inputs: a build context is a directory whose every file is one, and a
    selector names the workspace packages a build at the repository root
    compiles. `inputs` are further paths, beside JOB_INPUTS, that count.
    """

    name: str
    dockerfile: str
    context: str | None = None
    selector: str | None = None
    package_dir: str | None = None
    inputs: tuple[str, ...] = ()


# Root files the eval-runner build stage reads beyond the package directories:
# pnpm installs from the manifest, lockfile and workspace file, and every
# package tsconfig extends tsconfig.base.json. Matched as whole paths, so a
# nested file of the same name, such as an example's package.json, is not one.
ROOT_INPUTS = (
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "tsconfig.base.json",
)

IMAGES = {
    image.name: image
    for image in (
        Image(
            name="eval-runner",
            dockerfile="packages/eval-runner/Dockerfile",
            # The same selector as packages/eval-runner/Dockerfile's build step:
            # the scope is the set of packages that build compiles.
            selector="@eks-agent/eval-runner...",
            package_dir="packages/eval-runner/",
            inputs=(*ROOT_INPUTS, ".github/actions/eval-runner-image-check/"),
        ),
        Image(name="operator", dockerfile="operators/Dockerfile", context="operators/"),
        Image(name="sandbox-worker", dockerfile="sandbox-worker/Dockerfile", context="sandbox-worker/"),
    )
}

# The files that define every image job: the workflow, the scan each job runs on
# every platform, and this decision. A trailing slash matches the directory.
JOB_INPUTS = (
    ".github/workflows/ci.yaml",
    ".github/actions/image-scan/",
    "scripts/check-image-scope.py",
)


def matches(path: str, entry: str) -> bool:
    """An entry ending in "/" is a directory and matches what is under it; any
    other entry matches only that exact path."""
    return path.startswith(entry) if entry.endswith("/") else path == entry


def image_inputs(changed: list[str], scope: tuple[str, ...]) -> list[str]:
    """The changed paths that are inputs of an image with this scope, in the
    order given."""
    return [p for p in changed if any(matches(p, entry) for entry in scope)]


def full_scope(image: Image, packages: list[str]) -> tuple[str, ...]:
    """Every entry a changed path is matched against for this image."""
    return (*packages, *image.inputs, *JOB_INPUTS)


def package_dirs(listing: str, package_dir: str) -> list[str] | None:
    """Repo-relative directories, each ending in "/", from `pnpm ls --json`
    output. None when the listing does not include the image's own package:
    pnpm prints nothing in a tree with no workspace, and a listing without that
    package is not a scope."""
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
    return dirs if package_dir in dirs else None


def resolve_packages(image: Image) -> list[str]:
    """The package directories whose every path is an input of this image.
    Exits 3 if they cannot be decided."""
    if image.context is not None:
        dockerfile = ROOT / image.dockerfile
        if not dockerfile.is_file():
            print(
                f"check-image-scope: {image.name} builds {image.dockerfile} from {image.context}, "
                "and that Dockerfile is not in this tree, so the build context is not the image's "
                "and no scope can be decided.",
                file=sys.stderr,
            )
            sys.exit(EXIT_CANNOT_EVALUATE)
        return [image.context]

    r = run_tool(["pnpm", "--filter", image.selector, "ls", "--depth", "-1", "--json"], timeout=120, cwd=ROOT)
    dirs = package_dirs(r.stdout, image.package_dir) if r.returncode == 0 else None
    if dirs is None:
        print(
            f"check-image-scope: `pnpm --filter {image.selector} ls` did not resolve "
            f"{image.package_dir} (exit {r.returncode}), so the {image.name} image's packages are "
            "unknown and no scope can be decided. An empty scope here would build nothing for "
            "every change.\n"
            f"  stdout: {r.stdout.strip()[:300]}\n  stderr: {r.stderr.strip()[:300]}",
            file=sys.stderr,
        )
        sys.exit(EXIT_CANNOT_EVALUATE)
    return dirs


def git_lines(*args: str) -> list[str]:
    r = run_tool(["git", *args], timeout=120, cwd=ROOT)
    if r.returncode != 0:
        print(
            f"check-image-scope: `git {' '.join(args)}` exited {r.returncode}: {r.stderr.strip()[:300]}",
            file=sys.stderr,
        )
        sys.exit(EXIT_CANNOT_EVALUATE)
    return [ln for ln in r.stdout.splitlines() if ln]


def changed_paths(base: str, head: str | None) -> list[str]:
    # --no-renames reports a moved file as a deletion plus an addition. With
    # rename detection only the new path is listed, so a file moved out of an
    # image's inputs would read as a change outside them.
    if head is not None:
        return git_lines("diff", "--name-only", "--no-renames", base, head)
    tracked = git_lines("diff", "--name-only", "--no-renames", base)
    untracked = git_lines("ls-files", "--others", "--exclude-standard")
    return sorted(set(tracked) | set(untracked))


def self_test() -> int:
    """The matching rules against a fixed graph, the reading of pnpm's listing,
    and the exit a hung or unstartable tool call takes, without running git or
    pnpm."""
    failures = []

    # The packages each image would resolve to: a fixed workspace graph for
    # eval-runner, and the build context for the others.
    graph = {"eval-runner": ["packages/eval-runner/", "packages/core/", "packages/pricing/"]}

    def fixture_scope(image: Image) -> tuple[str, ...]:
        return full_scope(image, graph[image.name] if image.selector else [image.context])

    everything = set(IMAGES)
    # (path, the images it is an input of, what it is)
    cases = [
        ("packages/pricing/src/data/bedrock-pricing.json", {"eval-runner"}, "a file in an eval-runner dependency package"),
        ("packages/eval-runner/Dockerfile", {"eval-runner"}, "eval-runner's own package"),
        ("pnpm-lock.yaml", {"eval-runner"}, "a root input of the workspace build"),
        (".github/actions/eval-runner-image-check/action.yml", {"eval-runner"}, "the eval-runner job's check"),
        ("operators/internal/controller/platform_controller.go", {"operator"}, "operator source"),
        ("operators/internal/controller/platform_controller_test.go", {"operator"}, "a file the operator .dockerignore drops"),
        ("sandbox-worker/Dockerfile", {"sandbox-worker"}, "the sandbox-worker build"),
        ("sandbox-worker/.trivyignore.yaml", {"sandbox-worker"}, "the sandbox-worker scan's acknowledgements"),
        (".github/actions/image-scan/action.yml", everything, "the scan every image job runs"),
        (".github/workflows/ci.yaml", everything, "the workflow defining every image job"),
        ("scripts/check-image-scope.py", everything, "this decision"),
        ("docs/runbook.md", set(), "documentation"),
        ("packages/client/src/index.ts", set(), "a workspace package no image bundles"),
        ("packages/eval-runner-extras/src/index.ts", set(), "a sibling whose name extends a package's"),
        ("operators-extras/main.go", set(), "a sibling whose name extends a build context's"),
        ("examples/blank-tenant/package.json", set(), "a nested file named like a root input"),
        (".github/workflows/release.yaml", set(), "the release workflow, which no pull-request job runs"),
    ]
    for path, want, what in cases:
        got = {name for name, image in IMAGES.items() if image_inputs([path], fixture_scope(image))}
        if got != want:
            failures.append(f"{path} ({what}) is an input of {sorted(got)}, want {sorted(want)}")
    for name, image in IMAGES.items():
        if image_inputs([], fixture_scope(image)):
            failures.append(f"an empty change was classified as touching {name}")
        if (image.context is None) == (image.selector is None):
            failures.append(f"{name} names its packages by {'both' if image.context else 'neither'} a context and a selector")
        if image.context is not None and not image.dockerfile.startswith(image.context):
            failures.append(f"{name} builds {image.dockerfile}, which is outside its context {image.context}")

    eval_runner = IMAGES["eval-runner"]
    root = ROOT.resolve()
    listing = json.dumps(
        [{"name": n, "path": str(root / d)} for n, d in (
            ("@eks-agent/eval-runner", "packages/eval-runner"),
            ("@eks-agent/pricing", "packages/pricing"),
        )]
    )
    if package_dirs(listing, eval_runner.package_dir) != ["packages/eval-runner/", "packages/pricing/"]:
        failures.append(f"a resolved listing read as {package_dirs(listing, eval_runner.package_dir)!r}")
    unresolved = {
        "no output, as pnpm prints in a tree with no workspace": "",
        "an empty listing": "[]",
        "a listing without eval-runner": json.dumps([{"path": str(root / "packages/pricing")}]),
    }
    for what, text in unresolved.items():
        if package_dirs(text, eval_runner.package_dir) is not None:
            failures.append(f"{what} was read as a scope rather than as no answer")

    # A build context with no Dockerfile in it is not the image's context, so
    # it decides nothing rather than scoping every change out.
    elsewhere = Image(name="fixture", dockerfile="no-such-context/Dockerfile", context="no-such-context/")
    try:
        with contextlib.redirect_stderr(io.StringIO()):
            got = resolve_packages(elsewhere)
    except SystemExit as e:
        if e.code != EXIT_CANNOT_EVALUATE:
            failures.append(f"a context with no Dockerfile exited {e.code}, not {EXIT_CANNOT_EVALUATE}")
    else:
        failures.append(f"a context with no Dockerfile resolved to {got!r} instead of exiting {EXIT_CANNOT_EVALUATE}")

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
        f"✓ check-image-scope self-test: {len(cases)} paths classified against {len(IMAGES)} images "
        f"as expected, {len(unresolved)} unresolved graphs and a context with no Dockerfile refused, "
        f"and {len(undecided)} undecided tool calls exit {EXIT_CANNOT_EVALUATE}"
    )
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--image", choices=sorted(IMAGES), help="decide this image alone (default: every image)")
    ap.add_argument("--base", default="HEAD", help="revision the change is measured from (default HEAD)")
    ap.add_argument("--head", help="revision the change is measured to (default: the working tree)")
    ap.add_argument("--list", action="store_true", help="print each scope and every changed path's classification")
    ap.add_argument("--self-test", action="store_true", help="prove known paths classify as expected and an unresolved graph is refused")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    images = [IMAGES[args.image]] if args.image else list(IMAGES.values())
    require_binary("git", "list the paths the change touches")
    if any(image.selector for image in images):
        require_binary("pnpm", "read the workspace packages the eval-runner image bundles")

    scopes = {image.name: full_scope(image, resolve_packages(image)) for image in images}
    changed = changed_paths(args.base, args.head)
    span = f"{args.base}..{args.head}" if args.head else f"{args.base} and the working tree"

    build = False
    for image in images:
        scope = scopes[image.name]
        inputs = image_inputs(changed, scope)
        print(f"{image.name}:")
        if args.list:
            print(f"  scope: {', '.join(scope)}")
        for p in changed:
            if p in inputs:
                print(f"  in scope: {p}")
            elif args.list:
                print(f"  out of scope: {p}")
        if inputs:
            build = True
            print(f"  build: {len(inputs)} of {len(changed)} path(s) changed between {span} are inputs")
        else:
            print(f"  nothing to build: none of the {len(changed)} path(s) changed between {span} is an input")
    return 1 if build else 0


if __name__ == "__main__":
    sys.exit(main())
