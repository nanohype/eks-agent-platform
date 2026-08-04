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
import re
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock

# boto3.client() at import time needs a region; set one before importing the
# module under test. No AWS calls are made — the clients are mocked per-test.
os.environ.setdefault("AWS_DEFAULT_REGION", "us-east-1")

from botocore.exceptions import ClientError  # noqa: E402

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
PLATFORM_ID_TAG_KEY = "PlatformId"


def _arn(role_name: str) -> str:
    return f"arn:aws:sts::123456789012:assumed-role/{role_name}/session-id"


# Every event in a CloudWatch Logs subscription payload carries a millisecond
# timestamp set by the producer, and the handler partitions the estimate export by it.
# The fixture stamps one on every event because a payload without one is not a shape
# the service delivers — leaving it off would exercise only the fallback.
EVENT_TIME = datetime(2026, 3, 14, 15, 9, 26, tzinfo=timezone.utc)
EVENT_MS = int(EVENT_TIME.timestamp() * 1000)
EVENT_DAY = EVENT_TIME.strftime("%Y-%m-%d")


def _envelope(records: list[dict], timestamps: list[int] | None = None) -> dict:
    """Build a CloudWatch Logs subscription event from invocation records."""
    if timestamps is None:
        timestamps = [EVENT_MS] * len(records)
    payload = {
        "messageType": "DATA_MESSAGE",
        "logEvents": [
            {"id": str(i), "timestamp": ts, "message": json.dumps(r)}
            for i, (r, ts) in enumerate(zip(records, timestamps, strict=True))
        ],
    }
    packed = gzip.compress(json.dumps(payload).encode("utf-8"))
    return {"awslogs": {"data": base64.b64encode(packed).decode("utf-8")}}


def _pages(*pages: list[tuple[str, str]]) -> list[dict]:
    """IAM ListRoleTags pages, in the shape the paginator yields."""
    return [{"Tags": [{"Key": k, "Value": v} for k, v in page]} for page in pages]


def _tagged(**tags: str) -> list[dict]:
    """A single-page IAM ListRoleTags response."""
    return _pages(list(tags.items()))


