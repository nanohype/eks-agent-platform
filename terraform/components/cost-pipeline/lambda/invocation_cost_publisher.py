"""
invocation_cost_publisher
─────────────────────────

CloudWatch Logs subscription handler. Bedrock invocation logs land in the
log group as JSON records (one per invocation). We:

  1. gunzip + base64-decode the CloudWatch payload,
  2. parse each invocation record,
  3. compute estimated USD cost from input/output token counts plus the
     per-model pricing table (``pricing_data.PRICING``),
  4. emit a PutMetricData call to CloudWatch under
     ``namespace=agents/Bedrock`` / ``MetricName=EstimatedInvocationCostUsd``,
     dimensioned by PlatformId (extracted from the invocation's tags or
     headers, falling back to "unknown").

An invocation on a model that isn't in the pricing table is **not** priced
against a borrowed rate — that would silently undercount spend on a new or
mistyped model id. Instead it is published as an explicit ``UnpricedInvocations``
count dimensioned by PlatformId + ModelId, so unpriced traffic is observable
and the pricing table can be extended before the next billing cycle.

The pricing table is generated from the same JSON source of truth the
TypeScript ``@eks-agent/pricing`` package imports
(``packages/pricing/src/data/bedrock-pricing.json``); a CI drift gate keeps the
two in lockstep. Prices are USD per 1,000,000 tokens.

The Budget reconciler reads the cost metric to estimate in-flight cost
incurred since the most recent CUR partition (which lags by ~24h). The metric
is intended for threshold/alerting decisions, not finance-grade reporting; the
CUR-based view is authoritative for billing.
"""

from __future__ import annotations

import base64
import gzip
import json
import logging
import os
import uuid
from collections import defaultdict
from datetime import datetime, timezone
from typing import Any

import boto3

from pricing_data import PRICING

logger = logging.getLogger()
logger.setLevel(logging.INFO)

cloudwatch = boto3.client("cloudwatch")
s3 = boto3.client("s3")
NAMESPACE = "agents/Bedrock"
METRIC_NAME = "EstimatedInvocationCostUsd"
TOKENS_IN_METRIC = "TokensIn"
TOKENS_OUT_METRIC = "TokensOut"
UNPRICED_METRIC = "UnpricedInvocations"

# Estimate export → S3 (Hive-partitioned NDJSON under usage_date=<d>/) feeds
# the Athena `invocation_cost_estimates` table, which the
# `invocation_cost_reconciliation` view SUMs against CUR truth. An empty
# ESTIMATE_BUCKET disables the export (the CloudWatch metric path is
# unaffected) so the handler degrades cleanly if the env wiring is absent.
ESTIMATE_BUCKET = os.environ.get("ESTIMATE_BUCKET", "")
ESTIMATE_PREFIX = os.environ.get("ESTIMATE_PREFIX", "estimates")
ESTIMATE_KMS_KEY_ID = os.environ.get("ESTIMATE_KMS_KEY_ID", "")


def _bare_model(model_id: str) -> str:
    """
    Strip a Bedrock cross-region inference-profile prefix (``us.``, ``eu.``,
    ``jp.``, ``ap.``, ``apac.``, ``global.`` …) from a model id, leaving the
    bare ``<provider>.<model>`` id used as the pricing-table key.

    Removes exactly one leading lowercase-alpha segment, and only when a
    ``<provider>.<model>`` id remains (the remainder still contains a dot), so
    a bare provider id like ``anthropic.claude-3-opus-20240229-v1:0`` is left
    untouched. Pattern-based — no hardcoded prefix list — so future regional
    shorts of any length resolve automatically.
    """
    head, sep, tail = model_id.partition(".")
    if sep and head.isalpha() and head.islower() and "." in tail:
        return tail
    return model_id


def _price_for(model_id: str) -> dict[str, float] | None:
    """Return the price row for a model id, or None when it isn't priced."""
    return PRICING.get(_bare_model(model_id))


def _extract_platform(identity_arn: str) -> str:
    """Recover the PlatformId from an assumed-role ARN's role name.

    The tenant role naming contract (see ADR 0004) embeds the platform name in
    the ``${env}-${platform}-tenant`` role, so the role name is the source of
    truth. Returns "unknown" when the ARN doesn't match the tenant shape.
    """
    if ":assumed-role/" not in identity_arn:
        return "unknown"
    role_part = identity_arn.split(":assumed-role/", 1)[1].split("/", 1)[0]
    suffix = "-tenant"
    if not role_part.endswith(suffix):
        return "unknown"
    stripped = role_part[: -len(suffix)]
    env_prefix = os.environ.get("AGENTS_ENVIRONMENT", "")
    if env_prefix and stripped.startswith(env_prefix + "-"):
        return stripped[len(env_prefix) + 1 :]
    return stripped


