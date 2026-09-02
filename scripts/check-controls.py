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

  AN ANCHOR IS NOT A VALUE. A control that names its edit by quoting a version,
  a pinned tool release or a default model id stops matching the moment ordinary
  work moves that value, and a control that matches nothing mutates nothing.
  Controls over such text derive their anchor from the file's shape instead —
  see Control — and the run fails when the shape moves, which is the change that
  should be read.

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
import re
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary

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


class ControlAnchorError(Exception):
    """The control could not decide what to edit in this file."""


# Text that carries a VALUE rather than a shape: a dotted version, a v-prefixed
# release, an image digest, a model id's version suffix. A literal anchor
# quoting one of these has an expiry date it does not state — the next Renovate
# bump, chart release or model refresh removes it, the control stops matching,
# and a control that matches nothing mutates nothing while still being counted
# as declared.
#
# Enforced at construction rather than described, because the description is
# what was already there: the chart-version control quoted a version, the
# paragraph above the class said to anchor on shape, and the control expired
# anyway. There is no opt-out. A control that needs to sit on such a line
# derives its anchor instead, which is strictly more capable — a pattern can
# always name the literal it replaces.
VALUE_SHAPED = re.compile(r"\d+\.\d+(\.\d+)?|\bv\d+\.\d+|sha256:[0-9a-f]{8,}|-v\d+(:\d+)?\b")


class Control:
    """One (file, edit) pair that must make a gate fail.

    The edit is named in one of two forms, and which one is correct is a
    property of what the anchor text IS.

      LITERAL (before / after) — right when the anchor is a declaration whose
      wording is itself the thing under test: a type union, a permissions block,
      a resource name. Changing it is the deliberate act the gate exists to
      catch, so a control that stops matching is telling the truth.

      DERIVED (anchor / rewrite) — required when the anchor carries a VALUE that
      ordinary work moves: a chart version, a pinned tool release, a default
      model id. A literal there is a control with an expiry date. It does not
      fail loudly at the moment it expires — it fails on whatever unrelated
      commit next moves the value, in a run whose author has no reason to think
      about this file, and the message is about an anchor rather than about the
      gate. `anchor` is a regex that must match EXACTLY ONCE, and `rewrite`
      turns that match into the replacement, so the edit is described by the
      file's shape and the value it holds is the file's business.

    A derived control also has to keep the property that makes the mutation
    evidence: the replacement must make the gate reject whatever the file
    currently says. Choosing a value that is invalid by construction — a version
    below every version, an id no vocabulary contains — is what buys that,
    rather than a value chosen by looking at the file once.
    """

    def __init__(
        self,
        gate: str,
        path: str,
        catches: str,
        expect_output: str,
        before: str | None = None,
        after: str | None = None,
        anchor: str | None = None,
        rewrite=None,
        args: list[str] | None = None,
        expect_reject: bool = True,
        # Seconds this gate is given. The default suits a gate that reads the
        # tree; a gate that RUNS other gates needs its own budget.
        timeout: int = 300,
    ):
        self.gate = gate
        self.path = ROOT / path
        literal = before is not None and after is not None
        derived = anchor is not None and rewrite is not None
        if literal == derived:
            raise ValueError(
                f"control for {gate} must name its edit exactly one way: before+after (literal) "
                "or anchor+rewrite (derived from the file's shape)"
            )
        if literal:
            carried = VALUE_SHAPED.search(before)
            if carried:
                raise ValueError(
                    f"control for {gate} anchors on the literal {before[:60]!r}, which carries the "
                    f"value {carried.group(0)!r}. A value moves on ordinary work and takes the "
                    "anchor with it. Give this control anchor= (a regex naming the line's shape) "
                    "and rewrite= instead."
                )
        self.before = before
        self.after = after
        self.anchor = re.compile(anchor) if anchor else None
        self.rewrite = rewrite
        self.catches = catches
        self.args = args or []
        # REQUIRED. The gate must not only reject — its rejection must NAME the
        # violation this control planted.
        #
        # Exit status alone cannot separate "the gate failed" from "the gate
        # found what I planted". A gate rejecting the mutated fixture for an
        # unrelated reason — a missing dependency, a pre-existing finding
        # elsewhere in the file — scores as proof it catches this violation, and
        # the control then guards nothing while reporting that it does.
        if not expect_output:
            raise ValueError(f"control for {gate} declares no expect_output; a rejection that does not name the planted violation is not evidence about it")
        self.expect_output = expect_output
        # Most controls plant a violation and require a rejection. A few plant
        # something that must NOT trip the gate — the near-miss that proves the
        # gate is not over-matching. Without this the floor is one-sided at the
        # control level: it can show a gate rejects, never that it discriminates,
        # and a gate rejecting everything would pass every control it has.
        self.expect_reject = expect_reject
        self.timeout = timeout

    def resolve(self, text: str) -> tuple[str, str]:
        """The (before, after) pair for THIS text.

        Raises rather than returning a sentinel: a control that cannot find what
        it edits has to stop the run, and a caller that forgets to check a
        sentinel would record the control as proven.
        """
        if self.anchor is None:
            if self.before not in text:
                raise ControlAnchorError(
                    "the control's anchor text is gone, so it mutates nothing and proves nothing. "
                    "Re-point the control at the current shape of the file — and if the text that "
                    "vanished was a value ordinary work moves, give the control an anchor pattern "
                    "instead of a literal."
                )
            return self.before, self.after
        hits = list(self.anchor.finditer(text))
        if not hits:
            raise ControlAnchorError(
                f"the control's anchor pattern {self.anchor.pattern!r} matches nothing in the file, "
                "so it mutates nothing and proves nothing. The file's SHAPE moved, which is the one "
                "thing a derived anchor is not allowed to survive silently."
            )
        if len(hits) > 1:
            raise ControlAnchorError(
                f"the control's anchor pattern {self.anchor.pattern!r} matches {len(hits)} places, so "
                "which one it mutates is decided by the file rather than by the control. Narrow the "
                "pattern until it names one."
            )
        match = hits[0]
        return match.group(0), self.rewrite(match)


