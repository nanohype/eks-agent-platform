#!/usr/bin/env python3
"""The RouteAPI vocabulary is spelled in three places and no type system spans them.

A ModelGateway route publishes a wire format on `status.routes[].api`. The
operator forwards `string(route.API)` verbatim; the eval-runner branches on it to
choose a request body; `@eks-agent/core` re-declares it as a zod enum for every
other TypeScript consumer. Three declarations of one closed set, in two
languages, with a JSON boundary between them.

Nothing holds them together on its own. A value added to the Go enum and missing
from the TypeScript union is not a compile error anywhere: the runner falls
through to its other branch and posts an OpenAI body at an Anthropic endpoint,
against a gateway that reports healthy and a route whose status looks correct.
The failure is a model returning nonsense, several layers from the edit.

Three sources, so three ways to drift:

  * The generated CRD enum (`config/crd/bases/...modelgateways.yaml`) is the
    authority, because it is what the API server admits. Compared against the
    Go marker rather than trusting it: the marker only matters once
    controller-gen has run.
  * The eval-runner's `RouteAPI` union decides which request body is sent.
  * `@eks-agent/core`'s zod enum validates CRs read back out of the cluster,
    and zod strips what it does not model — so a missing member is silent
    there too.

Case is load-bearing. The Go values are capitalised (`Anthropic`, `OpenAI`) and a
lowercase union would match nothing the operator can send, so this compares
exact bytes rather than normalising.

    scripts/check-route-api-parity.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _source_text import strip_comments  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent

CRD = ROOT / "operators" / "config" / "crd" / "bases" / "agents.nanohype.dev_modelgateways.yaml"
GO_TYPES = ROOT / "operators" / "api" / "agents" / "v1alpha1" / "modelgateway_types.go"
RUNNER = ROOT / "packages" / "eval-runner" / "src" / "model.ts"
CORE = ROOT / "packages" / "core" / "src" / "schemas.ts"

# +kubebuilder:validation:Enum=Anthropic;OpenAI on the RouteAPI type.
GO_ENUM = re.compile(r"^//\s*\+kubebuilder:validation:Enum=(\S+)\s*$\s*^type RouteAPI string", re.M)

# The rendered `api:` property's enum members. controller-gen emits the enum as a
# YAML sequence under the property, so anchor on the property name and take the
# `- <value>` lines that follow before the next key at the same indent.
CRD_API_BLOCK = re.compile(r"^(\s+)api:\n(?:\1\s+.*\n)*?\1\s+enum:\n((?:\1\s+- \S+\n)+)", re.M)
CRD_MEMBER = re.compile(r"-\s*(\S+)")

# export type RouteAPI = 'Anthropic' | 'OpenAI';
TS_UNION = re.compile(r"export type RouteAPI\s*=\s*([^;]+);")

# export const RouteAPI = z.enum(['Anthropic', 'OpenAI']);
ZOD_ENUM = re.compile(r"export const RouteAPI\s*=\s*z\.enum\(\[([^\]]+)\]\)")

QUOTED = re.compile(r"['\"]([^'\"]+)['\"]")


def read(path: pathlib.Path, language: str) -> str:
    """Read a source file with its comments blanked out.

    A commented-out declaration above the real one wins a `.search()`, so a
    matcher over raw text reports the set the comment names. Verified by the
    positive control in scripts/check-controls.py: a commented-out RouteAPI
    union placed above the live one must fail this gate, and against raw text it
    did not — it silently reported the comment's members.

    The Go marker is the exception and is read from RAW text: a kubebuilder
    validation marker IS a comment, and blanking it would delete the thing being
    checked.
    """
    if not path.is_file():
        sys.exit(f"check-route-api-parity: {path.relative_to(ROOT)} is missing; this gate cannot compare anything")
    return strip_comments(path.read_text(encoding="utf-8"), language)


def read_raw(path: pathlib.Path) -> str:
    if not path.is_file():
        sys.exit(f"check-route-api-parity: {path.relative_to(ROOT)} is missing; this gate cannot compare anything")
    return path.read_text(encoding="utf-8")


def go_marker_members() -> list[str]:
    m = GO_ENUM.search(read_raw(GO_TYPES))
    if not m:
        sys.exit(
            "check-route-api-parity: no +kubebuilder:validation:Enum marker on `type RouteAPI string` in "
            f"{GO_TYPES.relative_to(ROOT)} — without it the API server admits any string on a route's api field"
        )
    return [v for v in m.group(1).split(";") if v]


def crd_members() -> list[str]:
    blocks = CRD_API_BLOCK.findall(read(CRD, "yaml"))
    if not blocks:
        sys.exit(
            f"check-route-api-parity: no enum rendered for an `api:` property in {CRD.relative_to(ROOT)}. "
            "Run `make manifests` in operators/ — a marker that has not been generated constrains nothing."
        )
    sets = {tuple(CRD_MEMBER.findall(body)) for _indent, body in blocks}
    if len(sets) != 1:
        sys.exit(
            "check-route-api-parity: the CRD renders more than one distinct enum for an `api:` property "
            f"({sorted(sets)}); spec.routes[].api and status.routes[].api must admit the same set"
        )
    return list(next(iter(sets)))


def quoted_members(text: str, pattern: re.Pattern[str], path: pathlib.Path, what: str) -> list[str]:
    m = pattern.search(text)
    if not m:
        sys.exit(f"check-route-api-parity: no {what} found in {path.relative_to(ROOT)}")
    return QUOTED.findall(m.group(1))


def main() -> int:
    # Strict on purpose — see scripts/check-gates.py. A gate that ignores argv
    # cannot tell a renamed flag from a correct one.
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print each declaration and the members it admits")
    args = ap.parse_args()

    sources = {
        f"go marker      ({GO_TYPES.relative_to(ROOT)})": go_marker_members(),
        f"generated CRD  ({CRD.relative_to(ROOT)})": crd_members(),
        f"eval-runner    ({RUNNER.relative_to(ROOT)})": quoted_members(
            read(RUNNER, "ts"), TS_UNION, RUNNER, "`export type RouteAPI` union"
        ),
        f"core zod enum  ({CORE.relative_to(ROOT)})": quoted_members(
            read(CORE, "ts"), ZOD_ENUM, CORE, "`export const RouteAPI = z.enum([...])`"
        ),
    }

    if args.list:
        for label, members in sources.items():
            print(f"  {label}: {', '.join(members)}")

    # Order is presentation, membership is the contract — a union may list the
    # values in any order without changing what it admits.
    distinct = {frozenset(m) for m in sources.values()}
    if len(distinct) != 1:
        print("RouteAPI has drifted across its declarations:\n", file=sys.stderr)
        for label, members in sources.items():
            print(f"  {label}: {', '.join(sorted(members))}", file=sys.stderr)
        print(
            "\nEvery declaration must admit the same set, byte for byte. The operator forwards the CRD's\n"
            "value verbatim, so a member the TypeScript side is missing makes the runner take its other\n"
            "branch — an OpenAI body posted at an Anthropic endpoint, against a gateway reporting healthy.\n"
            "A member TypeScript has and the CRD does not is dead code that admission will never produce.",
            file=sys.stderr,
        )
        return 1

    empty = [label for label, members in sources.items() if not members]
    if empty:
        print(f"RouteAPI resolved to an EMPTY set in: {', '.join(empty)}", file=sys.stderr)
        print("Four empty sets compare equal, which is a pass this gate must not report.", file=sys.stderr)
        return 1

    print(f"✓ RouteAPI agrees across {len(sources)} declarations: {', '.join(sorted(next(iter(distinct))))}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