def _stub_iam(iam, pages_or_exc):
    """Wire a mocked IAM client to answer list_role_tags through its paginator.

    Each call gets a FRESH iterator: a paginator returns a generator, and a stub
    that hands out one exhausted iterator would make the second lookup in a test
    silently see zero tags — which is the failure this module exists to not have.
    Returns the paginate mock so call counts can be asserted.
    """
    paginator = iam.get_paginator.return_value
    if isinstance(pages_or_exc, Exception):
        paginator.paginate.side_effect = pages_or_exc
    else:
        paginator.paginate.side_effect = lambda **_kw: iter(pages_or_exc)
    return paginator.paginate


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
        iv._BATCH_FAILED_ROLES.clear()

    def run_handler(self, records: list[dict], tags: dict | Exception | None = None):
        """Run the handler with IAM stubbed, returning (result, metrics, iam_mock)."""
        if tags is None:
            tags = _tagged(PlatformId=COST_ID)
        with (
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3"),
            mock.patch.object(iv, "iam") as iam,
        ):
            _stub_iam(iam, tags)
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
        iam.get_paginator.assert_not_called()

    def test_reads_every_page_of_tags(self):
        # ListRoleTags is paginated, and not for the reason it looks like: a role
        # caps at 50 tags and MaxItems defaults to 100, so a full page is never the
        # trigger. AWS documents that IAM "might return fewer results, even when
        # there are more results available" with IsTruncated set, and recommends
        # checking after every call — so the service, not the tag count, decides
        # where the split falls. A single-page read puts PlatformId behind a page
        # boundary the caller cannot predict, and the miss is indistinguishable
        # from an untagged role: the invocation attributes to "unknown" and the
        # tenant's spend reads low, with nothing red.
        with mock.patch.object(iv, "iam") as iam:
            _stub_iam(iam, _pages(
                [("Team", "acme-team"), ("Environment", "staging")],
                [("PlatformId", COST_ID)],
            ))
            self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), COST_ID)

    def test_lookup_is_memoized_per_role(self):
        # A log batch is overwhelmingly the same few roles and IAM's endpoint is
        # global and throttled, so one lookup per record would be the bottleneck.
        _result, _metrics, iam = self.run_handler([_record(TENANT_ROLE) for _ in range(5)])
        self.assertEqual(iam.get_paginator.return_value.paginate.call_count, 1)

    def test_access_denied_is_a_settled_answer_and_is_cached(self):
        # The log group is account-wide — Bedrock invocation logging is one
        # configuration per account+region, so every principal's invocations land in
        # it, not just tenants'. The grant is scoped to the operator's IAM path, so
        # AccessDenied on everything else is the DESIGN, not an incident, and it is
        # the common case rather than the exception. Not caching it would mean one
        # IAM call per log RECORD for every non-tenant Bedrock caller in the account,
        # against a global throttled endpoint, in the hot path of a firehose.
        for code in ("AccessDenied", "NoSuchEntity"):
            with self.subTest(code=code):
                iv._ROLE_PLATFORM_CACHE.clear()
                err = ClientError({"Error": {"Code": code, "Message": "x"}}, "ListRoleTags")
                with mock.patch.object(iv, "iam") as iam:
                    paginate = _stub_iam(iam, err)
                    for _ in range(4):
                        self.assertEqual(iv._platform_id_for_role("some-other-role"), "unknown")
                    self.assertEqual(paginate.call_count, 1)
                self.assertEqual(iv._ROLE_PLATFORM_CACHE["some-other-role"], "unknown")

    def test_a_transient_failure_is_asked_once_per_batch_not_once_per_record(self):
        # The amplification this prevents is not one extra request. botocore's
        # default retry mode is legacy — up to 5 attempts with rand()*2^(n-1)
        # backoff — so a sustained throttle costs seconds of sleeping PER lookup,
        # against a 30s function timeout. A handful of such records exhausts it,
        # and every _emit_* call happens after the record loop, so a timeout
        # mid-loop publishes NOTHING: the whole batch's cost metric, token metrics
        # and estimate export are lost, CloudWatch Logs re-invokes, and the retry
        # walks into the same throttle. One degraded record must not become a
        # silent zero for the batch.
        throttle = ClientError({"Error": {"Code": "Throttling", "Message": "slow down"}}, "ListRoleTags")
        _result, _metrics, iam = self.run_handler([_record(TENANT_ROLE) for _ in range(25)], tags=throttle)
        self.assertEqual(iam.get_paginator.return_value.paginate.call_count, 1)
        # Still not cached across invocations — the next batch gets a fresh look.
        self.assertNotIn(TENANT_ROLE, iv._ROLE_PLATFORM_CACHE)

    def test_a_transient_failure_is_forgotten_at_the_next_batch(self):
        # The other half of the two scopes. Within a batch a failed role is not
        # re-asked; ACROSS batches it must be, because a throttle pinning a real
        # tenant to "unknown" for the life of a warm container turns a blip into
        # hours of spend attributed to nobody, long after IAM recovered.
        iv._ROLE_PLATFORM_CACHE.clear()
        with mock.patch.object(iv, "iam") as iam:
            _stub_iam(iam, ClientError({"Error": {"Code": "Throttling", "Message": "slow down"}}, "ListRoleTags"))
            self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), "unknown")
        self.assertNotIn(TENANT_ROLE, iv._ROLE_PLATFORM_CACHE)
        self.assertIn(TENANT_ROLE, iv._BATCH_FAILED_ROLES)

        # A new invocation. The handler clears the batch memo; the long-lived cache
        # is untouched, so the role gets a genuinely fresh look.
        iv._BATCH_FAILED_ROLES.clear()
        with mock.patch.object(iv, "iam") as iam:
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), COST_ID)

    def test_definitive_absence_is_cached(self):
        # A role that exists and has no PlatformId will not grow one mid-batch,
        # so it is answered once rather than re-queried per record.
        with mock.patch.object(iv, "iam") as iam:
            _stub_iam(iam, _tagged(Tenant="acme-team"))
            for _ in range(3):
                self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), "unknown")
            self.assertEqual(iam.get_paginator.return_value.paginate.call_count, 1)
        # Still counted every time, so the published gap reflects invocations
        # rather than cache misses.
        self.assertEqual(iv._UNTAGGED_ROLES[TENANT_ROLE], 3)