# Exit codes that are NOT a verdict about the tree. Every one of them exits
# non-zero, which is indistinguishable from a rejection unless it is named.
NOT_A_REJECTION = {
    2: "argparse usage error — the gate never reached its check",
    3: "precondition failure — a tool the gate needs is missing or does not run",
    126: "found but not executable",
    124: "timed out — the gate never finished, so it rejected nothing",
    127: "command not found",
}

def crash_reason(returncode: int) -> str | None:
    """Why this exit is not a verdict about the tree, or None if it is a rejection.

    Kept separate from the verdict loop so the self-test can attack it directly.
    A rule that only exists inline is a rule nothing has ever exercised.
    """
    if returncode > 128:
        return "killed by a signal"
    return NOT_A_REJECTION.get(returncode)


CONTROLS = [
    Control(
        gate="check-route-api-parity.py",
        path="packages/eval-runner/src/model.ts",
        before="export type RouteAPI = 'Anthropic' | 'OpenAI';",
        after="export type RouteAPI = 'Anthropic';",
        catches="a member dropped from the TypeScript union while the CRD still admits it — "
        "the runner then posts the wrong body at a gateway reporting healthy",
        expect_output='Anthropic',
    ),
    Control(
        gate="check-route-api-parity.py",
        path="packages/eval-runner/src/model.ts",
        before="export type RouteAPI = 'Anthropic' | 'OpenAI';",
        after="// export type RouteAPI = 'Anthropic' | 'OpenAI';\nexport type RouteAPI = 'Anthropic';",
        catches="the SAME drift hidden behind a comment. This is the control that found the gate "
        "blind: matching over raw text, the commented-out line won the search and the gate "
        "reported the comment's members instead of the code's",
        expect_output='Anthropic',
    ),
    Control(
        gate="check-version-pins.py",
        path=".github/workflows/security.yaml",
        # Derived: the anchor is a version Renovate exists to move, so a literal
        # here expires on a bot's schedule rather than a person's. The pattern
        # names the pin's SHAPE and carries its indentation into the line the
        # mutation adds, so the fixture stays valid YAML wherever the block sits.
        anchor=r'(?m)^([ \t]*)GITLEAKS_VERSION: "[^"]+"$',
        rewrite=lambda m: f'{m.group(0)}\n{m.group(1)}UNWATCHED_TOOL_VERSION: "1.0.0"',
        catches="a new pinned tool version with no Renovate customManager watching it",
        expect_output='UNWATCHED_TOOL_VERSION',
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
        expect_output='GITLEAKS_VERSION',
    ),
    Control(
        gate="check-version-pins.py",
        path="renovate.json",
        before='"ZIZMOR_VERSION:',
        after='"ZIZMOR_VERSION_RULE_DELETED_BY_CONTROL:',
        expect_output="ZIZMOR_VERSION",
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
        expect_output='zzsynthetic-control-marker',
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
        expect_output="README.md:4",
        catches="a citation shifted off the violation's own line. Three comment lines sit above the "
        "broken reference, so a pattern anchored with a newline-spanning class would report one of "
        "THEM instead — a failure that does not announce itself, because the gate still rejects and "
        "only the line number is wrong. Asserting the line rather than the exit code makes this "
        "property survive a refactor to a different matching mechanism",
    ),
    Control(
        gate="check-model-tiers.py",
        path="operators/internal/agentctl/model_defaults.json",
        # Derived: the value is a model id, and the file's own instruction is to
        # bump model ids here. A literal anchor would be a control that expires
        # on the next model refresh — the edit this control is closest to.
        anchor=r'(?m)^(\s*)"default": "[^"]+",$',
        rewrite=lambda m: f'{m.group(1)}"default": "us.anthropic.claude-zzsynthetic-drifted-tier",',
        catches="the scaffolder's model tiers drifting from the org LLM policy — the drift that "
        "shipped a full generation behind while the file's own comment claimed it mirrored the "
        "standard. The marker is a synthetic id so it cannot already appear in the tree",
        expect_output='claude-zzsynthetic-drifted-tier',
    ),
    Control(
        gate="check-tenant-chart-artifacts.py",
        path="charts/tenant/values-staging.yaml",
        before="# Per-environment delta",
        after="account: 123456789012\n# Per-environment delta",
        catches="an estate literal reaching a chart value — an AWS account id written down where "
        "landing-zone outputs should plumb it. The delta files are exactly where someone reaches "
        "for one",
        expect_output='123456789012',
    ),
    Control(
        gate="check-chart-args.py",
        path="operators/cmd/main.go",
        before="\tflag.StringVar(&environment,",
        after="\t// zzsynthetic-commented-flag: flag.StringVar(&environment,",
        catches="a flag definition read from a COMMENT. Commenting one out left the chart passing a "
        "flag the binary no longer defines while this gate passed — and Go's flag package exits on "
        "an unknown argument, so every operator pod crashloops at startup, which is the exact "
        "failure the gate exists to prevent",
        expect_output="environment",
    ),
    Control(
        gate="check-ssm-contract.py",
        path="terraform/components/bedrock/main.tf",
        # The WHOLE resource, not just its `name` line. Commenting one line
        # leaves an aws_ssm_parameter with no resolvable name, which the gate
        # correctly refuses to evaluate — a different property from the one
        # under test here, and it exits 3 rather than rejecting.
        before="""resource "aws_ssm_parameter" "baseline_guardrail_id" {
  count = local.enable_guardrail ? 1 : 0
  name  = "/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id"
  type  = "String"
  value = aws_bedrock_guardrail.baseline[0].guardrail_id
  tags  = local.tags
}""",
        after="""# resource "aws_ssm_parameter" "baseline_guardrail_id" {
#   count = local.enable_guardrail ? 1 : 0
#   name  = "/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id"
#   type  = "String"
#   value = aws_bedrock_guardrail.baseline[0].guardrail_id
#   tags  = local.tags
# }""",
        catches="an SSM producer read from a COMMENT. A commented-out publisher counted as "
        "publishing, so a parameter nobody writes read as written and the gate reported the "
        "contract whole",
        expect_output="baseline_guardrail_id",
    ),
    Control(
        gate="check-shell-portability.py",
        path="scripts/local-kx/uninstall.sh",
        before="set -euo pipefail",
        after="set -euo pipefail\nmapfile -t zzsynthetic_lines < /dev/null",
        catches="a bash 4 builtin in a script macOS will run on bash 3.2 — the class that shipped "
        "here as `declare -A` in the local-kx installer, aborting it before a single prerequisite "
        "was checked",
        expect_output="mapfile",
    ),
    Control(
        gate="check-shell-portability.py",
        path="scripts/local-kx/uninstall.sh",
        before="set -euo pipefail",
        after="set -euo pipefail\n# zzsynthetic: mapfile and declare -A are bash 4 builtins",
        catches="the construct appearing ONLY IN A COMMENT, which must NOT trip the gate. A gate "
        "that flagged it could not carry the prose warning about the class it enforces — the first "
        "thing it would reject is the sentence explaining it. This is the near-miss that proves the "
        "gate discriminates rather than merely rejects.",
        expect_output="use nothing newer than bash 3.2",
        expect_reject=False,
    ),
    Control(
        gate="no-placeholders.sh",
        path="charts/tenant/values.yaml",
        before="platform:",
        after="platform:\n  accountId: CHANGEME",
        catches="an unfilled fill-me sentinel reaching applied deploy configuration",
        expect_output='CHANGEME',
    ),
    Control(
        gate="check-doc-contracts.sh",
        path="README.md",
        before="## Layout",
        after="## Layout\n\nSee `terraform/components/agent-iam` for the tenant role.\n",
        catches="on-call prose pointing at a path this repo does not have",
        expect_output='agent-iam',
    ),
    Control(
        gate="check-doc-contracts.sh",
        path="terraform/components/cost-pipeline/main.tf",
        before='check "the_cost_allocation_tags_are_active"',
        after='check "zzsynthetic_renamed_check_block"',
        catches="a terraform check block renamed while the README still documents the old name — the "
        "operator sees a warning on every plan and the README cannot tell them whether it is "
        "expected. This control also exercises the gate's sed extraction path, which the README "
        "control does not reach: sed dialects differ between BSD and GNU, so an extraction that "
        "silently returns nothing would make the comparison vacuous on one platform",
        expect_output="zzsynthetic_renamed_check_block",
    ),
    Control(
        gate="check-project-resources.py",
        path="operators/PROJECT",
        before="    kind: Tenant",
        after="    kind: TenantDeleted",
        catches="PROJECT declaring a kind with no CRD behind it — the file announcing an API that does not exist",
        expect_output='TenantDeleted',
    ),
    Control(
        gate="check-chart-args.py",
        path="charts/operator/templates/deployment.yaml",
        before="            - --leader-elect={{ .Values.leaderElection.enabled }}",
        after="            - --leader-elect={{ .Values.leaderElection.enabled }}\n            - --flag-the-binary-does-not-define=1",
        catches="the chart passing a flag the operator binary does not define — Go's flag package exits on it, "
        "so every pod crashloops at startup",
        expect_output='flag-the-binary-does-not-define',
    ),
    Control(
        gate="check-chart-rbac.py",
        path="charts/operator/templates/rbac.yaml",
        before='  - apiGroups: [""]\n    resources: ["users"]\n    verbs: ["impersonate"]',
        after="",
        catches="a permission the kubebuilder markers generate that the hand-maintained chart ClusterRole omits — "
        "the operator then cannot do the thing its markers say it does",
        expect_output='impersonate',
    ),
    Control(
        gate="check-image-refs.py",
        path="charts/operator/values.yaml",
        before="repository: ghcr.io/nanohype/eks-agent-platform/operator",
        after="repository: ghcr.io/nanohype/eks-agent-platform/zzsynthetic-absent-image",
        catches="the chart naming an image no registry can serve. Controlled in ONLINE mode, "
        "because that is the half this violation belongs to — the offline mode deliberately does "
        "not consult a registry, so it cannot and should not catch this one",
        expect_output='zzsynthetic-absent-image',
    ),
    Control(
        gate="check-crd-instantiation.py",
        # SandboxPool, not Platform. Renaming Platform plants TWO violations —
        # the kind stops being instantiated AND every platformRef in the render
        # is orphaned — so the gate rejects on the orphan it reaches first and
        # never evaluates the planted one, while the floor scores the rejection
        # as a catch. Nothing references a SandboxPool, so renaming it changes
        # exactly the fact under test.
        path="charts/tenant/templates/sandboxpool.yaml",
        before="kind: SandboxPool",
        after="kind: SandboxPoolTypo",
        catches="a shipped CRD kind that nothing instantiates, or a rendered CR whose kind does not exist",
        expect_output="SandboxPool is a shipped CRD kind",
    ),
    Control(
        gate="check-chart-crd-parity.py",
        path="charts/tenant/templates/platform.yaml",
        before="    extraPolicyArns: {{- toYaml .Values.identity.extraPolicyArns | nindent 6 }}",
        after="    extraPolicyArns: {{- toYaml .Values.identity.extraPolicyArns | nindent 6 }}\n    notACrdField: true",
        catches="the tenant chart emitting a field the Platform CRD would reject at admission",
        expect_output='notACrdField',
    ),
    Control(
        gate="check-workflow-triggers.py",
        path=".github/workflows/pr-title.yaml",
        before="    types: [opened, edited, synchronize, reopened]",
        after="    types: [opened, synchronize, reopened]",
        catches="a gate on PR metadata that stops subscribing to the event which changes it — it then runs once "
        "on open, goes red, and fixing the title never re-triggers it",
        expect_output='edited',
    ),
    Control(
        gate="check-eval-workflow.py",
        path="charts/operator/files/eval-runtime/workflow-template.yaml",
        before="  securityContext:",
        after="  removedSecurityContext:",
        catches="the eval WorkflowTemplate losing the securityContext that lets its pods pass "
        "PodSecurity `restricted` admission — every step pod is then REJECTED, and nothing about "
        "the YAML is malformed",
        expect_output='securityContext',
    ),
    Control(
        gate="check-runtime-contract.py",
        path="charts/operator/files/eval-runtime/workflow-template.yaml",
        before="model-route-api",
        after="model-route-api-renamed",
        catches="the WorkflowTemplate declaring a route parameter under a name the reconciler does "
        "not supply — the chart and the binary are versioned separately, so this is exactly the "
        "drift that leaves every other gate green",
        expect_output='model-route-api',
    ),
    Control(
        gate="check-ssm-contract.py",
        path="terraform/components/bedrock/main.tf",
        before="/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id",
        after="/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id_orphan",
        catches="an SSM parameter published under a name no consumer reads — the operator ignores "
        "keys it does not recognise, so an orphaned contract looks exactly like a working one",
        expect_output='baseline_guardrail_id',
    ),
    Control(
        gate="check-leaf-input-parity.py",
        path="terraform/live/development-platform/bedrock/terragrunt.hcl",
        before="  enable_guardrails_baseline = true",
        after="",
        catches="one environment dropping an input its siblings set — three environments run the same "
        "component, so a decision one of them makes is a decision all of them have to make",
        expect_output='enable_guardrails_baseline',
    ),
    Control(
        gate="check-vcluster-chart.py",
        path="scripts/vcluster-render-inventory.txt",
        before="apps/v1 StatefulSet vcluster",
        after="apps/v1 StatefulSet vcluster-renamed",
        catches="the recorded vcluster render inventory drifting from what the pinned chart actually "
        "renders for the values the operator emits",
        expect_output='vcluster',
    ),
    Control(
        gate="check-chart-version-bump.py",
        path="charts/operator/Chart.yaml",
        # Derived, and this is the control that taught the distinction: it
        # anchored on the literal version and went vacuous on the first bump
        # after it was written — reported not as "the control expired" but as an
        # anchor failure on a security change that had nothing to do with it.
        #
        # 0.0.0 is chosen so the rejection does not depend on what the base
        # branch says. It is either strictly below the published version, which
        # is the backwards case, or equal to it, which is the no-bump case. Both
        # are rejections, so the mutation cannot become a no-op by the chart
        # moving underneath it.
        anchor=r"(?m)^version: \d+\.\d+\.\d+$",
        rewrite=lambda m: "version: 0.0.0",
        catches="packaged chart content changing while the chart version stays put — an OCI tag is "
        "mutable, so the second push silently replaces the bytes behind a version already deployed",
        expect_output="0.0.0",
    ),
    # Two spellings, two code paths. `permissions: write-all` is a STRING at the
    # top level and `{id-token: write}` is a mapping; a gate that only walks the
    # mapping form passes the broader grant of the two.
    Control(
        gate="check-workflow-permissions.py",
        path=".github/workflows/ci.yaml",
        before="\npermissions:\n  contents: read\n",
        after="\npermissions:\n  contents: read\n  id-token: write\n",
        catches="an OIDC-token grant moved to workflow level, where it applies to every job "
        "added afterwards — so the diff that acquires the cloud role contains no permission change. "
        "zizmor's DEFAULT persona exits 0 on this input and --persona=auditor exits 14",
        expect_output="id-token",
    ),
    Control(
        gate="check-workflow-permissions.py",
        path=".github/workflows/pr-title.yaml",
        before="\npermissions:\n  contents: read\n  pull-requests: read   # read the pull request title this validates\n",
        after="\npermissions: write-all\n",
        catches="the string spelling of a total grant, which a gate walking only the mapping "
        "form reads as no permissions at all",
        expect_output="write-all",
    ),
    # The gate that guards the guard. Dropping a job from the needs list is the
    # silent half of this class: the job still runs and still goes red, and the
    # merge button stays enabled beside it.
    Control(
        gate="check-merge-gate.py",
        path=".github/workflows/ci.yaml",
        before="        named-paths,\n",
        after="",
        catches="a job that runs on pull requests dropped from the merge gate's needs list, "
        "where it reports failures into a UI that ignores them",
        expect_output="named-paths",
    ),
    # An ACCEPT control. The stripper chained two single-syntax passes, so a `#`
    # comment mentioning an S3 key prefix had its `/*` read as a block-comment
    # opener and 130 lines of live Terraform vanished — three published
    # parameters with them, while the gate reported the contract whole. The
    # property under test is that a comment mentioning a slash-star does not
    # remove the resources under it.
    Control(
        gate="check-ssm-contract.py",
        path="terraform/components/bedrock/main.tf",
        before='resource "aws_ssm_parameter" "baseline_guardrail_id" {',
        after='# reports/{platform}/manifests/* are written beside this\n'
              'resource "aws_ssm_parameter" "baseline_guardrail_id" {',
        catches="a hash comment containing a slash-star erasing the resources below it",
        expect_output="22 parameters published",
        expect_reject=False,
    ),
    # Per FEATURE, not per spelling: -n is the same absence as -A, and the table
    # listed only -A.
    Control(
        gate="check-shell-portability.py",
        path="scripts/local-kx/install.sh",
        before="set -euo pipefail",
        after="set -euo pipefail\ndeclare -n __ref=PATH",
        catches="a bash 4.3 nameref in a script that must run on the bash 3.2 macOS ships — "
        "an adjacent spelling of the absence the table already knew about",
        expect_output="declare/local -n nameref",
    ),
    # Removing a floor must make the empty-tree check fail. Without this the
    # floors it relies on could be weakened and nothing would notice — which is
    # how both of them came to be satisfiable by the gates' own directory.
    Control(
        gate="check-empty-tree.py",
        path="scripts/check-shell-portability.py",
        before="    if outside_gate_dir == 0:",
        after="    if False:",
        catches="a gate whose floor counts only what MATCHED, so a tree holding just the gate "
        "scripts satisfies it with the gates' own files and the gate certifies its own presence",
        expect_output="check-shell-portability.py",
        # It runs every other gate twice over, once clean and once mutated.
        timeout=1800,
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

    # A DERIVED anchor has two ways to stop deciding what it edits, and both
    # have to raise rather than pick something. Silently matching nothing is the
    # vacuity a literal anchor already produced once; silently matching several
    # places is the same failure inverted — the control still runs, and what it
    # edits is chosen by whichever occurrence comes first in the file.
    def probe(anchor, text):
        c = Control(
            gate="check-controls.py", path="scripts/check-controls.py",
            anchor=anchor, rewrite=lambda m: "x",
            catches="probe", expect_output="x",
        )
        try:
            c.resolve(text)
        except ControlAnchorError as e:
            return str(e)
        return None

    if probe(r"^delta$", base) is None:
        problems.append("harness: a derived anchor matching NOTHING was resolved instead of raising")
    if probe(r"(?m)^[a-z]+$", base) is None:
        problems.append("harness: a derived anchor matching THREE places was resolved instead of raising")
    if probe(r"(?m)^beta$", base) is not None:
        problems.append("harness: a derived anchor matching exactly once failed to resolve")

    # A literal anchor quoting a value is refused, and the same text is fine as a
    # derived anchor — which is what makes the rule a redirection rather than a
    # ban on controlling those files.
    try:
        Control(gate="check-controls.py", path="scripts/check-controls.py",
                before="version: 1.2.3", after="version: 0.0.0",
                catches="probe", expect_output="x")
        problems.append("harness: a literal anchor quoting a version was accepted")
    except ValueError:
        pass
    try:
        Control(gate="check-controls.py", path="scripts/check-controls.py",
                anchor=r"(?m)^version: \d+\.\d+\.\d+$", rewrite=lambda m: "version: 0.0.0",
                catches="probe", expect_output="x")
    except ValueError:
        problems.append("harness: a derived anchor over the same line was refused")

    # Naming an edit both ways, or neither, is a control that has not said what
    # it does. Caught at construction so it cannot reach a run.
    for kwargs in (
        {"before": "a", "after": "b", "anchor": r"a", "rewrite": lambda m: "b"},
        {},
    ):
        try:
            Control(gate="check-controls.py", path="scripts/check-controls.py",
                    catches="probe", expect_output="x", **kwargs)
        except ValueError:
            continue
        problems.append(f"harness: a control declaring {sorted(kwargs) or 'no edit'} was accepted")

    ok, why = mutation_landed(base, "alpha\nbeta\ndelta\n", "delta")
    if not ok:
        problems.append(f"harness: a genuine mutation was rejected ({why})")

    # The crash rule. Every code here exits non-zero, which is how a gate says
    # REJECTED — so if any of them classifies as a verdict, this suite reports
    # guards that do not exist.
    if crash_reason(1) is not None:
        problems.append("harness: exit 1 was classified as a crash, so real rejections cannot score")
    for code in (2, 3, 126, 127, 137):
        if crash_reason(code) is None:
            problems.append(f"harness: exit {code} would score as a rejection")

    # Against a real process, not only the table: a shell asked for a binary that
    # does not exist exits 127, and that is the case eks-gitops scored as five
    # catches.
    got = subprocess.run(
        ["sh", "-c", "definitely-not-a-real-binary-xyz"], capture_output=True, text=True
    ).returncode
    if crash_reason(got) is None:
        problems.append(
            f"harness: a missing binary really exited {got}, and that code classifies as a rejection"
        )

    # A gate can exit 127 having printed NOTHING — its tool vanished, or it
    # discards its own diagnostics. Screening crashes by matching output for
    # "Traceback" or "command not found" cannot see this shape by construction,
    # and it is the one where the floor records the strictest gate in the suite.
    # Both rules are kept: the text rule catches a gate that died mid-run and
    # said so, the NUMBER catches one that said nothing.
    silent = subprocess.run(["sh", "-c", "exit 127"], capture_output=True, text=True)
    if silent.returncode != 127 or (silent.stdout + silent.stderr).strip():
        problems.append(
            f"harness: the silent-crash fixture exited {silent.returncode} and printed "
            f"{(silent.stdout + silent.stderr)!r}. It must exit 127 and print nothing — a fixture "
            "that starts printing stops testing the case it exists for."
        )
    elif crash_reason(silent.returncode) is None:
        problems.append("harness: a SILENT exit 127 classifies as a rejection")
    if "Traceback" in (silent.stdout + silent.stderr) or "not found" in (silent.stdout + silent.stderr):
        problems.append("harness: the silent fixture is not silent, so it no longer tests the text-blind case")
    return problems


def run(gate: str, args: list[str], timeout: int = 300) -> subprocess.CompletedProcess[str]:
    """Run a gate. A timeout becomes a reportable result rather than a traceback.

    A gate that never finishes has not rejected anything, and letting the
    TimeoutExpired escape loses which control was in flight — the same
    crash-instead-of-verdict shape this suite screens gates for.
    """
    try:
        return subprocess.run(
            [str(SCRIPTS / gate), *args], capture_output=True, text=True, timeout=timeout, cwd=ROOT
        )
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(
            args=[gate], returncode=124,
            stdout="", stderr=f"{gate}: did not finish within {timeout}s",
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

    # git is asserted HERE rather than inside each gate, because it does not
    # merely run alongside them: several scope their population to the tracked
    # set, and the controls build fixture repositories. It decides what the
    # suite is meant to examine, which makes it upstream of the entire run —
    # asserting it per-gate is too late, and its absence would surface as an
    # exception naming a binary instead of naming what could not be determined.
    require_binary("git", "determine the tracked set the gates scope themselves to")

    # COMMITTED DEBRIS. A crash mid-loop used to leave a fixture mutated on disk;
    # the restore now runs in a finally, but that does not help a mutation that
    # was already staged and committed before the crash was understood. One was:
    # a workflow-level `id-token: write` planted by a control reached main's
    # branch and was caught by CI rather than here.
    #
    # So the committed form of every control target is checked against the
    # mutation that control applies, before any control runs. Knowing the failure
    # mode does not prevent it; this does.
    debris = []
    for c in CONTROLS:
        rel = c.path.relative_to(ROOT)
        head = subprocess.run(
            ["git", "show", f"HEAD:{rel}"], capture_output=True, text=True, cwd=ROOT, timeout=60
        )
        if head.returncode != 0:
            continue
        try:
            before, after = c.resolve(head.stdout)
        except ControlAnchorError:
            # The committed form does not carry what this control edits, so it
            # cannot be carrying that control's mutation either. Whether the
            # anchor is stale is the loop below's question, against the file on
            # disk; answering it twice here would report it against the wrong
            # tree.
            continue
        if after and after not in before and after in head.stdout and before not in head.stdout:
            debris.append(f"{rel}: carries the mutation from the {c.gate} control")
    if debris:
        print(
            f"\n{len(debris)} control mutation(s) are COMMITTED, not just present on disk:\n",
            file=sys.stderr,
        )
        for d in debris:
            print(f"  - {d}", file=sys.stderr)
        print(
            "\nA control's fixture is meant to exist only between the mutation and the restore. "
            "Committed, it is a real defect in the tree wearing a test's clothes, and every control "
            "over that file now measures the debris.",
            file=sys.stderr,
        )
        return 1

    # A JUDGEMENT, not an oversight, and stated as one so a reviewer can overrule
    # it rather than having to discover it: `sh` is invoked by the self-test
    # fixtures below and is NOT asserted by name. POSIX requires it, every runner
    # and developer machine this suite targets has it, and asserting it would
    # mean asserting the shell that would have to exist for the assertion itself
    # to be reportable. If `sh` is genuinely absent the fixtures raise, and that
    # is the one binary whose absence this file does not promise to name.

    failures = harness_self_test() + anti_vacuity()
    # CASES PROVEN, incremented at the END of the loop body — after the gate has
    # passed clean, the mutation has landed, the gate has rejected, and the
    # rejection has NAMED the mutation. len(CONTROLS) is a different quantity: it
    # counts controls DECLARED, and reads the same whether every one completed a
    # proof or none did.
    proven = 0
    # Every file this run touches, with the content it had BEFORE the run. A
    # crash anywhere in the loop — including in this suite's own bookkeeping —
    # otherwise leaves a mutation on disk, and the NEXT run measures the debris:
    # a gate reported as failing open when it rejects correctly on a clean tree.
    # That happened here, and git status was what settled it, so the restore is
    # no longer left to the per-control finally alone.
    touched: dict[pathlib.Path, str] = {}

    try:
      for c in CONTROLS:
          label = f"{c.gate} ← {c.path.relative_to(ROOT)}"
          if args.list:
              print(f"  {label}\n      catches: {c.catches}")

          if not c.path.is_file():
              failures.append(f"{label}: the control's target file does not exist")
              continue
          original = c.path.read_text(encoding="utf-8")
          touched.setdefault(c.path, original)
          try:
              before, after = c.resolve(original)
          except ControlAnchorError as e:
              failures.append(f"{label}: {e}")
              continue

          # CLEAN FIRST. A non-zero exit after mutating means nothing if the gate
          # was already failing.
          pre = run(c.gate, c.args, c.timeout)
          if "Traceback (most recent call last)" in (pre.stdout + pre.stderr) or "panic:" in (pre.stdout + pre.stderr):
              failures.append(
                  f"{label}: the gate CRASHED on the UNMODIFIED tree. A crash on either fixture means "
                  "the control is measuring an exception rather than a decision."
              )
              continue
          if pre.returncode != 0:
              pre_why = crash_reason(pre.returncode)
              failures.append(
                  f"{label}: the gate does not pass on the UNMODIFIED tree (exit {pre.returncode}"
                  f"{', ' + pre_why if pre_why else ''}), so its "
                  f"reaction to the mutation proves nothing.\n      {pre.stdout.strip()[:200]} {pre.stderr.strip()[:200]}"
              )
              continue

          mutated = original.replace(before, after, 1)

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
          landed, why = mutation_landed(original, mutated, after)
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
              post = run(c.gate, c.args, c.timeout)
          finally:
              c.path.write_text(original, encoding="utf-8")

          combined = post.stdout + post.stderr

          # A CRASHING gate exits non-zero, and a floor reading exit status alone
          # records that as a successful rejection — exit-code-conflates-causes
          # occurring inside the thing built to check for it. The name-the-mutation
          # assertion below catches most of it, since a traceback rarely contains
          # the planted marker, but "rarely" is not "cannot": a crash that echoes
          # the file it choked on can carry the marker along with it.
          if not c.expect_reject:
              # ACCEPT control: the mutated fixture must still pass.
              crashed = crash_reason(post.returncode)
              if crashed is not None:
                  failures.append(
                      f"{label}: the gate exited {post.returncode} ({crashed}) on a fixture it must "
                      "accept. That is not over-matching, it is a crash, and naming it as the former "
                      "sends the reader to the wrong line."
                  )
              elif post.returncode != 0:
                  failures.append(
                      f"{label}: the gate REJECTED a fixture it must accept — it over-matches.\n"
                      f"      the control introduced: {c.catches}"
                  )
              elif c.expect_output not in combined:
                  failures.append(
                      f"{label}: the gate accepted, but its output does not contain {c.expect_output!r}"
                  )
              else:
                  proven += 1
              continue

          # A non-zero exit is how a gate says REJECTED, so every other reason a
          # process exits non-zero arrives wearing the same clothes. A traceback is
          # only the visible half: exit 127 (command not found), 126 (found but not
          # executable), 3 (a precondition this repo's gates use for a missing
          # tool) and anything above 128 (killed by a signal) all exit non-zero
          # while proving nothing about the mutation. Scored as catches, they
          # report guards that do not exist — and they do it in the tool built to
          # check the other tools, where nothing else is looking.
          #
          # So the rejection code is named rather than inferred from "not zero".
          crashed = crash_reason(post.returncode)
          if crashed is not None:
              why = crashed
              failures.append(
                  f"{label}: the gate exited {post.returncode} ({why}) on the mutated fixture. That is "
                  "not a rejection — it exits non-zero and would otherwise score as a catch.\n"
                  f"      {combined.strip().splitlines()[-1][:160] if combined.strip() else '<no output>'}"
              )
          elif "Traceback (most recent call last)" in combined or "panic:" in combined:
              failures.append(
                  f"{label}: the gate CRASHED on the mutated fixture rather than rejecting it. A crash "
                  "exits non-zero and would otherwise score as a catch, so the control would report a "
                  "guard that does not exist.\n"
                  f"      {combined.strip().splitlines()[-1][:160] if combined.strip() else ''}"
              )
          elif post.returncode == 0:
              failures.append(
                  f"{label}: the gate PASSED with the violation present — it fails open.\n"
                  f"      the control introduced: {c.catches}"
              )
          elif c.expect_output not in combined:
              failures.append(
                  f"{label}: the gate rejected, but its finding does not contain {c.expect_output!r}. "
                  "The rejection alone is not the property under test."
              )
          else:
              proven += 1

    finally:
        # Runs whatever happened above, including an exception in this file.
        left = [q for q, before in touched.items() if q.read_text(encoding="utf-8") != before]
        for q in left:
            q.write_text(touched[q], encoding="utf-8")
        if left:
            print(
                f"check-controls: restored {len(left)} file(s) a failed run had left mutated: "
                + ", ".join(str(q.relative_to(ROOT)) for q in left),
                file=sys.stderr,
            )

    # TWO LAYERS, SAFE FOR DIFFERENT REASONS, and the difference is worth writing
    # down rather than calling both of them safe:
    #
    #   `proven` initialises to 0, which is the value that FAILS below. That is a
    #   GUARANTEE — a loop that never runs leaves the failing value in place, so
    #   it does not depend on anything else executing.
    #
    #   An EMPTY registry is caught by anti_vacuity() deriving membership from the
    #   gates on disk, which indicts every gate rather than certifying them. That
    #   is a second, independent check rather than an arrangement of this one, but
    #   it is a different mechanism and would not survive anti_vacuity being
    #   rewritten to read the registry's own length.
    #
    # Every control must have either proven something or failed. A control that
    # leaves the loop recording neither is a silent skip, and the summary would
    # report it as covered.
    unaccounted = len(CONTROLS) - proven - len(failures)
    if unaccounted:
        failures.append(
            f"{unaccounted} control(s) left the loop without proving or failing anything, so this "
            "run licenses nothing. The summary counts cases PROVEN; a control that records neither "
            "outcome is covered in the count and tested nowhere."
        )

    if failures:
        print(f"\n{len(failures)} control failure(s):\n", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    if proven == 0:
        print(
            "check-controls: zero controls completed a proof, so this run licenses nothing.",
            file=sys.stderr,
        )
        return 1
    print(
        f"✓ {proven} positive controls PROVEN (of {len(CONTROLS)} declared): every gate passes "
        "clean and rejects its own violation"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
