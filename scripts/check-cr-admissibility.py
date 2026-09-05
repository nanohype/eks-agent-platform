#!/usr/bin/env python3
"""Every custom resource this repository ships is one the API server accepts,
and the reading that decides it agrees with the API server.

WHY THIS EXISTS

`examples/` was read by nothing. It is the tree a person copies from, and it is
outside every gate here: check-chart-crd-parity.py walks chart renders,
check-crd-instantiation.py walks chart renders. An AgentFleet missing
`spec.agents[].image` shipped in a chart for exactly that reason — nothing
walked the shape a reader would copy — and the same omission has shipped in a
catalog elsewhere.

The second half is the harder one. A reading of "would this be accepted?" is
itself a claim, and three of them in this org disagreed: each was built from the
rules its author had met, so a custom resource was as validated as whichever
reading saw it. scripts/crd_admissibility.py is the one reading; this gate is
one of its callers, and what makes it more than an assertion is the corpus.

WHAT IT CHECKS

1. EXAMPLES. Every document under examples/ is admitted with no finding at all —
   not refused, and not admitted-then-pruned, because a property the API server
   drops is a line in a worked example that has never reached a cluster.

2. THE CORPUS. Each file under testdata/cr-admissibility/ declares what
   the API server does to it, and the walker must produce exactly that — every
   declared finding, and nothing besides. A case that fails for a second reason
   its author did not intend is a case that would pass on a coincidence.

3. THE HEADERS RESOLVE. A `refused` header must name a path the schema declares,
   and a `pruned` header must name one it does not while its parent does.
   Otherwise a mistyped header is a fixture that asserts nothing.

4. COVERAGE. Every rule these CustomResourceDefinitions make reachable is
   exercised by some fixture. The requirement is derived from the schemas, so a
   definition that grows its first list of a new kind makes this fail until a
   fixture exists — rather than waiting for someone to remember.

   The same derivation holds the gap open. These definitions declare CEL rules
   and the reading evaluates none, so at least one fixture must be a case the
   API server refuses and the reading says nothing about. The limit is then a
   file a control plane is required to reject, not a paragraph.

5. VACUITY. No CRDs, no examples, no fixtures, or a corpus with no refusal, no
   pruning or no admission, each fail. A reading that refuses everything agrees
   with every rejection in the corpus and is still wrong.

WHAT THIS GATE DOES NOT DO

The corpus sits at the repository root rather than beside this script because
scripts/ is copied whole into the gates-only tree check-empty-tree.py builds, and
a corpus living there is repository content in a tree that is meant to hold none
— which is enough to make another gate report success over it.

It does not run an API server. The corpus header is the shared claim, and
`operators/test/admissibility` is the half that puts every one of these files
through a real one — a refused case rejected naming that path, a pruned case
created with the path gone, an admitted case created. This gate and that suite
read the same headers from opposite sides, so neither the walker nor the corpus
can drift from the control plane without one of them failing.

Usage: ./scripts/check-cr-admissibility.py
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from crd_admissibility import (
    PRUNED,
    REFUSED,
    RULES,
    admissibility,
    cel_rules,
    load_crds,
    reachable_rules,
    schema_declares,
)

from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
CRD_DIR = ROOT / "charts" / "operator" / "crds"
EXAMPLES = ROOT / "examples"
CORPUS = ROOT / "testdata" / "cr-admissibility"

HEADER = re.compile(r"^#\s*admissibility:\s*(?P<rest>.+?)\s*$", re.MULTILINE)

# The API server refuses it and this reading cannot say why. A fixture declaring
# this is the CEL gap held open as a case: the reading must report NOTHING about
# it, and the conformance oracle must watch a real control plane refuse it.
CEL_REFUSED = "cel-refused"
INDEX = re.compile(r"\[\d+\]")


def declared(path: Path) -> list[tuple[str, str, str]] | None:
    """The (verdict, rule, path) triples a fixture declares, or None if it has none."""
    out: list[tuple[str, str, str]] = []
    for match in HEADER.finditer(path.read_text()):
        parts = match.group("rest").split()
        if parts == ["admitted"]:
            continue
        if len(parts) != 3 or parts[0] not in (REFUSED, PRUNED, CEL_REFUSED):
            raise ValueError(
                f"{path.name}: `# admissibility: {match.group('rest')}` is not "
                f"`admitted`, `{REFUSED} <rule> <path>`, `{PRUNED} <rule> <path>` "
                f"or `{CEL_REFUSED} <rule> <path>`"
            )
        if parts[0] == CEL_REFUSED and parts[1] in RULES:
            raise ValueError(
                f"{path.name}: `{CEL_REFUSED} {parts[1]}` names a rule the reading "
                "applies, so this is not a gap — declare it as a refusal the "
                "reading reports"
            )
        out.append((parts[0], parts[1], parts[2]))
    if not HEADER.search(path.read_text()):
        return None
    return out


def one_document(path: Path) -> dict:
    docs = [d for d in yaml.safe_load_all(path.read_text()) if d]
    if len(docs) != 1:
        raise ValueError(
            f"{path.name} holds {len(docs)} documents and a fixture declares the "
            "verdict on one — split it, or the header names an object nobody can "
            "tell apart from its neighbour"
        )
    return docs[0]


def check_header_paths(kind_schema, verdict: str, dotted: str, problems: list[str], where: str) -> None:
    """A header that names nothing is a header that can never be matched."""
    bare = INDEX.sub("", dotted)
    if verdict == REFUSED:
        if bare.startswith("metadata."):
            return  # scope is a property of the definition, not of the schema
        if not schema_declares(kind_schema, bare):
            problems.append(
                f"{where}: `{dotted}` is declared refused, and the schema declares no "
                "such property — a path the schema does not carry cannot be refused for "
                "being wrong, so this header names nothing"
            )
        return
    parent, _, leaf = bare.rpartition(".")
    if leaf and schema_declares(kind_schema, bare):
        problems.append(
            f"{where}: `{dotted}` is declared pruned, and the schema declares it — a "
            "property the schema carries is kept, so this header contradicts the CRD"
        )
    if parent and not schema_declares(kind_schema, parent):
        problems.append(
            f"{where}: `{dotted}` is declared pruned, and the schema does not carry its "
            f"parent `{parent}` either — pruning is reported against a described object, "
            "so the header names a place nothing is described"
        )


def main() -> int:
    problems: list[str] = []

    if not CRD_DIR.is_dir():
        print(f"FAIL  {CRD_DIR} does not exist")
        return 1
    crds = load_crds(CRD_DIR)
    if not crds:
        print(f"FAIL  no CustomResourceDefinition under {CRD_DIR.relative_to(ROOT)},")
        print("      so every document below would be compared against nothing.")
        return 1

    # 1. EXAMPLES
    example_files = sorted(EXAMPLES.rglob("*.yaml"))
    example_docs = 0
    for path in example_files:
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc:
                continue
            rel = path.relative_to(ROOT)
            try:
                findings = admissibility(doc, crds)
            except KeyError as exc:
                problems.append(f"{rel}: {exc}")
                continue
            example_docs += 1
            for finding in findings:
                name = (doc.get("metadata") or {}).get("name")
                problems.append(f"{rel}: {doc.get('kind')}/{name} {finding}")

    if not example_docs:
        print(f"FAIL  no custom resource under {EXAMPLES.relative_to(ROOT)}, so the")
        print("      class this gate was written for is unread again.")
        return 1

    # 2 & 3. THE CORPUS
    fixtures = sorted(CORPUS.glob("*.yaml"))
    if not fixtures:
        print(f"FAIL  no fixture under {CORPUS.relative_to(ROOT)}, so nothing holds the")
        print("      reading to what an API server actually does.")
        return 1

    covered: set[str] = set()
    verdicts: set[str] = set()
    for path in fixtures:
        try:
            expected = declared(path)
            doc = one_document(path)
        except ValueError as exc:
            problems.append(str(exc))
            continue
        if expected is None:
            problems.append(
                f"{path.name} carries no `# admissibility:` header, so neither this "
                "gate nor the conformance oracle knows what it is for"
            )
            continue
        for verdict in (REFUSED, PRUNED, CEL_REFUSED):
            if any(v == verdict for v, _, _ in expected):
                verdicts.add(verdict)
        if not expected:
            verdicts.add("admitted")
        covered |= {rule for v, rule, _ in expected if v != CEL_REFUSED}

        try:
            findings = admissibility(doc, crds)
        except KeyError as exc:
            problems.append(f"{path.name}: {exc}")
            continue

        schema = crds[doc["kind"]].schemas[doc["apiVersion"].partition("/")[2]]
        for verdict, _, dotted in expected:
            if verdict == CEL_REFUSED:
                # The rule is CEL's, but the path it names is still the schema's.
                check_header_paths(schema, REFUSED, dotted, problems, path.name)
                continue
            check_header_paths(schema, verdict, dotted, problems, path.name)

        if all(v == CEL_REFUSED for v, _, _ in expected) and expected:
            for finding in findings:
                problems.append(
                    f"{path.name} is declared a case only a CEL rule refuses, and the "
                    f"reading reports {finding} — either the reading now covers it, in "
                    "which case declare the refusal, or the fixture has a second defect"
                )
            continue

        actual = [(f.verdict, f.rule, f.path) for f in findings]
        missing = [e for e in expected if e not in actual]
        extra = [a for a in actual if a not in expected]
        for verdict, rule, dotted in missing:
            problems.append(
                f"{path.name} declares `{verdict} {rule} {dotted}` and the walker does "
                "not report it"
            )
        for finding in findings:
            if (finding.verdict, finding.rule, finding.path) in extra:
                problems.append(
                    f"{path.name} does not declare what the walker reports: {finding}"
                )

    # 4. COVERAGE
    reachable = reachable_rules(crds)
    for rule in sorted(reachable - covered):
        problems.append(
            f"these CustomResourceDefinitions make the `{rule}` rule reachable and no "
            "fixture exercises it — the reading applies a rule nothing has ever seen it "
            "apply"
        )

    # 4b. THE GAP IS MEASURED, NOT DESCRIBED
    declared_cel = cel_rules(crds)
    if declared_cel and CEL_REFUSED not in verdicts:
        problems.append(
            f"these CustomResourceDefinitions declare {declared_cel} CEL rule(s) and the "
            f"reading evaluates none — with no `{CEL_REFUSED}` fixture, the one limit "
            "this gate cannot close is a sentence in a docstring rather than a case a "
            "real API server is required to refuse"
        )

    # 5. VACUITY
    for verdict, why in (
        (REFUSED, "a reading that never refuses agrees with every example and is useless"),
        (PRUNED, "a reading that cannot tell dropped from rejected reports a field as working"),
        ("admitted", "a reading that refuses everything agrees with every rejection here"),
    ):
        if verdict not in verdicts:
            problems.append(f"no fixture is `{verdict}` — {why}")

    if problems:
        print("custom resources this repository ships, or the reading that admits them:\n", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        print(
            "\nTo fix: correct the resource, or — if the API server really does this — "
            "correct the fixture header and re-run operators/test/admissibility, which "
            "puts the same file through a real control plane.",
            file=sys.stderr,
        )
        return 1

    print(
        f"{example_docs} custom resource(s) under {EXAMPLES.relative_to(ROOT)} are "
        f"admitted whole, and {len(fixtures)} corpus fixture(s) get exactly the verdict "
        f"they declare across all {len(reachable)} rule(s) these CRDs make reachable:"
    )
    print(f"  rules: {', '.join(sorted(reachable))}")
    print(
        f"  not read: {declared_cel} CEL rule(s), held open by the `{CEL_REFUSED}` "
        "fixture(s) the conformance oracle watches a real API server refuse"
    )
    return 0


# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    # This gate takes no arguments; argparse rejects anything passed.
    return ap.parse_args()


if __name__ == "__main__":
    _parse_args()
    sys.exit(main())
