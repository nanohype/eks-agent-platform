#!/usr/bin/env python3
"""Every SSM parameter this repo publishes must have a consumer, and every
parameter it consumes must have a producer.

An SSM parameter is a contract between two things that never reference each
other. Nothing links them: the writer applies, the reader reads a string, and if
the two disagree — or if the reader was never built — both sides are green and
the feature is simply absent. The operator makes this worse in the direction
that matters, deliberately: it sweeps its whole subtree with
GetParametersByPath and ignores keys it does not recognise, so adding an output
is non-breaking. The cost is that publishing an output nobody reads is also
non-breaking, and looks identical.

That is not hypothetical. A component in this tree published a JSON catalog of
accelerator node pools, keyed for a CRD the operator does not define, alongside
two IAM role ARNs for consumers bound by EKS Pod Identity — which never need an
ARN. Three parameters read by nothing, indistinguishable from a working contract
for as long as anyone looked. The component has since been deleted along with
the addons it served; this check is what makes the next one visible.

So a published parameter has to name its consumer, and this checks that each one
does. A parameter is consumed when any of these holds:

  1. Some component in this repo reads it with `data "aws_ssm_parameter"`.
  2. It sits under /eks-agent-platform/<cluster>/, its relative key appears in
     the operator's Config.assign switch, AND the struct field that case arm
     assigns to is read somewhere outside the operatorconfig package.

     The last clause is the whole point. "Decoded" is not "consumed": a case arm
     writes a field, and a field nothing reads is a value that arrived and
     stopped. Six parameters passed this check on that technicality — three
     eval-runtime and kill-switch keys this repo writes, plus three
     model-artifacts keys landing-zone writes — while the code that needed them
     used hardcoded literals instead. Requiring a reader is what makes the rule
     mean what its name says.

     The operatorconfig package is excluded from the reader search on purpose.
     Its own test asserts every field is populated after a decode, which is a
     decode test and not a use; counting it would launder every dead field back
     into "consumed".
  3. The resource carries an `# ssm-consumer: <who>` comment, for a parameter
     published as a query surface rather than to code — the account's Athena
     handles, read by an analyst or a dashboard.

The marker in (3) is a claim, not a proof, and it is meant to be: it costs one
line to write and it puts the claim next to the resource, where the reviewer who
would have asked "consumed by what?" can see it. The accelerator-pool catalog
above would have needed one naming the CRD that reads it, and that is the
sentence somebody would have checked.

This checks one direction only — that what is written is read. The other
direction (that what is read is written) spans repos: landing-zone publishes
agent-iam, model-artifacts, observability and managed-monitoring into the same
tree, and a single-repo job cannot see it.

THE OTHER DIRECTION

Everything above walks the published set. That can only find a parameter written
with no reader; it can never find a READER WITH NO WRITER, because such a path
appears nowhere in the collection being walked. A check that iterates one
collection finds what is missing from the other, never what the walked
collection was never told about.

That gap was live when this was written. The operator reads

    /eks-agent-platform/<cluster>/<platform>/ses/domain

to bind a tenant's SES sending domain, and nothing in either landing-zone tree
writes it — verified with a positive control, not inferred from a quiet grep. A
Platform declaring the ses capability therefore gets no grant, reports Ready,
and the first symptom is a tenant workload failing to send mail. The reading
half was built and the writing half never was, and every check here ran from the
side that could not see it.

So the two sets are now compared rather than one being iterated.

WHAT COUNTS AS A CONSUMED PATH

Terraform `data "aws_ssm_parameter"` blocks, as before, and the operator's
specific-path reads. The latter are found by convention: a function whose name
ends in ParamPath returns an SSM parameter name, and there are three
(sesDomainParamPath, masterSecretParamPath, tenantKeyParamPath). The convention
is what makes them findable — a specific-path read built inline, without going
through such a function, is invisible to this check in exactly the way an
unmarked human consumer is. That is a limit worth knowing rather than a claim
this check does not make.

The operator's GetParametersByPath sweep is not a consumed path. It reads a
subtree and ignores what it does not recognise, so it names nothing.

A consumed path is satisfied when this repo publishes it, or when a
`// ssm-producer: <who>` comment above the builder names who does. The marker is
a claim, like its ssm-consumer counterpart, and it is meant to be: it costs one
line and it puts the claim next to the reader, where the reviewer who doubts it
is standing.

Usage: scripts/check-ssm-contract.py [--verbose]
"""