def _estimate_cost(record: dict[str, Any]) -> tuple[float, str, str, int, int, bool]:
    """
    Returns (cost_usd, platform_id, model_id, in_tokens, out_tokens, priced)
    for one invocation log record.

    Bedrock invocation log shape (abridged):
      {
        "modelId": "us.anthropic.claude-sonnet-4-6",
        "input": {"inputTokenCount": 142, ...},
        "output": {"outputTokenCount": 87, ...},
        "requestId": "...",
        "identity": {"arn": "arn:aws:sts:::assumed-role/<env>-<platform>-tenant/..."}
      }

    ``priced`` is False when the model id has no pricing-table entry; the cost
    is then an unmetered 0 rather than a borrowed rate, and the caller surfaces
    it as an UnpricedInvocations count.
    """
    model = record.get("modelId") or record.get("model") or "unknown"
    bare = _bare_model(model)
    in_tokens = (record.get("input") or {}).get("inputTokenCount", 0) or 0
    out_tokens = (record.get("output") or {}).get("outputTokenCount", 0) or 0

    price = PRICING.get(bare)
    priced = price is not None
    cost = 0.0
    if price is not None:
        cost = (in_tokens / 1_000_000.0) * price["input"] + (
            out_tokens / 1_000_000.0
        ) * price["output"]

    platform_id = _extract_platform((record.get("identity") or {}).get("arn", ""))
    return cost, platform_id, bare, int(in_tokens), int(out_tokens), priced


def _decode_payload(awslogs_data: str) -> dict[str, Any]:
    """
    Decode the CloudWatch Logs subscription envelope.

    Returns an empty 'CONTROL_MESSAGE'-shaped dict when the payload is
    corrupt rather than raising — a single bad envelope from CloudWatch
    (which AWS can produce during service events) should not poison the
    whole subscription with infinite retries until the retention window
    expires. The handler treats the empty messageType as 'skipped'.
    """
    try:
        raw = base64.b64decode(awslogs_data)
        unz = gzip.decompress(raw)
        return json.loads(unz)
    except (ValueError, OSError, json.JSONDecodeError) as exc:
        logger.warning("decode failed; treating as control message: %s", exc)
        return {"messageType": "CONTROL_MESSAGE", "logEvents": []}


def _emit_metrics(per_platform: dict[str, float]) -> None:
    # PutMetricData caps at 20 metric data values per call.
    items = list(per_platform.items())
    while items:
        chunk = items[:20]
        items = items[20:]
        cloudwatch.put_metric_data(
            Namespace=NAMESPACE,
            MetricData=[
                {
                    "MetricName": METRIC_NAME,
                    "Dimensions": [{"Name": "PlatformId", "Value": pid}],
                    "Unit": "None",
                    "Value": round(cost, 6),
                }
                for pid, cost in chunk
            ],
        )


def _emit_token_metrics(aggregates: dict[tuple[str, str], dict[str, float]]) -> None:
    # TokensIn/TokensOut per (PlatformId, ModelId), same agents/Bedrock
    # namespace. Distinct dimensions from EstimatedInvocationCostUsd (which the
    # Budget reconciler reads by PlatformId only), so this is purely additive.
    data: list[dict[str, Any]] = []
    for (platform_id, model_id), agg in aggregates.items():
        dims = [
            {"Name": "PlatformId", "Value": platform_id},
            {"Name": "ModelId", "Value": model_id},
        ]
        data.append({"MetricName": TOKENS_IN_METRIC, "Dimensions": dims, "Unit": "Count", "Value": float(agg["in"])})
        data.append({"MetricName": TOKENS_OUT_METRIC, "Dimensions": dims, "Unit": "Count", "Value": float(agg["out"])})
    while data:
        chunk = data[:20]
        data = data[20:]
        cloudwatch.put_metric_data(Namespace=NAMESPACE, MetricData=chunk)