class RealBotoCallShapeTest(unittest.TestCase):
    """The one test that is not talking to a MagicMock.

    Every other IAM assertion in this file goes through `mock.patch.object(iv, "iam")`,
    and a MagicMock accepts any operation name, any kwarg, and returns whatever the
    test handed it. That proves the code reads what the MOCK returns — it cannot
    notice a misspelled paginator name, a wrong parameter, or a response key that
    does not exist. Which is the same category of green the old prefix-strip tests
    produced: a fixture agreeing with the author instead of with the service.

    botocore's Stubber validates both directions against the real service model, so
    this pins the call shape. Nothing here reaches the network.
    """

    def test_the_lookup_matches_the_real_iam_api(self):
        import boto3
        from botocore.stub import Stubber

        client = boto3.client("iam", region_name="us-east-1")
        stubber = Stubber(client)
        # Rejected at add_response time if ListRoleTags does not take RoleName, or
        # if the response does not carry Tags/IsTruncated in this shape.
        stubber.add_response(
            "list_role_tags",
            {"Tags": [{"Key": PLATFORM_ID_TAG_KEY, "Value": COST_ID}], "IsTruncated": False},
            {"RoleName": TENANT_ROLE},
        )
        with stubber, mock.patch.object(iv, "iam", client):
            iv._ROLE_PLATFORM_CACHE.clear()
            self.assertEqual(iv._platform_id_for_role(TENANT_ROLE), COST_ID)
        stubber.assert_no_pending_responses()

    def test_the_tag_key_is_the_one_cost_explorer_activates(self):
        # Case-sensitive in Billing, and the CUR column is derived from it. A
        # lowercase key activates nothing and the reconciler's column never exists.
        self.assertEqual(iv.PLATFORM_ID_TAG, "PlatformId")


class EstimateCostTest(AttributionTestCase):
    def test_prices_a_current_model_via_inference_profile(self):
        with mock.patch.object(iv, "iam") as iam:
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
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
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
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
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
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
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
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
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
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
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            result = iv.handler(_envelope([self._record(1_000_000, 0)]), None)
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        self.assertEqual(result["platforms"], 1)
        self.assertEqual(result["unpriced"], 0)
        cost_metric = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertAlmostEqual(cost_metric["Value"], 4.0, places=4)  # 1M/1M * 4.0
        tok = next(m for m in metrics if m["MetricName"] == iv.TOKENS_IN_METRIC)
        self.assertEqual(_dims(tok)["ModelId"], "imported/abc123")