from __future__ import annotations

import sys as _sys
from pathlib import Path as _Path

_sys.path.insert(0, str(_Path(__file__).resolve().parent))
from _source_text import strip_comments  # noqa: E402

import argparse

import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
COMPONENTS = REPO / "terraform" / "components"
OPERATORS = REPO / "operators"
OPERATOR_CONFIG = OPERATORS / "internal" / "operatorconfig" / "config.go"

# The subtree the operator sweeps. A parameter under it is a candidate for rule
# (2); anything else — /eks-agent-platform/org/... — is not, because the sweep is
# rooted at the cluster name and never sees it.
CLUSTER_ROOT = "/eks-agent-platform/<var.cluster_name>/"


def normalize(expr: str, locals_map: dict[str, str]) -> str:
    """Resolve a name expression to a comparable shape.

    Interpolations of locals are substituted (there are only a handful, and all
    of them are plain string literals). Anything left — ${var.cluster_name} —
    keeps its identity rather than collapsing to a wildcard, so a parameter keyed
    by environment is never mistaken for one keyed by cluster.
    """
    value = expr
    for _ in range(5):  # locals can reference locals; bounded rather than recursive
        before = value

        def sub_local(m: re.Match[str]) -> str:
            return locals_map.get(m.group(1), m.group(0))

        value = re.sub(r"\$\{\s*local\.(\w+)\s*\}", sub_local, value)
        if value == before:
            break
    # Remaining interpolations keep their reference: ${var.x} -> <var.x>
    value = re.sub(r"\$\{\s*([\w.]+)\s*\}", r"<\1>", value)
    return value


def parse_locals(sources: list[str]) -> dict[str, str]:
    """Collect `key = "literal"` assignments from every locals block."""
    found: dict[str, str] = {}
    for src in sources:
        for block in re.finditer(r"\blocals\s*\{(.*?)\n\}", src, re.S):
            for m in re.finditer(r'^\s*(\w+)\s*=\s*"([^"\n]*)"\s*$', block.group(1), re.M):
                found[m.group(1)] = m.group(2)
    return found


HEADER = re.compile(r'(?P<kind>resource|data)\s+"aws_ssm_parameter"\s+"(?P<label>\w+)"\s*\{')
NAME = re.compile(r'\bname\s*=\s*"(?P<value>[^"\n]*)"')
CONSUMER = re.compile(r"#\s*ssm-consumer:\s*(?P<who>.+?)\s*$", re.M)
# The reader-side counterpart, written as a Go comment above the ParamPath
# builder: `// ssm-producer: landing-zone's agent-iam component`.
PRODUCER = re.compile(r"//\s*ssm-producer:\s*(?P<who>.+?)\s*$", re.M)
# A ParamPath builder returns the parameter name. Captured with its fmt template
# so the path can be compared against what terraform publishes.
PARAM_PATH_FN = re.compile(
    r"((?:^//[^\n]*\n)*)^func\s+(?P<fn>\w*ParamPath)\s*\([^)]*\)\s*string\s*\{"
    r"(?P<body>.*?)\n\}",
    re.M | re.S,
)
SSM_LITERAL = re.compile(r'"(?P<path>/eks-agent-platform/[^"\n]*)"')


def wildcard(name: str) -> str:
    """Collapse a path to the shape both sides can be compared in.

    Terraform names carry ${var.cluster_name}; Go templates carry %s. Neither
    resolves at check time and neither needs to: what matters is whether the
    same SLOTS in the same positions are published and consumed. Every
    substitution becomes *, so the two vocabularies meet.
    """
    out = re.sub(r"<[\w.]+>", "*", name)
    out = re.sub(r"%[sdv]", "*", out)
    return out.rstrip("/")


