"""
Unit tests for the invocation cost publisher.

Two things are covered here, and the second is the reason this file is worth
reading.

Pricing correctness: cross-region inference-profile prefix stripping (so profile
IDs resolve to the base model price) and the unpriced path (an unknown model is
surfaced as an UnpricedInvocations count, never priced against a borrowed rate).

Attribution correctness: that the PlatformId this Lambda publishes is the one the
operator wrote, and not a value this Lambda decided. The previous version of these
tests is why that mattered. It fed role ARNs of the shape ``dev-acme-tenant`` and
asserted the handler recovered ``acme`` — and it passed, for the whole life of the
code, while the operator was minting ``staging-platform-acme-tenant`` and the
handler was publishing ``platform-acme`` to a metric dimension no reader ever
queried. The fixture agreed with the docstring instead of with the producer, so
the tests confirmed a belief rather than a behaviour, and the platform's entire
in-flight cost signal was a silent zero.

Every role ARN below is therefore the shape the operator actually mints
(``<cluster>-<platform>-tenant`` and ``<cluster>-<platform>-session``, from
operators/internal/controller/platform_iam.go and platform_session_iam.go), and
the assertions are on the value IAM returned rather than on any string this file
could derive. ROLE_SHAPE_IS_IRRELEVANT makes that explicit by attributing a role
whose name follows no contract at all.

Run: cd terraform/components/cost-pipeline/lambda && python -m unittest
"""

from __future__ import annotations

import base64
import gzip
import json
import os
import unittest
from unittest import mock

# boto3.client() at import time needs a region; set one before importing the
# module under test. No AWS calls are made — the clients are mocked per-test.
os.environ.setdefault("AWS_DEFAULT_REGION", "us-east-1")

import invocation_cost_publisher as iv  # noqa: E402

# The role names the operator really mints, and the cost identity it stamps on
# them. cluster_name is `<environment>-<clusterBase>` (terraform/live/*/env.hcl),
# so the cluster token is two words — which is exactly what the old prefix-strip
# got wrong.
CLUSTER = "staging-platform"
PLATFORM = "acme"
COST_ID = f"{CLUSTER}-{PLATFORM}"  # operators: platformCostID(cluster, name)
TENANT_ROLE = f"{CLUSTER}-{PLATFORM}-tenant"
SESSION_ROLE = f"{CLUSTER}-{PLATFORM}-session"


def _arn(role_name: str) -> str:
    return f"arn:aws:sts::123456789012:assumed-role/{role_name}/session-id"


def _envelope(records: list[dict]) -> dict:
    """Build a CloudWatch Logs subscription event from invocation records."""
    payload = {
        "messageType": "DATA_MESSAGE",
        "logEvents": [{"id": str(i), "message": json.dumps(r)} for i, r in enumerate(records)],
    }
    packed = gzip.compress(json.dumps(payload).encode("utf-8"))
    return {"awslogs": {"data": base64.b64encode(packed).decode("utf-8")}}


def _tagged(**tags: str) -> dict:
    """An IAM list_role_tags response."""
    return {"Tags": [{"Key": k, "Value": v} for k, v in tags.items()]}


class AttributionTestCase(unittest.TestCase):
    """Base for tests that resolve a PlatformId, with the lookup cache isolated.

    The cache lives for the life of an execution environment, so leaking it
    between tests would let one test's stub answer another test's lookup — and a
    test that passes because a previous test warmed a cache is exactly the kind
    of green this file exists to stop producing.
    """

    def setUp(self) -> None:
        iv._ROLE_PLATFORM_CACHE.clear()
        iv._UNTAGGED_ROLES.clear()

    def run_handler(self, records: list[dict], tags: dict | Exception | None = None):
        """Run the handler with IAM stubbed, returning (result, metrics, iam_mock)."""
        if tags is None:
            tags = _tagged(PlatformId=COST_ID)
        with (
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3"),
            mock.patch.object(iv, "iam") as iam,
        ):
            if isinstance(tags, Exception):
                iam.list_role_tags.side_effect = tags
            else:
                iam.list_role_tags.return_value = tags
            result = iv.handler(_envelope(records), None)
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        return result, metrics, iam


