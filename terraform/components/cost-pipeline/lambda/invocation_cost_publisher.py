"""
invocation_cost_publisher
─────────────────────────

CloudWatch Logs subscription handler. Bedrock invocation logs land in the
log group as JSON records (one per invocation). We:

  1. gunzip + base64-decode the CloudWatch payload,
  2. parse each invocation record,
  3. compute estimated USD cost from input/output token counts plus the
     per-model pricing table below,
  4. emit a PutMetricData call to CloudWatch under
     ``namespace=agents/Bedrock`` / ``MetricName=EstimatedInvocationCostUsd``,
     dimensioned by PlatformId (extracted from the invocation's tags or
     headers, falling back to "unknown").

The Budget reconciler reads this metric to estimate in-flight cost
incurred since the most recent CUR partition (which lags by ~24h).

Pricing table is conservative and rounded; the metric is intended for
threshold/alerting decisions, not finance-grade reporting. The CUR-based
view is authoritative for billing.
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

logger = logging.getLogger()
logger.setLevel(logging.INFO)

cloudwatch = boto3.client("cloudwatch")
s3 = boto3.client("s3")
NAMESPACE = "agents/Bedrock"
METRIC_NAME = "EstimatedInvocationCostUsd"
TOKENS_IN_METRIC = "TokensIn"
TOKENS_OUT_METRIC = "TokensOut"

# Estimate export → S3 (Hive-partitioned NDJSON under usage_date=<d>/) feeds
# the Athena `invocation_cost_estimates` table, which the
# `invocation_cost_reconciliation` view SUMs against CUR truth. An empty
# ESTIMATE_BUCKET disables the export (the CloudWatch metric path is
# unaffected) so the handler degrades cleanly if the env wiring is absent.
ESTIMATE_BUCKET = os.environ.get("ESTIMATE_BUCKET", "")
ESTIMATE_PREFIX = os.environ.get("ESTIMATE_PREFIX", "estimates")
ESTIMATE_KMS_KEY_ID = os.environ.get("ESTIMATE_KMS_KEY_ID", "")


# USD per 1K tokens. Rough; rounded conservatively upward so the
# metric trips alerts slightly before the CUR confirms.
# Bedrock-specific identifiers (cross-region inference profile prefixes
# like 'us.' get stripped before lookup).
PRICING: dict[str, dict[str, float]] = {
    # Anthropic
    "anthropic.claude-3-5-sonnet-20241022-v2:0": {"input": 0.003,  "output": 0.015},
    "anthropic.claude-3-5-haiku-20241022-v1:0":  {"input": 0.001,  "output": 0.005},
    "anthropic.claude-3-opus-20240229-v1:0":     {"input": 0.015,  "output": 0.075},
    "anthropic.claude-3-haiku-20240307-v1:0":    {"input": 0.00025, "output": 0.00125},
    # Amazon Nova
    "amazon.nova-pro-v1:0":   {"input": 0.0008,  "output": 0.0032},
    "amazon.nova-lite-v1:0":  {"input": 0.00006, "output": 0.00024},
    "amazon.nova-micro-v1:0": {"input": 0.000035, "output": 0.00014},
    # Meta Llama
    "meta.llama3-1-70b-instruct-v1:0": {"input": 0.00072, "output": 0.00072},
    "meta.llama3-1-8b-instruct-v1:0":  {"input": 0.00022, "output": 0.00022},
}

# Fallback when modelId isn't in the table. Tuned to be roughly mid-range —
# alerts on unknown models will be slightly off but not silent.
FALLBACK_PRICING = {"input": 0.003, "output": 0.015}


def _bare_model(model_id: str) -> str:
    # Strip cross-region prefixes like 'us.' / 'eu.' that AWS prepends to
    # inference profile IDs, leaving the bare Bedrock model id.
    return model_id.split(".", 1)[1] if "." in model_id and len(model_id.split(".", 1)[0]) <= 3 else model_id


def _model_pricing(model_id: str) -> dict[str, float]:
    return PRICING.get(_bare_model(model_id), FALLBACK_PRICING)


def _estimate_cost(record: dict[str, Any]) -> tuple[float, str, str, int, int]:
    """
    Returns (cost_usd, platform_id, model_id, in_tokens, out_tokens) for one
    invocation log record.

    Bedrock invocation log shape (abridged):
      {
        "modelId": "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
        "input": {"inputTokenCount": 142, ...},
        "output": {"outputTokenCount": 87, ...},
        "requestId": "...",
        "identity": {"arn": "arn:aws:sts:::assumed-role/<env>-<platform>-tenant/..."}
      }

    The PlatformId is recovered from the assumed-role ARN's session name
    — the tenant role naming contract (see ADR 0004) embeds the platform
    name there. The role-name itself is the source of truth.
    """
    model = record.get("modelId") or record.get("model") or "unknown"
    bare = _bare_model(model)
    in_tokens = (record.get("input") or {}).get("inputTokenCount", 0) or 0
    out_tokens = (record.get("output") or {}).get("outputTokenCount", 0) or 0
    pricing = PRICING.get(bare, FALLBACK_PRICING)
    cost = (in_tokens / 1000.0) * pricing["input"] + (out_tokens / 1000.0) * pricing["output"]

    platform_id = "unknown"
    identity_arn = (record.get("identity") or {}).get("arn", "")
    # Tenant role pattern: ${env}-${platform}-tenant
    # ARN looks like arn:aws:sts::123:assumed-role/dev-acme-tenant/<session>
    if ":assumed-role/" in identity_arn:
        role_part = identity_arn.split(":assumed-role/", 1)[1].split("/", 1)[0]
        env_prefix = os.environ.get("AGENTS_ENVIRONMENT", "")
        suffix = "-tenant"
        if role_part.endswith(suffix):
            stripped = role_part[: -len(suffix)]
            if env_prefix and stripped.startswith(env_prefix + "-"):
                platform_id = stripped[len(env_prefix) + 1 :]
            else:
                platform_id = stripped
    return cost, platform_id, bare, int(in_tokens), int(out_tokens)


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
    parsed = 0
    skipped = 0
    for log_event in payload.get("logEvents", []):
        try:
            record = json.loads(log_event["message"])
        except (KeyError, json.JSONDecodeError):
            skipped += 1
            continue
        cost, platform_id, model_id, in_tokens, out_tokens = _estimate_cost(record)
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

    logger.info(
        "processed log batch parsed=%d skipped=%d platforms=%d",
        parsed,
        skipped,
        len(per_platform),
    )
    return {"status": "ok", "parsed": parsed, "skipped": skipped, "platforms": len(per_platform)}