def block_body(src: str, open_brace: int) -> str:
    """Return the body of the block whose opening brace is at open_brace.

    Braces are counted outside double-quoted strings only, so a one-line block
    and an interpolation like "${local.prefix}/x" are both read correctly —
    the interpolation's closing brace lives inside a string and is not a block
    delimiter.
    """
    depth = 0
    in_string = False
    i = open_brace
    while i < len(src):
        c = src[i]
        if in_string:
            if c == "\\":
                i += 2
                continue
            if c == '"':
                in_string = False
        elif c == '"':
            in_string = True
        elif c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return src[open_brace + 1 : i]
        i += 1
    return src[open_brace + 1 :]


def leading_comments(src: str, start: int) -> str:
    """The contiguous run of comment lines immediately above a block.

    Exactly one newline is consumed, not every trailing blank: a comment
    separated from the resource by an empty line is a comment about something
    else, and must not be read as that resource's declaration.
    """
    head = src[:start].rstrip(" \t")
    if head.endswith("\n"):
        head = head[:-1]
    lines: list[str] = []
    while True:
        nl = head.rfind("\n")
        line = head[nl + 1 :].strip()
        if not line.startswith("#"):
            break
        lines.append(line)
        head = head[:nl] if nl >= 0 else ""
        if nl < 0:
            break
    return "\n".join(reversed(lines))


class Param:
    def __init__(self, component: str, path: Path, label: str, name: str, consumer: str | None):
        self.component = component
        self.file = path
        self.label = label
        self.name = name
        self.consumer = consumer

    def __repr__(self) -> str:
        return f"{self.component}:{self.label}"


def collect() -> tuple[list[Param], set[str]]:
    written: list[Param] = []
    read: set[str] = set()

    for component_dir in sorted(p for p in COMPONENTS.iterdir() if p.is_dir()):
        files = sorted(component_dir.glob("*.tf"))
        locals_map = parse_locals([strip_comments(f.read_text(), "hcl") for f in files])

        for tf in files:
            # TWO VIEWS OF ONE FILE, chosen by what each check is asking.
            #
            # `raw` carries the `# ssm-consumer:` annotations — a comment, and
            # this gate's own vocabulary for "another repo reads this one". A
            # stripped view erases them and every cross-repo parameter reports as
            # unread. Measured: stripping this read turned four correctly
            # annotated parameters into findings, including
            # kill-switch/state_machine_arn, which the telemetry-pipeline
            # standard names as a published discovery path.
            #
            # `src` is the same file with comments blanked, and the resource
            # bodies are read from it — because a commented-out `name =` is not a
            # published parameter, and counting it as one let a parameter nobody
            # writes read as written.
            raw = tf.read_text()
            src = strip_comments(raw, "hcl")
            for m in HEADER.finditer(src):
                body = block_body(src, m.end() - 1)
                raw_body = block_body(raw, m.end() - 1) if m.end() - 1 < len(raw) else body
                name_match = NAME.search(body)
                if not name_match:
                    # A name built by something other than a literal string. Fail
                    # loudly rather than skipping: a parameter this cannot read is
                    # a parameter it cannot check.
                    print(
                        f"ERROR {tf.relative_to(REPO)}: "
                        f'{m.group("kind")} "aws_ssm_parameter" "{m.group("label")}" '
                        "has no literal name; this check cannot resolve it",
                        file=sys.stderr,
                    )
                    sys.exit(2)
                resolved = normalize(name_match.group("value"), locals_map)

                if m.group("kind") == "data":
                    read.add(resolved)
                    continue

                # RAW: the annotation IS a comment, and it is what says another
                # repo reads this parameter. The resource body above is read from
                # the stripped view; this one line is the other half of the same
                # view-per-check split.
                declared = CONSUMER.search(leading_comments(raw, m.start()))
                written.append(
                    Param(
                        component_dir.name,
                        tf.relative_to(REPO),
                        m.group("label"),
                        resolved,
                        declared.group("who") if declared else None,
                    )
                )

    return written, read


class ConsumedPath:
    def __init__(self, fn: str, file: Path, path: str, producer: str | None):
        self.fn = fn
        self.file = file
        self.path = path
        self.producer = producer

    def __repr__(self) -> str:
        return f"{self.fn}"


