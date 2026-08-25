#!/usr/bin/env python3
"""Positive controls: every gate is proven to reject by introducing its violation.

A gate that has only been READ is a gate being trusted. Reading finds the
defects you already have words for, and the ones that matter are the ones you do
not — this repo shipped a parity gate that matched over raw source, so a
commented-out declaration above the live one won the search and the gate
reported the comment's members. It read correctly. It was blind.

So each gate here carries a control that makes the exact edit the gate exists to
catch, runs the gate, and requires a non-zero exit. Three properties make that
mean something:

  ANTI-VACUITY. A gate with no control fails this run, and a control naming a
  gate that no longer exists fails it too. The suite cannot quietly shrink to
  the gates someone remembered — which is the same silent-absence failure the
  gates themselves are written against.

  CLEAN BEFORE MUTATING. Every control asserts the gate passes on the unmodified
  tree first. Without that a non-zero exit proves nothing: the gate might have
  been failing all along for an unrelated reason, and the control would report a
  pass while measuring something else entirely.

  RESTORE ALWAYS. Mutations are applied to real files and reverted in a finally
  block, with the original bytes held in memory. A control that leaves the tree
  dirty turns the next gate's clean-check into a false negative.

WHAT THIS DOES NOT COVER

Stated rather than rounded up, because a limit left unstated is a limit that
reads as coverage:

  * The harness cannot control ITSELF, and check-gates.py is exempt because its
    own probe is already a mutation. So two of the gates in scripts/ are held to
    the anti-vacuity floor by nothing but this paragraph. Both fail open.
  * Fixtures are MUTATED COPIES of real files, not built from literals. Building
    them from literals would delete the did-the-edit-land question outright
    rather than defending against it, and the defence is what is implemented
    here. Several gates discover their inputs by walking the repository, so a
    literal fixture for those would have to be a repository — a real cost, not a
    design preference.
  * A control proves a gate rejects ONE violation. A gate can still be blind to
    a second shape nobody thought to plant, which is how the RouteAPI gate
    shipped: it rejected the drift it was written for and read a comment as code.

    scripts/check-controls.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"

# Gates that take a mutation-based control. A gate absent from here fails the
# anti-vacuity check below; add a control rather than an exemption.
#
# Two gates are exempt for a stated reason rather than by omission:
#   check-gates.py       — its own probe is a mutation (it writes a deliberately
#                          permissive gate and requires it to be caught), so a
#                          control here would test the same thing twice.
#   check-controls.py    — this file.
EXEMPT = {
    "check-gates.py": "carries its own mutation probe: writes a permissive gate and requires it to be caught",
    "check-controls.py": "is the control harness",
}


class Control:
    """One (file, edit) pair that must make a gate fail."""

    def __init__(
        self,
        gate: str,
        path: str,
        before: str,
        after: str,
        catches: str,
        args: list[str] | None = None,
        expect_output: str | None = None,
    ):
        self.gate = gate
        self.path = ROOT / path
        self.before = before
        self.after = after
        self.catches = catches
        self.args = args or []
        # When set, the gate must not only reject — its output must contain this
        # text. That is what turns a control on the MECHANISM into a control on
        # the PROPERTY: "the gate exits non-zero" holds for any refactor, while
        # "the finding is cited at line 7" fails the moment a rewrite shifts the
        # citation, which is the defect worth catching and the one a bare exit
        # code cannot see.
        self.expect_output = expect_output


CONTROLS = [
    Control(
        gate="check-route-api-parity.py",
        path="packages/eval-runner/src/model.ts",
        before="export type RouteAPI = 'Anthropic' | 'OpenAI';",
        after="export type RouteAPI = 'Anthropic';",
        catches="a member dropped from the TypeScript union while the CRD still admits it — "
        "the runner then posts the wrong body at a gateway reporting healthy",
    ),
    Control(
        gate="check-route-api-parity.py",
        path="packages/eval-runner/src/model.ts",
        before="export type RouteAPI = 'Anthropic' | 'OpenAI';",
        after="// export type RouteAPI = 'Anthropic' | 'OpenAI';\nexport type RouteAPI = 'Anthropic';",
        catches="the SAME drift hidden behind a comment. This is the control that found the gate "
        "blind: matching over raw text, the commented-out line won the search and the gate "
        "reported the comment's members instead of the code's",
    ),
    Control(
        gate="check-version-pins.py",
        path=".github/workflows/security.yaml",
        before='GITLEAKS_VERSION: "8.30.1"',
        after='GITLEAKS_VERSION: "8.30.1"\n          UNWATCHED_TOOL_VERSION: "1.0.0"',
        catches="a new pinned tool version with no Renovate customManager watching it",
    ),
    Control(
        gate="check-version-pins.py",
        path="renovate.json",
        before="GITLEAKS_VERSION:",
        after="GITLEAKS_VERSION_NEVER_MATCHES:",
        catches="a manager whose regex stops matching its pin. This proves the gate READS "
        "renovate.json and applies its real regexes rather than restating the rule — a gate "
        "carrying its own copy would still report the pin covered. An earlier version of this "
        "control renamed depNameTemplate instead and passed, because a rename leaves the "
        "matchStrings intact and coverage genuinely unchanged: the control was wrong, not the gate",
    ),
    Control(
        gate="check-version-pins.py",
        path="renovate.json",
        before='"ZIZMOR_VERSION:',
        after='"ZIZMOR_VERSION_RULE_DELETED_BY_CONTROL:',
        catches="a manager whose matchStrings stop matching their pin. Deleting the RULE must make "
        "the gate fail rather than fall back to an empty pattern — an empty pattern matches nothing "
        "and passes everything, the empty-enumeration failure wearing different clothes. An earlier "
        "form of this control renamed depNameTemplate and left matchStrings intact: the edit landed, "
        "the bytes changed, and the meaning did not, so the gate's pass was correct and the CONTROL "
        "was the thing at fault",
    ),
    Control(
        gate="check-named-paths.py",
        path="README.md",
        before="## Layout",
        after="## Layout\n\nSee [the ledger](docs/zzsynthetic-control-marker/ledger.md).\n",
        catches="prose naming a repo path that does not exist — the claim documentation makes most "
        "often and the one that rots fastest. The marker is a synthetic token that appears nowhere "
        "in the tree: a realistic-looking path could already exist, and then present-after-mutating "
        "would prove nothing",
    ),
    Control(
        gate="check-named-paths.py",
        path="README.md",
        # Anchored at the FIRST line so the expected citation is deterministic.
        # Anchoring mid-file would make the assertion depend on how much prose
        # sits above it, and the control would then break on an unrelated edit —
        # a control that goes stale is a control that stops being run.
        before="# eks-agent-platform",
        after=(
            "<!-- filler -->\n<!-- filler -->\n<!-- filler -->\n"
            "[the ledger](docs/zzsynthetic-line-marker/ledger.md)\n\n# eks-agent-platform"
        ),
        catches="a citation shifted off the violation's own line. Three comment lines sit above the "
        "broken reference, so a pattern anchored with a newline-spanning class would report one of "
        "THEM instead — a failure that does not announce itself, because the gate still rejects and "
        "only the line number is wrong. Asserting the line rather than the exit code makes this "
        "property survive a refactor to a different matching mechanism",
        expect_output="README.md:4",
    ),
    Control(
        gate="no-placeholders.sh",
        path="charts/tenant/values.yaml",
        before="platform:",
        after="platform:\n  accountId: CHANGEME",
        catches="an unfilled fill-me sentinel reaching applied deploy configuration",
    ),
    Control(
        gate="check-doc-contracts.sh",
        path="README.md",
        before="## Layout",
        after="## Layout\n\nSee `terraform/components/agent-iam` for the tenant role.\n",
        catches="on-call prose pointing at a path this repo does not have",
    ),
    Control(
        gate="check-doc-contracts.sh",
        path="terraform/components/cost-pipeline/main.tf",
        before='check "the_cost_allocation_tags_are_active"',
        after='check "renamed_check_block"',
        catches="a terraform check block renamed while the README still documents the old name — the "
        "operator sees a warning on every plan and the README cannot tell them whether it is "
        "expected. This control also exercises the gate's sed extraction path, which the README "
        "control does not reach: sed dialects differ between BSD and GNU, so an extraction that "
        "silently returns nothing would make the comparison vacuous on one platform",
    ),
    Control(
        gate="check-project-resources.py",
        path="operators/PROJECT",
        before="    kind: Tenant",
        after="    kind: TenantDeleted",
        catches="PROJECT declaring a kind with no CRD behind it — the file announcing an API that does not exist",
    ),
    Control(
        gate="check-chart-args.py",
        path="charts/operator/templates/deployment.yaml",
        before="            - --leader-elect={{ .Values.leaderElection.enabled }}",
        after="            - --leader-elect={{ .Values.leaderElection.enabled }}\n            - --flag-the-binary-does-not-define=1",
        catches="the chart passing a flag the operator binary does not define — Go's flag package exits on it, "
        "so every pod crashloops at startup",
    ),
    Control(
        gate="check-chart-rbac.py",
        path="charts/operator/templates/rbac.yaml",
        before='  - apiGroups: [""]\n    resources: ["users"]\n    verbs: ["impersonate"]',
        after="",
        catches="a permission the kubebuilder markers generate that the hand-maintained chart ClusterRole omits — "
        "the operator then cannot do the thing its markers say it does",
    ),
    Control(
        gate="check-image-refs.py",
        path="charts/operator/values.yaml",
        before="repository: ghcr.io/nanohype/eks-agent-platform/operator",
        after="repository: ghcr.io/nanohype/eks-agent-platform/operator-does-not-exist",
        catches="the chart naming an image no registry can serve",
    ),
    Control(
        gate="check-crd-instantiation.py",
        path="charts/tenant/templates/platform.yaml",
        before="kind: Platform",
        after="kind: PlatformTypo",
        catches="a shipped CRD kind that nothing instantiates, or a rendered CR whose kind does not exist",
    ),
    Control(
        gate="check-chart-crd-parity.py",
        path="charts/tenant/templates/platform.yaml",
        before="    extraPolicyArns: {{- toYaml .Values.identity.extraPolicyArns | nindent 6 }}",
        after="    extraPolicyArns: {{- toYaml .Values.identity.extraPolicyArns | nindent 6 }}\n    notACrdField: true",
        catches="the tenant chart emitting a field the Platform CRD would reject at admission",
    ),
    Control(
        gate="check-workflow-triggers.py",
        path=".github/workflows/pr-title.yaml",
        before="    types: [opened, edited, synchronize, reopened]",
        after="    types: [opened, synchronize, reopened]",
        catches="a gate on PR metadata that stops subscribing to the event which changes it — it then runs once "
        "on open, goes red, and fixing the title never re-triggers it",
    ),
    Control(
        gate="check-eval-workflow.py",
        path="charts/operator/files/eval-runtime/workflow-template.yaml",
        before="  securityContext:",
        after="  removedSecurityContext:",
        catches="the eval WorkflowTemplate losing the securityContext that lets its pods pass "
        "PodSecurity `restricted` admission — every step pod is then REJECTED, and nothing about "
        "the YAML is malformed",
    ),
    Control(
        gate="check-runtime-contract.py",
        path="charts/operator/files/eval-runtime/workflow-template.yaml",
        before="model-route-api",
        after="model-route-api-renamed",
        catches="the WorkflowTemplate declaring a route parameter under a name the reconciler does "
        "not supply — the chart and the binary are versioned separately, so this is exactly the "
        "drift that leaves every other gate green",
    ),
    Control(
        gate="check-ssm-contract.py",
        path="terraform/components/bedrock/main.tf",
        before="/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id",
        after="/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id_orphan",
        catches="an SSM parameter published under a name no consumer reads — the operator ignores "
        "keys it does not recognise, so an orphaned contract looks exactly like a working one",
    ),
    Control(
        gate="check-leaf-input-parity.py",
        path="terraform/live/development-platform/bedrock/terragrunt.hcl",
        before="  enable_guardrails_baseline = true",
        after="",
        catches="one environment dropping an input its siblings set — three environments run the same "
        "component, so a decision one of them makes is a decision all of them have to make",
    ),
    Control(
        gate="check-vcluster-chart.py",
        path="scripts/vcluster-render-inventory.txt",
        before="apps/v1 StatefulSet vcluster",
        after="apps/v1 StatefulSet vcluster-renamed",
        catches="the recorded vcluster render inventory drifting from what the pinned chart actually "
        "renders for the values the operator emits",
    ),
    Control(
        gate="check-chart-version-bump.py",
        path="charts/operator/Chart.yaml",
        before="version: 0.6.9",
        after="version: 0.6.8",
        catches="packaged chart content changing while the chart version stays put — an OCI tag is "
        "mutable, so the second push silently replaces the bytes behind a version already deployed",
    ),
]



def mutation_landed(original: str, mutated: str, marker: str) -> tuple[bool, str]:
    """Did the edit actually change the file's MEANING, not just its bytes?

    Three conditions, and each one has produced a false pass somewhere on this
    stack:

      changed   — a mutation that no-ops hands the gate an unmodified fixture.
                  The gate correctly passes it and the run records that pass as
                  evidence the control worked.
      present   — the edit may have applied somewhere other than intended.
      not-prior — the sharpest one. A marker the file ALREADY carried means the
                  edit landed, the bytes changed, and nothing about the meaning
                  did: planting a floor a package already meets is a mutation
                  that mutates nothing that matters.

    Reported as a reason rather than a bool so the failure names which condition
    was not met.
    """
    if mutated == original:
        return False, (
            "the mutation produced a byte-identical file, so the gate was handed an UNCHANGED "
            "fixture and its pass proves nothing — before and after are the same text"
        )
    if marker and marker not in mutated:
        return False, "the replacement text is absent after mutating; the edit described did not happen"
    if marker and marker in original:
        return False, (
            f"the marker {marker[:60]!r} was ALREADY in the file, so the edit changed bytes without "
            "changing meaning and the gate's verdict is about the original condition"
        )
    return True, ""


def harness_self_test() -> list[str]:
    """Try to fool mutation_landed. A harness nobody has attacked is one being trusted."""
    problems = []
    base = "alpha\nbeta\ngamma\n"

    ok, _ = mutation_landed(base, base, "beta")
    if ok:
        problems.append("harness: a no-op mutation was reported as landed")

    # Off-target: the file changed, but the marker claimed is not what appeared.
    ok, _ = mutation_landed(base, "alpha\nbeta\ndelta\n", "epsilon")
    if ok:
        problems.append("harness: an off-target edit was reported as landed")

    # Pre-existing: the marker was already there, so meaning did not change.
    ok, _ = mutation_landed(base, base + "extra\n", "beta")
    if ok:
        problems.append("harness: a pre-existing marker was reported as a landed mutation")

    ok, why = mutation_landed(base, "alpha\nbeta\ndelta\n", "delta")
    if not ok:
        problems.append(f"harness: a genuine mutation was rejected ({why})")
    return problems


def run(gate: str, args: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(SCRIPTS / gate), *args], capture_output=True, text=True, timeout=300, cwd=ROOT
    )


def anti_vacuity() -> list[str]:
    """The suite may not shrink to the gates someone remembered."""
    problems = []
    on_disk = {p.name for p in SCRIPTS.glob("check-*.py")} | {p.name for p in SCRIPTS.glob("*.sh")}
    if not on_disk:
        problems.append("no gates found on disk at all — this check is measuring nothing")
    controlled = {c.gate for c in CONTROLS}

    for gate in sorted(on_disk - controlled - set(EXEMPT)):
        problems.append(
            f"{gate} ships with no positive control. A gate nobody has broken on purpose is a gate "
            "being trusted; add a Control that introduces the violation it exists to catch."
        )
    for gate in sorted(controlled - on_disk):
        problems.append(f"a control names scripts/{gate}, which does not exist")
    for gate in sorted(set(EXEMPT) - on_disk):
        problems.append(f"{gate} is listed EXEMPT but does not exist; delete the exemption")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print each control and the violation it introduces")
    args = ap.parse_args()

    failures = harness_self_test() + anti_vacuity()

    for c in CONTROLS:
        label = f"{c.gate} ← {c.path.relative_to(ROOT)}"
        if args.list:
            print(f"  {label}\n      catches: {c.catches}")

        if not c.path.is_file():
            failures.append(f"{label}: the control's target file does not exist")
            continue
        original = c.path.read_text(encoding="utf-8")
        if c.before not in original:
            failures.append(
                f"{label}: the control's anchor text is gone, so it mutates nothing and proves nothing. "
                "Re-point the control at the current shape of the file."
            )
            continue

        # CLEAN FIRST. A non-zero exit after mutating means nothing if the gate
        # was already failing.
        pre = run(c.gate, c.args)
        if pre.returncode != 0:
            failures.append(
                f"{label}: the gate does not pass on the UNMODIFIED tree (exit {pre.returncode}), so its "
                f"reaction to the mutation proves nothing.\n      {pre.stdout.strip()[:200]} {pre.stderr.strip()[:200]}"
            )
            continue

        mutated = original.replace(c.before, c.after, 1)

        # PROVE THE MUTATION LANDED. A control that silently fails to mutate
        # hands the gate an unchanged fixture, the gate correctly passes it, and
        # the run records that pass as evidence the control worked — a false
        # negative that reads exactly like a success. Verified by inspecting the
        # text, never inferred from the exit code.
        #
        # This is why the mutation is a Python string replace rather than a
        # shell one-liner: sed address ranges, in-place flags and character
        # classes differ between BSD and GNU, so the same control can mutate on
        # one machine and no-op on the other.
        landed, why = mutation_landed(original, mutated, c.after)
        if not landed:
            failures.append(f"{label}: {why}")
            continue

        try:
            c.path.write_text(mutated, encoding="utf-8")
            # Read back from disk rather than trusting the write.
            if c.path.read_text(encoding="utf-8") == original:
                failures.append(f"{label}: the mutated file on disk is identical to the original")
                c.path.write_text(original, encoding="utf-8")
                continue
            post = run(c.gate, c.args)
        finally:
            c.path.write_text(original, encoding="utf-8")

        if post.returncode == 0:
            failures.append(
                f"{label}: the gate PASSED with the violation present — it fails open.\n"
                f"      the control introduced: {c.catches}"
            )
        elif c.expect_output and c.expect_output not in (post.stdout + post.stderr):
            failures.append(
                f"{label}: the gate rejected, but its finding does not contain {c.expect_output!r}. "
                "The rejection alone is not the property under test."
            )

    if failures:
        print(f"\n{len(failures)} control failure(s):\n", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    print(f"✓ {len(CONTROLS)} positive controls: every gate passes clean and rejects its own violation")
    return 0


if __name__ == "__main__":
    sys.exit(main())