# ───────────────────────────────────────────────────────────────────────────────
# The estimate export against the table that reads it.
#
# This Lambda writes NDJSON objects. A Glue table declared in main.tf — which this
# file's code never imports, references, or can see — reads them by S3 prefix and
# by column name. Nothing links the two: no shared constant, no generated file, no
# API call between them. They agree because two people wrote the same strings in
# two languages, and the day they stop agreeing is a day nothing reports.
#
# Every way they can disagree fails the same way, which is the reason this exists:
#
#   wrong prefix or partition spelling  Athena projects locations holding no
#                                       objects. Zero rows, query exits zero.
#   wrong date format                   same, one partition per day, forever.
#   renamed column                      the JSON SerDe returns NULL for a column
#                                       absent from the object and ignores an
#                                       object key absent from the schema, so a
#                                       rename on either side is not an error —
#                                       SUM(estimate_usd) is simply NULL.
#   wrong declared type                 SUM() over a string column fails the
#                                       query, which reads as unreadable spend.
#
# And the reconciliation view LEFT JOINs the estimate leg onto CUR, so an estimate
# leg that returns nothing renders as a reconciliation that found nothing to
# disagree about.
#
# So the declaration is parsed out of the terraform and compared against what the
# handler really put. Extraction in that direction on purpose: the terraform is the
# side that can be read exactly, and the Python side is exercised rather than
# read — the assertions below are on the object a real handler run produced.
# ───────────────────────────────────────────────────────────────────────────────

MAIN_TF = Path(__file__).resolve().parent.parent / "main.tf"