def collect_consumed() -> list[ConsumedPath]:
    """The operator's specific-path SSM reads, by convention.

    A function named *ParamPath returns an SSM parameter name; its comment block
    may carry `// ssm-producer:` naming who writes it. The convention is the
    whole discovery mechanism, so if it finds none at all that is a parse
    failure rather than a clean tree — the same reasoning as the Config.assign
    arm-count guard below.
    """
    found: list[ConsumedPath] = []
    for path in sorted(OPERATORS.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        # RAW, deliberately. The `// ssm-producer:` marker this function reads IS
        # a comment — it is the gate's own vocabulary for "another repo writes
        # this one" — so a stripped view here erases the annotation and reports
        # every cross-repo parameter as unpublished. Measured: stripping dropped
        # producers from 22 to 19 and turned three correctly-annotated reads into
        # findings.
        #
        # The sibling sites below DO strip, and the difference is what each is
        # asking. A commented-out `case` arm is not a live assignment and a
        # commented-out field reference is not a use; a commented-out annotation
        # is still the annotation.
        src = path.read_text()
        for m in PARAM_PATH_FN.finditer(src):
            lit = SSM_LITERAL.search(m.group("body"))
            if not lit:
                print(
                    f"ERROR {path.relative_to(REPO)}: {m.group('fn')} returns no "
                    "/eks-agent-platform literal; this check cannot resolve what it reads",
                    file=sys.stderr,
                )
                sys.exit(2)
            declared = PRODUCER.search(m.group(1) or "")
            found.append(
                ConsumedPath(
                    m.group("fn"),
                    path.relative_to(REPO),
                    lit.group("path"),
                    declared.group("who") if declared else None,
                )
            )
    if not found:
        print(
            "ERROR: found no *ParamPath builders under operators/; the convention this "
            "check discovers consumers by is not matching, and every reader would look "
            "satisfied",
            file=sys.stderr,
        )
        sys.exit(2)
    return found


def operator_keys() -> dict[str, str]:
    """{relative key: the Config field its case arm assigns}."""
    src = strip_comments(OPERATOR_CONFIG.read_text(), "go")
    body = re.search(r"func \(c \*Config\) assign\(.*?\n\}", src, re.S)
    if not body:
        print(f"ERROR: cannot find Config.assign in {OPERATOR_CONFIG}", file=sys.stderr)
        sys.exit(2)
    arms = re.findall(r'case\s+"([^"]+)":\s*\n\s*c\.(\w+)\s*=', body.group(0))
    keys = dict(arms)
    if len(keys) < 10:
        print(
            f"ERROR: found only {len(keys)} case arms in Config.assign; the parse is wrong "
            "and every operator-consumed parameter would look like an orphan",
            file=sys.stderr,
        )
        sys.exit(2)
    return keys


def live_fields() -> set[str]:
    """Config fields read somewhere outside the operatorconfig package.

    A field assigned by a case arm and referenced nowhere else is a value the
    operator decodes and drops. Its own package is excluded: load_test.go
    asserts each field is populated after a decode, which proves the decode and
    nothing about consumption.
    """
    live: set[str] = set()
    for path in sorted(OPERATORS.rglob("*.go")):
        if OPERATOR_CONFIG.parent in path.parents:
            continue
        for m in re.finditer(r"\.([A-Z]\w*)\b", strip_comments(path.read_text(), "go")):
            live.add(m.group(1))
    if not live:
        print(
            f"ERROR: found no field references anywhere under {OPERATORS}; the walk "
            "is not seeing the tree and every field would look dead",
            file=sys.stderr,
        )
        sys.exit(2)
    return live


def main() -> int:
    verbose = "--verbose" in sys.argv

    written, read = collect()
    keys = operator_keys()
    live = live_fields()
    consumed_paths = collect_consumed()

    # The set comparison. Published shapes come from what terraform writes;
    # consumed shapes from what the operator reads by name. Both are collapsed
    # to their slot shape so ${var.cluster_name} and %s meet.
    published_shapes = {wildcard(p.name) for p in written}
    unproduced: list[ConsumedPath] = []
    declared_elsewhere: list[ConsumedPath] = []
    for c in consumed_paths:
        if wildcard(c.path) in published_shapes:
            continue
        if c.producer:
            declared_elsewhere.append(c)
        else:
            unproduced.append(c)

    # A field decoded from a parameter and read by nothing. Reported separately
    # from orphan parameters because the remedy differs: the parameter may have
    # a legitimate non-code consumer, while the field never does.
    dead_fields = sorted(
        (key, field) for key, field in keys.items() if field not in live
    )

    if not written:
        print("ERROR: no aws_ssm_parameter resources found; the walk is not seeing the tree", file=sys.stderr)
        return 2

    orphans: list[Param] = []
    consumed: list[tuple[Param, str]] = []

    for p in written:
        if p.name in read:
            consumed.append((p, "read by a component in this repo"))
        elif (
            p.name.startswith(CLUSTER_ROOT)
            and keys.get(p.name[len(CLUSTER_ROOT):]) in live
        ):
            field = keys[p.name[len(CLUSTER_ROOT):]]
            consumed.append((p, f"decoded into Config.{field}, which is read by the operator"))
        elif p.consumer:
            consumed.append((p, f"declared: {p.consumer}"))
        else:
            orphans.append(p)

    if verbose:
        for p, why in consumed:
            print(f"  ok  {p.name}\n        {p} — {why}")

    print(f"{len(written)} parameters published, {len(consumed)} consumed, {len(orphans)} unaccounted for")

    if dead_fields:
        print()
        print("These parameters are decoded into a Config field that nothing reads:")
        print()
        for key, field in dead_fields:
            print(f"  {CLUSTER_ROOT}{key}")
            print(f"      assigned to Config.{field}, referenced nowhere outside operatorconfig")
        print()
        print("Decoding is not consuming. Either wire the field to the code that needs it —")
        print("the usual finding is a literal hardcoded where the field should have been — or")
        print("delete the field, the case arm, and the parameter together.")
        print()

    if verbose:
        for c in declared_elsewhere:
            print(f"  ok  {c.path}\n        {c} — produced by: {c.producer}")

    print(
        f"{len(consumed_paths)} parameters consumed by name, "
        f"{len(consumed_paths) - len(unproduced)} produced, {len(unproduced)} unaccounted for"
    )

    if unproduced:
        print()
        print("These parameters are read and nothing writes them:")
        print()
        for c in unproduced:
            print(f"  {c.path}")
            print(f"      read by {c.file}  ({c.fn})")
        print()
        print("A reader with no writer is the same broken contract as a writer with no")
        print("reader, and it fails the other way round: the read returns nothing, the")
        print("caller treats absent as 'not configured', and the feature is silently off")
        print("while everything reports healthy.")
        print()
        print("If another repo publishes it — landing-zone owns the account-level")
        print("identities — say so above the builder:")
        print()
        print("      // ssm-producer: landing-zone's agent-iam component")
        print("      func tenantKeyParamPath(clusterName, platform string) string {")
        print()
        print("If nothing publishes it anywhere, that is the finding: either build the")
        print("producer or drop the read, but do not leave a capability whose grant")
        print("depends on a parameter no one writes.")
        return 1

    if orphans:
        print()
        print("These parameters are written and nothing reads them:")
        print()
        for p in orphans:
            print(f"  {p.name}")
            print(f"      written by {p.file}  ({p.label})")
        print()
        print("Each one needs a consumer or a deletion. If it is genuinely published for")
        print("a human — an Athena handle an analyst queries, say — put a line above the")
        print("resource naming who reads it:")
        print()
        print("      # ssm-consumer: the account's Athena query surface, read by hand")
        print("      resource \"aws_ssm_parameter\" \"reconciliation_view\" {")
        print()
        print("A parameter with no reader and no such line is a contract with one side.")
        return 1

    if dead_fields:
        return 1
    return 0



# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--verbose', action='store_true', help='print every parameter compared, not only the mismatches')
    return ap.parse_args()


if __name__ == "__main__":
    _parse_args()
    sys.exit(main())
