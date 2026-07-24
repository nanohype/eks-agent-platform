"""
Unit tests for the invocation cost publisher.

Covers the two paths the pricing correctness rests on: cross-region
inference-profile prefix stripping (so profile IDs resolve to the base model
price) and the unpriced path (an unknown model is surfaced as an
UnpricedInvocations count, never priced against a borrowed rate).

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
# The env prefix the handler strips off the tenant role name to recover the
# platform id (role shape: ${env}-${platform}-tenant).
os.environ.setdefault("AGENTS_ENVIRONMENT", "dev")

import invocation_cost_publisher as iv  # noqa: E402


def _envelope(records: list[dict]) -> dict:
    """Build a CloudWatch Logs subscription event from invocation records."""
    payload = {
        "messageType": "DATA_MESSAGE",
        "logEvents": [{"id": str(i), "message": json.dumps(r)} for i, r in enumerate(records)],
    }
    packed = gzip.compress(json.dumps(payload).encode("utf-8"))
    return {"awslogs": {"data": base64.b64encode(packed).decode("utf-8")}}


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


class EstimateCostTest(unittest.TestCase):
    def test_prices_a_current_model_via_inference_profile(self):
        record = {
            "modelId": "us.anthropic.claude-sonnet-4-6",
            "input": {"inputTokenCount": 1_000_000},
            "output": {"outputTokenCount": 1_000_000},
            "identity": {"arn": "arn:aws:sts::1:assumed-role/dev-acme-tenant/sess"},
        }
        cost, platform, model, _in, _out, priced = iv._estimate_cost(record)
        self.assertTrue(priced)
        # 3.0 + 15.0 per million in+out
        self.assertAlmostEqual(cost, 18.0, places=4)
        self.assertEqual(model, "anthropic.claude-sonnet-4-6")

    def test_unknown_model_is_unpriced_not_borrowed(self):
        record = {
            "modelId": "us.anthropic.claude-imaginary-9-9",
            "input": {"inputTokenCount": 5_000_000},
            "output": {"outputTokenCount": 5_000_000},
            "identity": {"arn": "arn:aws:sts::1:assumed-role/dev-acme-tenant/sess"},
        }
        cost, _platform, _model, _in, _out, priced = iv._estimate_cost(record)
        self.assertFalse(priced)
        self.assertEqual(cost, 0.0)


class HandlerTest(unittest.TestCase):
    def _run(self, records: list[dict]):
        with mock.patch.object(iv, "cloudwatch") as cw, mock.patch.object(iv, "s3"):
            result = iv.handler(_envelope(records), None)
        # Flatten every metric emitted across all put_metric_data calls.
        metrics = [
            m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]
        ]
        return result, metrics

    def test_unpriced_model_emits_unpriced_metric_only(self):
        result, metrics = self._run(
            [
                {
                    "modelId": "us.anthropic.claude-imaginary-9-9",
                    "input": {"inputTokenCount": 100},
                    "output": {"outputTokenCount": 50},
                    "identity": {"arn": "arn:aws:sts::1:assumed-role/dev-acme-tenant/sess"},
                }
            ]
        )
        self.assertEqual(result["unpriced"], 1)
        self.assertEqual(result["platforms"], 0)
        names = {m["MetricName"] for m in metrics}
        self.assertIn(iv.UNPRICED_METRIC, names)
        self.assertNotIn(iv.METRIC_NAME, names)
        unpriced = next(m for m in metrics if m["MetricName"] == iv.UNPRICED_METRIC)
        dims = {d["Name"]: d["Value"] for d in unpriced["Dimensions"]}
        self.assertEqual(dims["PlatformId"], "acme")
        self.assertEqual(dims["ModelId"], "anthropic.claude-imaginary-9-9")

    def test_priced_model_emits_cost_metric(self):
        result, metrics = self._run(
            [
                {
                    "modelId": "us.anthropic.claude-sonnet-4-6",
                    "input": {"inputTokenCount": 1_000_000},
                    "output": {"outputTokenCount": 0},
                    "identity": {"arn": "arn:aws:sts::1:assumed-role/dev-acme-tenant/sess"},
                }
            ]
        )
        self.assertEqual(result["platforms"], 1)
        self.assertEqual(result["unpriced"], 0)
        cost_metric = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertAlmostEqual(cost_metric["Value"], 3.0, places=4)


class ImportedModelTest(unittest.TestCase):
    """Custom Model Import (open-weight) models: unpriced-but-observable by
    default, priced at the configured per-token governance estimate when set."""

    ARN = "arn:aws:bedrock:us-west-2:123456789012:imported-model/abc123"

    def _record(self, in_tok: int = 1_000_000, out_tok: int = 1_000_000) -> dict:
        return {
            "modelId": self.ARN,
            "input": {"inputTokenCount": in_tok},
            "output": {"outputTokenCount": out_tok},
            "identity": {"arn": "arn:aws:sts::1:assumed-role/dev-acme-tenant/sess"},
        }

    def test_detection_and_key(self):
        self.assertTrue(iv._is_imported(self.ARN))
        self.assertFalse(iv._is_imported("us.anthropic.claude-sonnet-4-6"))
        self.assertEqual(iv._imported_key(self.ARN), "imported/abc123")

    def test_unpriced_without_estimate(self):
        # Default estimate 0 → imported invocation is unpriced (surfaced as an
        # UnpricedInvocations count), never a borrowed rate. Model key is the
        # compact imported id, not the raw ARN.
        with mock.patch.object(iv, "IMPORTED_ESTIMATE_USD_PER_M", 0.0):
            cost, platform, model, _in, _out, priced = iv._estimate_cost(self._record())
        self.assertFalse(priced)
        self.assertEqual(cost, 0.0)
        self.assertEqual(model, "imported/abc123")
        self.assertEqual(platform, "acme")

    def test_priced_with_estimate(self):
        # A configured per-token estimate prices input+output tokens so imported
        # spend reaches the kill-switch cost signal.
        with mock.patch.object(iv, "IMPORTED_ESTIMATE_USD_PER_M", 4.0):
            cost, _platform, model, _in, _out, priced = iv._estimate_cost(self._record(1_000_000, 1_000_000))
        self.assertTrue(priced)
        self.assertAlmostEqual(cost, 8.0, places=4)  # (1M + 1M)/1M * 4.0
        self.assertEqual(model, "imported/abc123")

    def test_handler_prices_imported_when_estimate_set(self):
        with (
            mock.patch.object(iv, "IMPORTED_ESTIMATE_USD_PER_M", 4.0),
            mock.patch.object(iv, "cloudwatch") as cw,
            mock.patch.object(iv, "s3"),
        ):
            result = iv.handler(_envelope([self._record(1_000_000, 0)]), None)
        metrics = [m for call in cw.put_metric_data.call_args_list for m in call.kwargs["MetricData"]]
        self.assertEqual(result["platforms"], 1)
        self.assertEqual(result["unpriced"], 0)
        cost_metric = next(m for m in metrics if m["MetricName"] == iv.METRIC_NAME)
        self.assertAlmostEqual(cost_metric["Value"], 4.0, places=4)  # 1M/1M * 4.0
        tok = next(m for m in metrics if m["MetricName"] == iv.TOKENS_IN_METRIC)
        dims = {d["Name"]: d["Value"] for d in tok["Dimensions"]}
        self.assertEqual(dims["ModelId"], "imported/abc123")


if __name__ == "__main__":
    unittest.main()