def _block_at(src: str, open_brace: int) -> str:
    """Body of the HCL block whose opening brace is at `open_brace`.

    Braces are counted only outside double-quoted strings, so an interpolation like
    "${local.estimate_prefix}" is not read as a nested block.
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
    raise AssertionError(f"unterminated HCL block at offset {open_brace}")


def _hcl_block(src: str, header: str) -> str:
    """Body of the HCL block introduced by `header`."""
    return _block_at(src, src.index("{", src.index(header) + len(header)))


def _typed_blocks(body: str, kind: str) -> dict[str, str]:
    """`kind { ... }` sub-blocks of an HCL body, as {name: type}.

    Each block is brace-matched and then read for its own name and type, rather than
    matched as one flat `{ name = .. type = .. }` pattern. A columns block is free to
    carry a `#` comment or Glue's own `comment` argument, and the flat pattern drops
    such a block SILENTLY — the worst failure available to this parser. A column the
    table declares and this file never sees is one the comparison downstream can
    neither find missing nor find extra, so the gate goes quiet on exactly the change
    it exists to catch.

    A block that yields no name or no type raises instead of vanishing, and two
    blocks collapsing onto one name raises too.
    """
    found: dict[str, str] = {}
    blocks = 0
    for m in re.finditer(r"\b" + re.escape(kind) + r"\s*\{", body):
        blocks += 1
        block = _block_at(body, m.end() - 1)
        found[_hcl_arg(block, "name")] = _hcl_arg(block, "type")
    assert len(found) == blocks, f"{kind}: {blocks} blocks yielded {len(found)} distinct names"
    return found


def _hcl_arg(body: str, name: str) -> str:
    """A single `name = "literal"` or `"quoted.name" = "literal"` argument."""
    m = re.search(r'^\s*"?' + re.escape(name) + r'"?\s*=\s*"([^"]*)"\s*$', body, re.M)
    assert m, f"no literal argument {name!r} found"
    return m.group(1)


def _hcl_local(src: str, name: str) -> str:
    """A `name = "literal"` from any of the file's locals blocks.

    main.tf declares several, and the one holding a given value is not the first.
    """
    for m in re.finditer(r"^locals\s*\{", src, re.M):
        try:
            return _hcl_arg(_block_at(src, m.end() - 1), name)
        except AssertionError:
            continue
    raise AssertionError(f"no local {name!r} in any locals block")


# Glue's date-projection format is Java's, the handler's is strftime's. Translating
# rather than restating means changing the terraform side alone fails this file.
_GLUE_DATE_TOKENS = (("yyyy", "%Y"), ("MM", "%m"), ("dd", "%d"))


def _strftime_of(glue_format: str) -> str:
    out = glue_format
    for java, c in _GLUE_DATE_TOKENS:
        out = out.replace(java, c)
    # Anything alphabetic left over is a Java token with no translation here, and
    # translating it wrong would be worse than refusing: the comparison downstream
    # would still pass while describing a different day.
    leftover = re.sub(r"%.", "", out)
    assert not re.search(r"[a-zA-Z]", leftover), f"untranslated token in {glue_format!r}"
    return out


# A Glue type against the Python value the handler put in that field. bool is
# excluded from both numeric checks because isinstance(True, int) is True.
_GLUE_TYPE_ACCEPTS = {
    "string": lambda v: isinstance(v, str),
    "double": lambda v: isinstance(v, (int, float)) and not isinstance(v, bool),
    "bigint": lambda v: isinstance(v, int) and not isinstance(v, bool),
}


class DeclaredEstimateTable:
    """The estimates Glue table as terraform declares it."""

    def __init__(self) -> None:
        src = MAIN_TF.read_text()
        table = _hcl_block(src, 'resource "aws_glue_catalog_table" "estimates"')
        self.columns = _typed_blocks(table, "columns")
        self.partition_keys = _typed_blocks(table, "partition_keys")
        self.date_format = _hcl_arg(table, "projection.usage_date.format")
        self.projection_range = _hcl_arg(table, "projection.usage_date.range")
        self.serde = _hcl_arg(table, "serialization_library")
        self.prefix = _hcl_local(src, "estimate_prefix")

        # `${local.x}` resolved so the template can be compared against a real key.
        # A `${resource...}` reference is left standing — terraform is the only thing
        # that can resolve it, and the comparison below is on the tail regardless.
        self.location_template = re.sub(
            r"\$\{\s*local\.(\w+)\s*\}",
            lambda m: _hcl_local(src, m.group(1)),
            _hcl_arg(table, "storage.location.template"),
        )

        # If any of these came back empty the parse silently found nothing, and every
        # comparison below would pass against an empty declaration.
        assert self.columns, "parsed no columns out of the estimates table"
        assert len(self.partition_keys) == 1, f"expected one partition key, got {self.partition_keys}"
        assert self.prefix, "parsed no estimate_prefix out of locals"

    @property
    def partition_key(self) -> str:
        return next(iter(self.partition_keys))


DECLARED = DeclaredEstimateTable()


class DeclaredEstimateTableParseTest(unittest.TestCase):
    """The parser above is load-bearing, so it is checked against known values.

    Every assertion in the class after this one compares the handler to whatever this
    parse produced. A parse that quietly matched nothing would make all of them
    vacuous, so the shape it recovers is pinned here — not the values themselves,
    which live in main.tf, but that it recovered a plausible set at all.
    """

    def test_the_parse_recovered_a_real_table(self):
        self.assertEqual(DECLARED.prefix, "estimates")
        self.assertEqual(DECLARED.partition_key, "usage_date")
        self.assertEqual(DECLARED.date_format, "yyyy-MM-dd")
        # A lower bound, not the exact count. The exact count is not this test's job:
        # _typed_blocks now raises rather than dropping a block it cannot read, so a
        # column added to main.tf reaches DECLARED.columns and is caught by the set
        # equality against the handler's real output. Pinning a number here instead
        # would only mean editing it whenever the schema legitimately grows.
        self.assertGreaterEqual(len(DECLARED.columns), 4, "the parse recovered implausibly few columns")

        unhandled = set(DECLARED.columns.values()) - set(_GLUE_TYPE_ACCEPTS)
        self.assertFalse(unhandled, f"no Python check defined for Glue type(s) {unhandled}")

    def test_a_column_block_carrying_a_comment_is_still_seen(self):
        # The flat-pattern version of _typed_blocks dropped these silently, which left
        # every comparison downstream vacuous for that column. Both spellings of
        # "commented" are exercised: Glue's own `comment` argument and a bare `#`.
        body = """
          columns {
            name = "platform_id"
            type = "string"
          }
          columns {
            # the region the invocation was billed in
            name    = "region"
            type    = "string"
            comment = "not written by the publisher"
          }
        """
        self.assertEqual(_typed_blocks(body, "columns"), {"platform_id": "string", "region": "string"})

    def test_an_unreadable_column_block_raises_rather_than_vanishing(self):
        with self.assertRaises(AssertionError):
            _typed_blocks('columns {\n  name = "x"\n}', "columns")

    def test_the_java_date_format_translates(self):
        self.assertEqual(_strftime_of("yyyy-MM-dd"), "%Y-%m-%d")
        with self.assertRaises(AssertionError):
            _strftime_of("yyyy-MMM-dd")


class EstimateExportMatchesItsTableTest(AttributionTestCase):
    def _put_kwargs(self, records=None, timestamps=None):
        """Run the handler for real and return what it actually asked S3 to store."""
        with (
            mock.patch.object(iv, "cloudwatch"),
            mock.patch.object(iv, "s3") as s3,
            mock.patch.object(iv, "iam") as iam,
            mock.patch.object(iv, "ESTIMATE_BUCKET", "estimates-bucket"),
        ):
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            iv.handler(_envelope(records or [_record(TENANT_ROLE)], timestamps), None)
        return [c.kwargs for c in s3.put_object.call_args_list]

    def _rows(self, kwargs):
        return [json.loads(line) for line in kwargs["Body"].decode("utf-8").splitlines()]

    # ── the address ────────────────────────────────────────────────────────────

    def test_the_prefix_fallback_is_the_declared_prefix(self):
        # ESTIMATE_PREFIX comes from the environment terraform sets, but the module
        # default is what runs if that wiring is ever dropped — and a put to the wrong
        # prefix is swallowed, so the fallback has to be the declared value rather
        # than a convenient one.
        #
        # Neither variable is set in this process, which is what makes the resolved
        # module attribute BE the fallback. Stated as an assertion rather than assumed:
        # an environment that set them would turn every check below into a comparison
        # of the environment against itself.
        self.assertNotIn("ESTIMATE_PREFIX", os.environ)
        self.assertEqual(iv.ESTIMATE_PREFIX, DECLARED.prefix)

    def test_no_bucket_fallback_disables_the_export_rather_than_guessing_one(self):
        # The opposite choice for the bucket, and deliberately so: a default bucket
        # name would be a write into somewhere real. Empty means no export at all.
        self.assertNotIn("ESTIMATE_BUCKET", os.environ)
        self.assertEqual(iv.ESTIMATE_BUCKET, "")
        with (
            mock.patch.object(iv, "cloudwatch"),
            mock.patch.object(iv, "s3") as s3,
            mock.patch.object(iv, "iam") as iam,
            mock.patch.object(iv, "ESTIMATE_BUCKET", ""),
        ):
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            iv.handler(_envelope([_record(TENANT_ROLE)]), None)
        s3.put_object.assert_not_called()

    def test_the_object_key_lands_in_the_partition_the_table_projects(self):
        key = self._put_kwargs()[0]["Key"]
        prefix, partition, leaf = key.split("/")

        self.assertEqual(prefix, DECLARED.prefix)
        self.assertTrue(leaf.endswith(".json"), leaf)

        # The partition directory, spelled the way the table's location template
        # spells it — `usage_date=` — rather than the way this file would guess.
        name, _, value = partition.partition("=")
        self.assertEqual(name, DECLARED.partition_key)

        # And the value has to round-trip the declared Java format through strftime.
        # Parsing alone is too lenient (strptime accepts an unpadded month); the
        # round-trip is what pins the exact rendering Athena will look for.
        fmt = _strftime_of(DECLARED.date_format)
        self.assertEqual(datetime.strptime(value, fmt).strftime(fmt), value)

    def test_the_key_prefix_is_the_tail_of_the_tables_location_template(self):
        # The template's leading `s3://<bucket>/` is a terraform interpolation this
        # file cannot resolve, so the comparison is on the tail that addresses the
        # object: the prefix, the partition key, and the substitution.
        expected_tail = f"/{DECLARED.prefix}/{DECLARED.partition_key}=$${{{DECLARED.partition_key}}}"
        self.assertTrue(
            DECLARED.location_template.endswith(expected_tail),
            f"{DECLARED.location_template!r} does not end with {expected_tail!r}",
        )

        key = self._put_kwargs()[0]["Key"]
        self.assertTrue(key.startswith(f"{DECLARED.prefix}/{DECLARED.partition_key}="), key)

    def test_the_projection_range_stays_open_at_the_top(self):
        # A closed range puts TODAY outside the projection on a daily partition, so
        # the most recent day is never queryable and the reconciliation's newest row
        # is always missing — which reads as a quiet day, not as a broken table.
        self.assertTrue(DECLARED.projection_range.endswith(",NOW"), DECLARED.projection_range)

    # ── the schema ─────────────────────────────────────────────────────────────

    def test_the_record_carries_exactly_the_declared_columns(self):
        rows = self._rows(self._put_kwargs()[0])
        self.assertTrue(rows)
        for row in rows:
            # Set equality both ways. A column declared but never written reads NULL
            # for every row; a key written but never declared is dropped by the SerDe.
            # Neither is an error at query time, so neither can be a subset check here.
            self.assertEqual(set(row), set(DECLARED.columns))

    def test_every_value_fits_the_type_its_column_declares(self):
        for row in self._rows(self._put_kwargs()[0]):
            for column, glue_type in DECLARED.columns.items():
                accepts = _GLUE_TYPE_ACCEPTS.get(glue_type)
                self.assertIsNotNone(accepts, f"unhandled Glue type {glue_type!r} on {column}")
                self.assertTrue(
                    accepts(row[column]),
                    f"{column} is declared {glue_type} but the publisher wrote {row[column]!r}",
                )

    def test_the_serde_is_the_json_one_the_publisher_writes_for(self):
        # NDJSON: one JSON object per line. Both JSON SerDes require exactly that, and
        # the body below is the real one.
        self.assertIn("JsonSerDe", DECLARED.serde)
        body = self._put_kwargs()[0]["Body"].decode("utf-8")
        self.assertTrue(body)
        for line in body.splitlines():
            self.assertIsInstance(json.loads(line), dict)

    # ── the day ────────────────────────────────────────────────────────────────

    def test_the_partition_day_is_the_invocations_own_event_time(self):
        # Not the handler's wall clock. The CUR leg of the reconciliation buckets by
        # line_item_usage_start_date, so an estimate stamped at publish time joins
        # against the wrong day's bill.
        key = self._put_kwargs()[0]["Key"]
        self.assertIn(f"{DECLARED.partition_key}={EVENT_DAY}", key)

    def test_a_batch_straddling_utc_midnight_writes_both_days(self):
        # CloudWatch Logs batches by arrival, not by day, and retries a failed
        # delivery — so a batch holding both sides of midnight is ordinary. Stamping
        # one day on all of it moves the tail of a day into the next one, and both
        # days still have rows, so nothing looks wrong.
        before = datetime(2026, 3, 14, 23, 59, 30, tzinfo=timezone.utc)
        after = datetime(2026, 3, 15, 0, 0, 30, tzinfo=timezone.utc)
        puts = self._put_kwargs(
            records=[_record(TENANT_ROLE), _record(TENANT_ROLE)],
            timestamps=[int(before.timestamp() * 1000), int(after.timestamp() * 1000)],
        )
        days = {k["Key"].split("/")[1].partition("=")[2] for k in puts}
        self.assertEqual(days, {"2026-03-14", "2026-03-15"})

    def test_the_token_metric_is_not_split_by_day(self):
        # The day exists for the export's partitioning and must not leak into the
        # metric emission, which something else already graphs. Not a double-count —
        # two partials on identical dimensions sum back to the same total — but this
        # emission should stay exactly what it was before the export gained a day.
        before = datetime(2026, 3, 14, 23, 59, 30, tzinfo=timezone.utc)
        after = datetime(2026, 3, 15, 0, 0, 30, tzinfo=timezone.utc)
        with (
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3"),
            mock.patch.object(iv, "iam") as iam,
        ):
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            iv.handler(
                _envelope(
                    [_record(TENANT_ROLE), _record(TENANT_ROLE)],
                    [int(before.timestamp() * 1000), int(after.timestamp() * 1000)],
                ),
                None,
            )
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        tokens_in = [m for m in metrics if m["MetricName"] == iv.TOKENS_IN_METRIC]
        self.assertEqual(len(tokens_in), 1)
        self.assertEqual(tokens_in[0]["Value"], 2_000_000.0)

    # ── the swallow ────────────────────────────────────────────────────────────

    def test_a_failed_export_is_counted_rather_than_only_logged(self):
        # The put has to be swallowed — an S3 hiccup must not poison the metric path
        # the kill switch reads, or trigger a log-subscription retry. That makes it the
        # component's most silent seam: a grant that misses the prefix produces a
        # warning in a log group nothing watches while the handler reports success.
        # The counter is what makes a misconfigured export distinguishable from an
        # account nobody used.
        with (
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3") as s3,
            mock.patch.object(iv, "iam") as iam,
            mock.patch.object(iv, "ESTIMATE_BUCKET", "estimates-bucket"),
        ):
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            s3.put_object.side_effect = ClientError(
                {"Error": {"Code": "AccessDenied", "Message": "denied"}}, "PutObject"
            )
            result = iv.handler(_envelope([_record(TENANT_ROLE)]), None)

        # Still swallowed: the handler succeeds and the cost metric still published.
        self.assertEqual(result["status"], "ok")
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        self.assertIn(iv.METRIC_NAME, {m["MetricName"] for m in metrics})

        failures = [m for m in metrics if m["MetricName"] == iv.EXPORT_FAILURE_METRIC]
        self.assertEqual(len(failures), 1)
        self.assertEqual(failures[0]["Value"], 1.0)

    def test_a_successful_export_publishes_no_failure_metric(self):
        # A counter that is always present is one an alarm cannot use.
        with (
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3"),
            mock.patch.object(iv, "iam") as iam,
            mock.patch.object(iv, "ESTIMATE_BUCKET", "estimates-bucket"),
        ):
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            iv.handler(_envelope([_record(TENANT_ROLE)]), None)
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        self.assertNotIn(iv.EXPORT_FAILURE_METRIC, {m["MetricName"] for m in metrics})

    # ── the call ───────────────────────────────────────────────────────────────

    def test_the_put_matches_the_real_s3_api(self):
        # Same reason as RealBotoCallShapeTest: a MagicMock accepts any kwarg, so
        # every assertion above would survive a misspelled ServerSideEncryption or an
        # SSEKMSKeyId that S3 does not take. The Stubber validates against the service
        # model. Nothing here reaches the network.
        import boto3
        from botocore.stub import Stubber

        client = boto3.client("s3", region_name="us-east-1")
        stubber = Stubber(client)
        stubber.add_response("put_object", {}, None)
        with (
            stubber,
            mock.patch.object(iv, "cloudwatch"),
            mock.patch.object(iv, "iam") as iam,
            mock.patch.object(iv, "s3", client),
            mock.patch.object(iv, "ESTIMATE_BUCKET", "estimates-bucket"),
            mock.patch.object(iv, "ESTIMATE_KMS_KEY_ID", "arn:aws:kms:us-east-1:123456789012:key/abc"),
        ):
            _stub_iam(iam, _tagged(PlatformId=COST_ID))
            iv.handler(_envelope([_record(TENANT_ROLE)]), None)
        stubber.assert_no_pending_responses()


if __name__ == "__main__":
    unittest.main()