def _emit_unpriced(counts: dict[tuple[str, str], int]) -> None:
    # One UnpricedInvocations count per (PlatformId, ModelId) for models with no
    # pricing entry. Surfaces unpriced traffic as an observable miss instead of
    # letting a borrowed price silently undercount spend. Alarm on this metric
    # to catch a new or mistyped model id before it accrues unmetered cost.
    data: list[dict[str, Any]] = [
        {
            "MetricName": UNPRICED_METRIC,
            "Dimensions": [
                {"Name": "PlatformId", "Value": platform_id},
                {"Name": "ModelId", "Value": model_id},
            ],
            "Unit": "Count",
            "Value": float(count),
        }
        for (platform_id, model_id), count in counts.items()
    ]
    while data:
        chunk = data[:20]
        data = data[20:]
        cloudwatch.put_metric_data(Namespace=NAMESPACE, MetricData=chunk)


def _write_estimates(aggregates: dict[tuple[str, str], dict[str, float]], usage_date: str) -> None:
    """
    Write one NDJSON object per log batch to the Hive-partitioned estimate
    prefix (s3://$ESTIMATE_BUCKET/$ESTIMATE_PREFIX/usage_date=<d>/<uuid>.json),
    one record per (platform_id, model_id). The Athena table partitions on
    usage_date only (date projection) and treats platform_id as a data column,
    so the reconciliation view can SUM across all platforms without a
    per-partition predicate.

    A failed put is logged and swallowed — an S3 hiccup must never poison the
    CloudWatch metric path or trigger log-subscription retries.
    """
    if not ESTIMATE_BUCKET or not aggregates:
        return
    records = [
        {
            "platform_id": platform_id,
            "model_id": model_id,
            "estimate_usd": round(agg["cost"], 6),
            "input_tokens": int(agg["in"]),
            "output_tokens": int(agg["out"]),
            "invocation_count": int(agg["count"]),
        }
        for (platform_id, model_id), agg in aggregates.items()
    ]
    body = "\n".join(json.dumps(r) for r in records).encode("utf-8")
    key = f"{ESTIMATE_PREFIX}/usage_date={usage_date}/{uuid.uuid4().hex}.json"
    put_kwargs: dict[str, Any] = {
        "Bucket": ESTIMATE_BUCKET,
        "Key": key,
        "Body": body,
        "ContentType": "application/x-ndjson",
    }
    if ESTIMATE_KMS_KEY_ID:
        put_kwargs["ServerSideEncryption"] = "aws:kms"
        put_kwargs["SSEKMSKeyId"] = ESTIMATE_KMS_KEY_ID
    try:
        s3.put_object(**put_kwargs)
    except Exception as exc:  # telemetry export must never block the metric path
        logger.warning("estimate export failed: %s", exc)


def handler(event: dict[str, Any], _context: Any) -> dict[str, Any]:
    payload = _decode_payload(event["awslogs"]["data"])
    if payload.get("messageType") != "DATA_MESSAGE":
        return {"status": "skipped", "reason": payload.get("messageType")}

    per_platform: dict[str, float] = defaultdict(float)
    aggregates: dict[tuple[str, str], dict[str, float]] = {}
    unpriced: dict[tuple[str, str], int] = defaultdict(int)
    parsed = 0
    skipped = 0
    for log_event in payload.get("logEvents", []):
        try:
            record = json.loads(log_event["message"])
        except (KeyError, json.JSONDecodeError):
            skipped += 1
            continue
        cost, platform_id, model_id, in_tokens, out_tokens, priced = _estimate_cost(record)
        if not priced:
            unpriced[(platform_id, model_id)] += 1
            parsed += 1
            continue
        if cost <= 0:
            skipped += 1
            continue
        per_platform[platform_id] += cost
        agg = aggregates.setdefault((platform_id, model_id), {"cost": 0.0, "in": 0.0, "out": 0.0, "count": 0.0})
        agg["cost"] += cost
        agg["in"] += in_tokens
        agg["out"] += out_tokens
        agg["count"] += 1
        parsed += 1

    if per_platform:
        _emit_metrics(dict(per_platform))
        _emit_token_metrics(aggregates)
        _write_estimates(aggregates, datetime.now(timezone.utc).strftime("%Y-%m-%d"))
    if unpriced:
        _emit_unpriced(dict(unpriced))

    logger.info(
        "processed log batch parsed=%d skipped=%d unpriced=%d platforms=%d",
        parsed,
        skipped,
        sum(unpriced.values()),
        len(per_platform),
    )
    return {
        "status": "ok",
        "parsed": parsed,
        "skipped": skipped,
        "unpriced": sum(unpriced.values()),
        "platforms": len(per_platform),
    }
