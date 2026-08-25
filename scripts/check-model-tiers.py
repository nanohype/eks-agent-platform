#!/usr/bin/env python3
"""The scaffolder's model tiers must equal the org LLM policy's, exactly.

`operators/internal/agentctl/model_defaults.json` is the single source of truth
for the model ids every scaffolded Platform gets — `agentctl tenant init` and
`agentctl platform new` both read it, and its values propagate into the tenant
chart, the examples and the CRD reference. Its own `$comment` says the tiers
name the org LLM-policy models.

That was a comment, and a comment claiming to mirror a standard is a claim
nothing keeps true. It had drifted a full generation: sonnet-4-6 where the
policy names sonnet-5, opus-4-8 where it names opus-5. Nothing failed, because
nothing was comparing them — and the failure mode of a stale model default is
not an error, it is a fleet quietly running last generation's model while the
file that set it says otherwise.

WHY THE COMPARISON IS EXACT

Not "close enough" or "same family". The policy's `inference-profile-required`
rule turns on the precise string: every current Claude model must be invoked
through a cross-region inference-profile id carrying a geo prefix, because
Bedrock reports no ON_DEMAND path for the family and refuses a bare
foundation-model id with a ValidationException on the first call. A tier that is
almost right fails at runtime, on the first request, in the tenant's account.

THE STANDARD IS READ, NOT RESTATED

The policy is resolved from the nanohype catalog rather than copied here. A gate
carrying its own copy of the rule is a second source of truth, and the whole
defect being closed is a second source of truth drifting from the first.

Resolution order, most authoritative first:
  1. $NANOHYPE_STANDARDS_DIR/llm-policy.json
  2. the sibling nanohype checkout, if one is beside this repo
  3. a cached copy under /private/tmp, written by whoever last pulled it

When none resolve the gate SKIPS with a stated reason and a zero exit — it
cannot assert a comparison against a document it does not have, and inventing
the expected values here would be exactly the copy this file exists to avoid.
The skip prints, so an environment where it never runs is visible rather than
silently green.

    scripts/check-model-tiers.py [--list]
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DEFAULTS = ROOT / "operators" / "internal" / "agentctl" / "model_defaults.json"


def candidate_policy_paths() -> list[pathlib.Path]:
    out = []
    env = os.environ.get("NANOHYPE_STANDARDS_DIR")
    if env:
        out.append(pathlib.Path(env) / "llm-policy.json")
    out.append(ROOT.parent / "nanohype" / "standards" / "llm-policy.json")
    out.append(pathlib.Path.home() / "codes" / "nanohype" / "nanohype" / "standards" / "llm-policy.json")
    out.append(pathlib.Path("/private/tmp/claude-501/qc-standards/llm-policy.json"))
    return out


def load_policy() -> dict[str, str] | None:
    for p in candidate_policy_paths():
        if p.is_file():
            try:
                return json.loads(p.read_text(encoding="utf-8"))["content"]["models"]
            except (KeyError, json.JSONDecodeError) as e:
                sys.exit(f"check-model-tiers: {p} is not a readable llm-policy document: {e}")
    return None


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print both tier tables side by side")
    args = ap.parse_args()

    if not DEFAULTS.is_file():
        sys.exit(f"check-model-tiers: {DEFAULTS.relative_to(ROOT)} is missing")
    tiers = json.loads(DEFAULTS.read_text(encoding="utf-8")).get("tiers", {})
    if not tiers:
        print("check-model-tiers: model_defaults.json declares no tiers at all", file=sys.stderr)
        return 1

    policy = load_policy()
    if policy is None:
        # Loud skip. Silence here would be indistinguishable from a pass.
        print(
            "check-model-tiers: SKIPPED — no llm-policy.json resolved from "
            f"{len(candidate_policy_paths())} candidate locations, so the comparison asserts nothing "
            "on this run. Set NANOHYPE_STANDARDS_DIR to the catalog's standards directory."
        )
        print(f"  scaffolder tiers, unverified: {json.dumps(tiers, sort_keys=True)}")
        return 0

    if args.list:
        for tier in sorted(set(tiers) | set(policy)):
            print(f"  {tier:12} policy={policy.get(tier, '<absent>')}  scaffolder={tiers.get(tier, '<absent>')}")

    drift = []
    for tier in sorted(set(tiers) | set(policy)):
        want, got = policy.get(tier), tiers.get(tier)
        if want != got:
            drift.append((tier, want, got))

    if drift:
        print(f"\n{len(drift)} model tier(s) disagree with the org LLM policy:\n", file=sys.stderr)
        for tier, want, got in drift:
            print(f"  - {tier}: policy names {want!r}, model_defaults.json has {got!r}", file=sys.stderr)
        print(
            "\nThe comparison is exact by design. llm-policy's inference-profile-required rule turns on "
            "the precise string — a bare or mis-generationed id is refused by Bedrock with a "
            "ValidationException on the first call, in the tenant's account.",
            file=sys.stderr,
        )
        return 1

    print(f"✓ {len(tiers)} model tiers match the org LLM policy exactly")
    return 0


if __name__ == "__main__":
    sys.exit(main())