def _record(role_name: str = TENANT_ROLE, model: str = "us.anthropic.claude-sonnet-4-6") -> dict:
    return {
        "modelId": model,
        "input": {"inputTokenCount": 1_000_000},
        "output": {"outputTokenCount": 0},
        "identity": {"arn": _arn(role_name)},
    }


def _dims(metric: dict) -> dict[str, str]:
    return {d["Name"]: d["Value"] for d in metric["Dimensions"]}


class BareModelTest(unittest.TestCase):
    def test_strips_every_geo_prefix(self):
        base = "anthropic.claude-sonnet-4-6"
        for prefix in ("us", "eu", "jp", "ap", "apac", "global"):
            self.assertEqual(iv._bare_model(f"{prefix}.{base}"), base, prefix)

    def test_leaves_bare_provider_id_untouched(self):
        for bare in (
            "anthropic.claude-3-opus-20240229-v1:0",
            "anthropic.claude-sonnet-4-6",
            "amazon.nova-pro-v1:0",
        ):
            self.assertEqual(iv._bare_model(bare), bare)


class PlatformAttributionTest(AttributionTestCase):
    """The dimension published is the tag the operator wrote. Nothing else."""

    def test_publishes_the_tag_verbatim_for_a_tenant_role(self):
        # The assertion that would have caught the original defect. Under the old
        # prefix-strip this dimension was "platform-acme" — the environment token
        # removed from a role the operator names with the CLUSTER — while the
        # reconciler queried "acme". Equal-to-the-tag is the only relation that
        # cannot drift, because only one side computes the value.
        _result, metrics, _iam = self.run_handler([_record(TENANT_ROLE)])
        cost = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertEqual(_dims(cost)["PlatformId"], COST_ID)

    def test_attributes_the_session_role_family(self):
        # Attribution session roles end in `-session`. The old derivation only
        # recognised `-tenant` and returned the literal "unknown" for these, so
        # every attributed invocation — the whole point of that role family —
        # reached no Platform's budget.
        _result, metrics, _iam = self.run_handler([_record(SESSION_ROLE)])
        cost = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertEqual(_dims(cost)["PlatformId"], COST_ID)

    def test_role_name_shape_is_irrelevant(self):
        # No contract about role naming survives in this Lambda. A role called
        # anything at all attributes correctly as long as it carries the tag —
        # which is the property that makes a rename on the operator's side unable
        # to silently change what gets published.
        _result, metrics, _iam = self.run_handler([_record("something-nobody-agreed-on")])
        cost = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertEqual(_dims(cost)["PlatformId"], COST_ID)

    def test_role_without_the_tag_is_unknown_and_published(self):
        # Spend that reaches no budget has to be visible as its own signal, not
        # absorbed as a quiet shortfall in somebody else's number.
        result, metrics, _iam = self.run_handler(
            [_record(TENANT_ROLE)], tags=_tagged(Tenant="acme-team")
        )
        cost = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertEqual(_dims(cost)["PlatformId"], "unknown")
        self.assertEqual(result["unattributed"], 1)
        gap = next(m for m in metrics if m["MetricName"] == iv.UNATTRIBUTED_METRIC)
        self.assertEqual(_dims(gap)["RoleName"], TENANT_ROLE)

    def test_identity_that_is_not_an_assumed_role_is_unknown(self):
        _result, metrics, iam = self.run_handler(
            [
                {
                    "modelId": "us.anthropic.claude-sonnet-4-6",
                    "input": {"inputTokenCount": 1_000_000},
                    "output": {"outputTokenCount": 0},
                    "identity": {"arn": "arn:aws:iam::123456789012:user/someone"},
                }
            ]
        )
        cost = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertEqual(_dims(cost)["PlatformId"], "unknown")
        iam.list_role_tags.assert_not_called()

    def test_lookup_is_memoized_per_role(self):
        # A log batch is overwhelmingly the same few roles and IAM's endpoint is
        # global and throttled, so one lookup per record would be the bottleneck.
        _result, _metrics, iam = self.run_handler([_record(TENANT_ROLE) for _ in range(5)])
        self.assertEqual(iam.list_role_tags.call_count, 1)

    def test_iam_failure_is_not_cached(self):
        # A throttle must not pin a role to "unknown" for the rest of the warm
        # container's life — that would turn a transient blip into hours of spend
        # attributed to nobody, long after IAM recovered.
        iv._ROLE_PLATFORM_CACHE.clear()
        with mock.patch.object(iv, "iam") as iam:
            iam.list_role_tags.side_effect = RuntimeError("Throttling")
            self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), "unknown")
        self.assertNotIn(TENANT_ROLE, iv._ROLE_PLATFORM_CACHE)
        with mock.patch.object(iv, "iam") as iam:
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), COST_ID)

    def test_definitive_absence_is_cached(self):
        # A role that exists and has no PlatformId will not grow one mid-batch,
        # so it is answered once rather than re-queried per record.
        with mock.patch.object(iv, "iam") as iam:
            iam.list_role_tags.return_value = _tagged(Tenant="acme-team")
            for _ in range(3):
                self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), "unknown")
            self.assertEqual(iam.list_role_tags.call_count, 1)
        # Still counted every time, so the published gap reflects invocations
        # rather than cache misses.
        self.assertEqual(iv._UNTAGGED_ROLES[TENANT_ROLE], 3)


