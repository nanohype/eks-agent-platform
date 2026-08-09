#!/usr/bin/env python3
"""Every environment must make the same decisions about a component.

WHY THIS EXISTS

`terraform/live/production-platform/agent-egress` set `create_vpc_endpoints =
false` with a comment explaining that landing-zone's network component already
owns the VPC's endpoints. The development and staging leaves of the same
component said nothing, so they took the variable's default of `true` and would
have asked AWS for a second S3 gateway endpoint on route tables that already
carry one — which AWS rejects, aborting the apply partway through a seven-root
chain, on a cluster that is already built and running.

The value was not the problem. The SILENCE was. Somebody worked out, once, that
this VPC's endpoints belong to landing-zone, wrote it down in one leaf, and the
other two kept a default that had never been considered. Nothing could see the
divergence: each leaf is individually valid HCL, each renders, each plans, and
the variable has a perfectly good default.

WHAT THIS CHECKS

For every component with a leaf under more than one `terraform/live/*-platform/`
environment, the SET OF INPUT KEYS must match. Not the values — environments are
supposed to differ, and `enable_waf = true` in production against `false` in
development is the system working. What may not differ is whether the question
was asked at all.

So a key present in one environment's leaf and absent from another fails, naming
both. The remedy is never "delete the key": it is to write the same key in the
other leaf with whatever value that environment wants. A default is a fine
answer once it is a stated one.

    scripts/check-leaf-input-parity.py            # fail on the first divergence
    scripts/check-leaf-input-parity.py --list     # print what it parsed, then check
    scripts/check-leaf-input-parity.py --self-test
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LIVE = ROOT / "terraform" / "live"

# `org` is deliberately excluded: it is the account-scoped tier and has exactly
# one leaf per component by design, so there is nothing to compare it against.
# Restricting to the *-platform environments keeps the rule to the tier where
# "the same component, three times" is the actual shape.
ENV_GLOB = "*-platform"

# A component whose leaves legitimately carry different keys, with the reason.
# Empty, and worth keeping empty: an unargued entry turns this gate into a list
# of things it has agreed not to look at.
EXEMPT: dict[str, str] = {}

INPUTS_START = re.compile(r"^\s*inputs\s*=\s*\{\s*$")
# A top-level assignment inside the inputs block. Anchored on the key so a value
# that happens to contain '=' (a policy document, a URL) cannot be mistaken for
# one, and depth-guarded by the caller so a nested object's keys are not read as
# the leaf's own.
ASSIGN = re.compile(r"^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=")


def input_keys(text: str) -> set[str]:
    """The top-level keys of a terragrunt leaf's `inputs` block.

    Brace-depth tracking rather than a flat regex: a nested object's keys are
    that object's, not the leaf's, and counting them would make two leaves look
    different for a reason that is not a divergence. Braces inside strings and
    comments are ignored for the same reason — a heredoc with a `{` in it must
    not shift the depth for everything after it.
    """
    keys: set[str] = set()
    depth = 0
    inside = False
    for raw in text.splitlines():
        line = raw.split("#", 1)[0]
        if not inside:
            if INPUTS_START.match(line):
                inside = True
                depth = 1
            continue
        if depth == 1:
            m = ASSIGN.match(line)
            if m:
                keys.add(m.group(1))
        # Count braces after reading the key, so `foo = {` is recorded as a key
        # at depth 1 and its contents are read at depth 2.
        stripped = re.sub(r'"(?:[^"\\]|\\.)*"', "", line)
        depth += stripped.count("{") + stripped.count("[")
        depth -= stripped.count("}") + stripped.count("]")
        if depth <= 0:
            inside = False
    return keys


def collect() -> dict[str, dict[str, set[str]]]:
    """component -> environment -> input keys."""
    out: dict[str, dict[str, set[str]]] = {}
    for env_dir in sorted(LIVE.glob(ENV_GLOB)):
        if not env_dir.is_dir():
            continue
        for leaf in sorted(env_dir.iterdir()):
            hcl = leaf / "terragrunt.hcl"
            if not hcl.is_file():
                continue
            out.setdefault(leaf.name, {})[env_dir.name] = input_keys(hcl.read_text())
    return out


def check(listing: bool) -> int:
    table = collect()
    if not table:
        print(f"error: no leaves found under {LIVE}/{ENV_GLOB}", file=sys.stderr)
        return 2

    failures = 0
    for component, by_env in sorted(table.items()):
        if len(by_env) < 2:
            continue
        if component in EXEMPT:
            print(f"exempt: {component} — {EXEMPT[component]}")
            continue

        union: set[str] = set().union(*by_env.values())
        if listing:
            print(f"{component}: {', '.join(sorted(union)) or '<no inputs>'}")

        for key in sorted(union):
            missing = sorted(env for env, keys in by_env.items() if key not in keys)
            if not missing:
                continue
            present = sorted(env for env, keys in by_env.items() if key in keys)
            failures += 1
            print(
                f"\n{component}: `{key}` is set in {', '.join(present)} "
                f"but not in {', '.join(missing)}.",
                file=sys.stderr,
            )
            for env in missing:
                print(
                    f"  terraform/live/{env}/{component}/terragrunt.hcl",
                    file=sys.stderr,
                )
            print(
                "  One environment decided this and the others kept a default nobody "
                "considered.\n"
                "  Write the key in the missing leaves — any value, including the "
                "default — so the\n"
                "  decision is stated rather than inherited.",
                file=sys.stderr,
            )

    if failures:
        print(f"\n{failures} input(s) set in some environments and not others", file=sys.stderr)
        return 1

    compared = sum(1 for by_env in table.values() if len(by_env) >= 2)
    print(f"\nok: {compared} component(s) agree on their input keys across environments")
    return 0


def self_test() -> int:
    """The parser has to be wrong in a visible way, not a silent one.

    A gate that reads zero keys out of every leaf passes every repository. These
    cases pin the two properties that make the comparison mean anything: nested
    keys are not counted as the leaf's own, and a brace inside a string does not
    shift the depth for the rest of the file.
    """
    cases = [
        ("flat", 'inputs = {\n  a = 1\n  b = "x"\n}\n', {"a", "b"}),
        (
            "nested object keys are not the leaf's",
            'inputs = {\n  a = 1\n  tags = {\n    Name = "x"\n    Env  = "y"\n  }\n  b = 2\n}\n',
            {"a", "tags", "b"},
        ),
        (
            "list of objects",
            'inputs = {\n  a = 1\n  rules = [\n    { name = "r" },\n  ]\n}\n',
            {"a", "rules"},
        ),
        (
            "brace inside a string does not shift depth",
            'inputs = {\n  a = "${lookup(x, \\"k\\")}"\n  b = 2\n}\n',
            {"a", "b"},
        ),
        (
            "comment is not a key",
            "inputs = {\n  # ignored = 1\n  a = 1\n}\n",
            {"a"},
        ),
        ("no inputs block", 'terraform {\n  source = "x"\n}\n', set()),
    ]
    bad = 0
    for name, text, want in cases:
        got = input_keys(text)
        status = "ok " if got == want else "FAIL"
        if got != want:
            bad += 1
        print(f"{status} {name}: {sorted(got)}")
        if got != want:
            print(f"     wanted {sorted(want)}", file=sys.stderr)
    return 1 if bad else 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print what was parsed")
    ap.add_argument("--self-test", action="store_true", help="check the parser, not the repo")
    args = ap.parse_args()
    if args.self_test:
        return self_test()
    return check(args.list)


if __name__ == "__main__":
    sys.exit(main())
