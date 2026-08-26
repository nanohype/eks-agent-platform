#!/usr/bin/env python3
"""Every repo-relative path named in markdown must exist.

Prose that names a thing is a claim about the world. A path in a README, a
runbook or an ADR is the most checkable claim documentation makes, and the one
that rots fastest: a file moves, the reference does not, and nothing fails —
the reader following it is the failure detector, at the worst possible moment
if the document is a runbook.

This is the "named things resolve" rule made executable. It cannot judge whether
prose is TRUE, but it can prove the nouns exist, which is the half a machine can
hold.

WHAT COUNTS AS A NAMED PATH

  * A markdown link target that is not a URL, an anchor, a mailto, a sibling
    repository, or prose carrying regex metacharacters.
  * A backticked token containing a slash whose first segment is one of THIS
    repository's top-level directories.

The second rule is deliberately narrow, and the narrowness is the design. A
permissive detector reported 172 candidates here, almost all of them npm scopes
(`@eks-agent/core`), branch prefixes (`feat/`), SSM parameter paths and bare
filenames whose directory the prose left implicit — and a gate whose output is
mostly noise is a gate people stop reading, which is a slower way of not having
one. Keying on the repo's real top-level directories separates a claim about
this tree from a token that merely looks like a path.

The failure direction here is a false ALARM rather than a false pass, which is
why the exclusions are permitted to be broad: a missed reference is a reference a
reader still has to check, while a noisy gate costs the whole check.

SELF-EXEMPTION, STATED RATHER THAN DISCOVERED

This file's own docstring quotes path-shaped examples in order to explain
itself, so it would flag itself. A gate's documentation is made of the shapes it
hunts — that collision is structural, not incidental, so scripts/ is excluded
from the markdown scan by construction (it holds no markdown) and this docstring
names the reason rather than leaving a reader to wonder why the gate never
reports on the directory it lives in.

    scripts/check-named-paths.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

SKIP_DIRS = {".git", "node_modules", ".terraform", ".terragrunt-cache", "dist", ".turbo", "bin"}

# [text](target)
MD_LINK = re.compile(r"\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")

# `token` — backticked, one token, no spaces.
BACKTICK = re.compile(r"`([^`\n]+)`")

# The repository's own top-level directories. A backticked token counts as a
# claim about THIS repo only when it starts with one of these — which is what
# separates `charts/tenant/values.yaml` from an npm scope (`@eks-agent/core`), a
# branch prefix (`feat/`), an SSM path (`/eks-agent-platform/...`) and a bare
# filename whose directory the prose left implicit (`main.tf`).
#
# Derived from the tree rather than listed, so a new top-level directory is
# covered without editing this file — and an empty result fails below, because a
# derivation that stops deriving is the silent-absence failure again.
def repo_roots() -> set[str]:
    return {
        p.name
        for p in ROOT.iterdir()
        if p.is_dir() and p.name not in SKIP_DIRS and not p.name.startswith(".")
    }


# Paths into a SIBLING repository. Real references, unresolvable from inside one
# worktree, and not this gate's to judge — naming them keeps them out of the
# findings without pretending they were checked.
SIBLING_REPOS = ("landing-zone/", "eks-gitops/", "cloudgov/", "kx/", "rackctl/", "nanohype/")


def looks_like_a_path(tok: str, roots: set[str]) -> bool:
    """Is this backticked token claiming to be a file in THIS repo?

    Deliberately conservative. The cost of a false alarm is a gate people stop
    reading, and the shapes that are path-LIKE but not repo paths outnumber the
    real ones in this repo's prose several times over.
    """
    t = tok.strip()
    if not t or " " in t or "/" not in t:
        return False
    if t.startswith(("http://", "https://", "#", "mailto:", "//", "@", "/", "-")):
        return False
    if t.startswith(SIBLING_REPOS) or t.startswith("../"):
        return False
    if any(c in t for c in "*{}$<>|:"):
        return False
    head = t.lstrip("./").split("/", 1)[0]
    return head in roots


def markdown_files() -> list[pathlib.Path]:
    out = []
    for p in ROOT.rglob("*.md"):
        if any(part in SKIP_DIRS for part in p.relative_to(ROOT).parts):
            continue
        out.append(p)
    if not out:
        sys.exit(
            "check-named-paths: no markdown found under the repository root. Finding no broken "
            "references and reading no documents produce the same silence, so this is an error."
        )
    return sorted(out)


def resolve(doc: pathlib.Path, target: str) -> bool:
    """Does target exist, relative to the document and to the repo root?"""
    t = target.split("#", 1)[0].split("?", 1)[0]
    if not t:
        return True  # a bare anchor — not a path claim
    if (doc.parent / t).exists():
        return True
    return (ROOT / t.lstrip("/")).exists()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print every document scanned and its path count")
    args = ap.parse_args()

    roots = repo_roots()
    if not roots:
        sys.exit("check-named-paths: no top-level directories found; the root derivation is broken")
    docs = markdown_files()
    broken: list[tuple[str, int, str, str]] = []
    checked = 0

    for doc in docs:
        rel = str(doc.relative_to(ROOT))
        text = doc.read_text(encoding="utf-8", errors="replace")
        per_doc = 0
        for lineno, line in enumerate(text.split("\n"), start=1):
            for target in MD_LINK.findall(line):
                if target.startswith(("http://", "https://", "#", "mailto:")):
                    continue
                if target.startswith(SIBLING_REPOS) or target.startswith("../"):
                    continue
                # A target carrying regex metacharacters is prose that happens to
                # contain `](`, not a link. The generated CRD reference documents
                # kubebuilder patterns in table cells and produces exactly that.
                if any(c in target for c in "\\{}[]|^$*+"):
                    continue
                checked += 1
                per_doc += 1
                if not resolve(doc, target):
                    broken.append((rel, lineno, target, "markdown link"))
            for tok in BACKTICK.findall(line):
                if not looks_like_a_path(tok, roots):
                    continue
                checked += 1
                per_doc += 1
                if not resolve(doc, tok):
                    broken.append((rel, lineno, tok, "backticked path"))
        if args.list and per_doc:
            print(f"  {rel}: {per_doc} path reference(s)")

    if checked == 0:
        print(
            "check-named-paths: scanned "
            f"{len(docs)} document(s) and extracted NO path references. The extractors have stopped "
            "matching the shapes they describe; that is a broken check, not clean prose.",
            file=sys.stderr,
        )
        return 1

    if broken:
        print(f"\n{len(broken)} named path(s) that do not resolve:\n", file=sys.stderr)
        for rel, lineno, target, kind in broken:
            print(f"  - {rel}:{lineno}  {target}  ({kind})", file=sys.stderr)
        print(
            "\nProse that names a path is a claim about the world. A reader following one of these "
            "finds nothing, and in a runbook that happens during an incident.",
            file=sys.stderr,
        )
        return 1

    print(f"✓ {checked} named paths across {len(docs)} markdown documents all resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