class EstimateCostTest(AttributionTestCase):
    def test_prices_a_current_model_via_inference_profile(self):
        with mock.patch.object(iv, "iam") as iam:
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            record = {
                "modelId": "us.anthropic.claude-sonnet-4-6",
                "input": {"inputTokenCount": 1_000_000},
                "output": {"outputTokenCount": 1_000_000},
                "identity": {"arn": _arn(TENANT_ROLE)},
            }
            cost, platform, model, _in, _out, priced = iv._estimate_cost(record)
        self.assertTrue(priced)
        # 3.0 + 15.0 per million in+out
        self.assertAlmostEqual(cost, 18.0, places=4)
        self.assertEqual(model, "anthropic.claude-sonnet-4-6")
        self.assertEqual(platform, COST_ID)

    def test_unknown_model_is_unpriced_not_borrowed(self):
        with mock.patch.object(iv, "iam") as iam:
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            record = {
                "modelId": "us.anthropic.claude-imaginary-9-9",
                "input": {"inputTokenCount": 5_000_000},
                "output": {"outputTokenCount": 5_000_000},
                "identity": {"arn": _arn(TENANT_ROLE)},
            }
            cost, _platform, _model, _in, _out, priced = iv._estimate_cost(record)
        self.assertFalse(priced)
        self.assertEqual(cost, 0.0)


class HandlerTest(AttributionTestCase):
    def test_unpriced_model_emits_unpriced_metric_only(self):
        result, metrics, _iam = self.run_handler(
            [_record(TENANT_ROLE, model="us.anthropic.claude-imaginary-9-9")]
        )
        self.assertEqual(result["unpriced"], 1)
        self.assertEqual(result["platforms"], 0)
        names = {m["MetricName"] for m in metrics}
        self.assertIn(iv.UNPRICED_METRIC, names)
        self.assertNotIn(iv.METRIC_NAME, names)
        unpriced = next(m for m in metrics if m["MetricName"] == iv.UNPRICED_METRIC)
        dims = _dims(unpriced)
        self.assertEqual(dims["PlatformId"], COST_ID)
        self.assertEqual(dims["ModelId"], "anthropic.claude-imaginary-9-9")

    def test_priced_model_emits_cost_metric(self):
        result, metrics, _iam = self.run_handler([_record(TENANT_ROLE)])
        self.assertEqual(result["platforms"], 1)
        self.assertEqual(result["unpriced"], 0)
        cost_metric = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertAlmostEqual(cost_metric["Value"], 3.0, places=4)

    def test_estimate_records_carry_the_same_identity_as_the_metric(self):
        # The reconciliation view joins these estimate rows to CUR on platform_id.
        # When the metric dimension and the estimate column disagree the view
        # renders every row as 'no_cur_row', which reads on a dashboard exactly
        # like "no disagreement found".
        with (
            mock.patch.object(iv, "cloudwatch"),
            mock.patch.object(iv, "s3") as s3,
            mock.patch.object(iv, "iam") as iam,
            mock.patch.object(iv, "ESTIMATE_BUCKET", "estimates-bucket"),
        ):
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            iv.handler(_envelope([_record(TENANT_ROLE)]), None)
        body = s3.put_object.call_args.kwargs["Body"].decode("utf-8")
        rows = [json.loads(line) for line in body.splitlines()]
        self.assertEqual({r["platform_id"] for r in rows}, {COST_ID})


