#!/usr/bin/env python3
"""The tenant chart ships the per-environment deltas the contract requires.

platform-tenant-contract lists `<app>/chart/values-{dev,staging,production}.yaml`
among the required artifacts and says so again in its do_not list: every chart
has three deltas even if some are empty.

The reason the empty ones still have to exist is the one that gets argued away.
Three files that ALWAYS exist mean a deploy path can name one unconditionally.
A file that exists only when it has content makes every consumer branch on its
absence — and the branch nobody wrote is the one that silently deploys base
values into production, which is the failure this rule prevents and the failure
that produces no error when it happens.

Also asserted: no per-env delta hardcodes an AWS account id, a region, or a KMS
key ARN. The same contract forbids those in chart values because per-env values
plumb them from landing-zone outputs at deploy time, and a delta file is exactly
where someone reaches for a literal.

    scripts/check-tenant-chart-artifacts.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "tenant"
ENVIRONMENTS = ("dev", "staging", "production")

# Values that belong to one real estate and must reach the chart from
# landing-zone outputs instead of being written down here.
ESTATE_LITERALS = (
    (re.compile(r"\b\d{12}\b"), "a 12-digit AWS account id"),
    (re.compile(r"\b[a-z]{2}-[a-z]+-\d\b"), "an AWS region"),
    (re.compile(r"\barn:aws[a-z-]*:kms:"), "a KMS key ARN"),
)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print each required artifact and its size")
    args = ap.parse_args()

    if not CHART.is_dir():
        sys.exit(f"check-tenant-chart-artifacts: {CHART.relative_to(ROOT)} does not exist")

    problems: list[str] = []
    checked = 0

    for env in ENVIRONMENTS:
        f = CHART / f"values-{env}.yaml"
        checked += 1
        if not f.is_file():
            problems.append(
                f"{f.relative_to(ROOT)} is missing. Empty is a legitimate delta; absent is not — "
                "a consumer that names it unconditionally breaks, and one that branches on its "
                "absence deploys base values into that environment without saying so."
            )
            continue
        text = f.read_text(encoding="utf-8")
        if args.list:
            values = [ln for ln in text.split("\n") if ln.strip() and not ln.lstrip().startswith("#")]
            print(f"  {f.relative_to(ROOT)}: {len(values)} value line(s)")
        for pattern, what in ESTATE_LITERALS:
            for m in pattern.finditer(text):
                line = text[: m.start()].count("\n") + 1
                problems.append(
                    f"{f.relative_to(ROOT)}:{line} hardcodes {what} ({m.group(0)!r}). Per-env values "
                    "plumb these from landing-zone outputs at deploy time; a literal here is one "
                    "estate's value presented as the chart's shape."
                )

    if checked != len(ENVIRONMENTS):
        print("check-tenant-chart-artifacts: examined no environments; the list is empty", file=sys.stderr)
        return 1

    if problems:
        print(f"\n{len(problems)} tenant-chart artifact problem(s):\n", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        return 1

    print(f"✓ tenant chart ships all {checked} per-env deltas, none carrying an estate literal")
    return 0


if __name__ == "__main__":
    sys.exit(main())