class ImportedModelTest(AttributionTestCase):
    """Custom Model Import (open-weight) models: unpriced-but-observable by
    default, priced at the configured per-token governance estimate when set."""

    ARN = "arn:aws:bedrock:us-west-2:123456789012:imported-model/abc123"

    def _record(self, in_tok: int = 1_000_000, out_tok: int = 1_000_000) -> dict:
        return {
            "modelId": self.ARN,
            "input": {"inputTokenCount": in_tok},
            "output": {"outputTokenCount": out_tok},
            "identity": {"arn": _arn(TENANT_ROLE)},
        }

    def test_detection_and_key(self):
        self.assertTrue(iv._is_imported(self.ARN))
        self.assertFalse(iv._is_imported("us.anthropic.claude-sonnet-4-6"))
        self.assertEqual(iv._imported_key(self.ARN), "imported/abc123")

    def test_unpriced_without_estimate(self):
        # Default estimate 0 → imported invocation is unpriced (surfaced as an
        # UnpricedInvocations count), never a borrowed rate. Model key is the
        # compact imported id, not the raw ARN.
        with (
            mock.patch.object(iv, "IMPORTED_ESTIMATE_USD_PER_M", 0.0),
            mock.patch.object(iv, "iam") as iam,
        ):
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            cost, platform, model, _in, _out, priced = iv._estimate_cost(self._record())
        self.assertFalse(priced)
        self.assertEqual(cost, 0.0)
        self.assertEqual(model, "imported/abc123")
        self.assertEqual(platform, COST_ID)

    def test_priced_with_estimate(self):
        # A configured per-token estimate prices input+output tokens so imported
        # spend reaches the kill-switch cost signal.
        with (
            mock.patch.object(iv, "IMPORTED_ESTIMATE_USD_PER_M", 4.0),
            mock.patch.object(iv, "iam") as iam,
        ):
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            cost, _platform, model, _in, _out, priced = iv._estimate_cost(self._record(1_000_000, 1_000_000))
        self.assertTrue(priced)
        self.assertAlmostEqual(cost, 8.0, places=4)  # (1M + 1M)/1M * 4.0
        self.assertEqual(model, "imported/abc123")

    def test_handler_prices_imported_when_estimate_set(self):
        with (
            mock.patch.object(iv, "IMPORTED_ESTIMATE_USD_PER_M", 4.0),
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3"),
            mock.patch.object(iv, "iam") as iam,
        ):
            iam.list_role_tags.return_value = _tagged(PlatformId=COST_ID)
            result = iv.handler(_envelope([self._record(1_000_000, 0)]), None)
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        self.assertEqual(result["platforms"], 1)
        self.assertEqual(result["unpriced"], 0)
        cost_metric = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertAlmostEqual(cost_metric["Value"], 4.0, places=4)  # 1M/1M * 4.0
        tok = next(m for m in metrics if m["MetricName"] == iv.TOKENS_IN_METRIC)
        self.assertEqual(_dims(tok)["ModelId"], "imported/abc123")


if __name__ == "__main__":
    unittest.main()
