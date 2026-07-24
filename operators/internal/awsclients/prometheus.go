/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// PrometheusQuery reads instant values out of Amazon Managed Prometheus.
//
// Unlike every other interface in this package it does not mirror an AWS SDK
// client, because AMP's read path has none: the aws-sdk-go-v2 amp package covers
// only workspace management, while queries go to the workspace's
// Prometheus-compatible HTTP API. So this is a signed HTTP client — the request
// is SigV4-signed for service "aps" with the operator's own credentials, exactly
// as the SDK would sign a control-plane call.
type PrometheusQuery interface {
	// QueryScalar evaluates expr as an instant query and returns its single
	// sample. The second return is false when the query succeeded but matched no
	// series (or produced NaN/Inf): a metric that does not exist yet is not the
	// same reading as a healthy zero, and callers must be able to tell them
	// apart before acting on the number.
	QueryScalar(ctx context.Context, expr string) (float64, bool, error)
}

// ampSigningService is AMP's SigV4 service name. It is "aps" (the API's own
// name), not "prometheus" or "amp" — a wrong service name yields a signature
// mismatch that surfaces as an opaque 403.
const ampSigningService = "aps"

// promQueryTimeout bounds one instant query. The burn-rate reconciler issues one
// per window per tick, so a stalled AMP must not pin a reconcile worker; its
// caller additionally bounds the whole tick.
const promQueryTimeout = 15 * time.Second

// ampQueryClient is the signed-HTTP PrometheusQuery implementation.
type ampQueryClient struct {
	httpClient *http.Client
	signer     *v4.Signer
	creds      aws.CredentialsProvider
	region     string
	// queryURL is the fully-resolved instant-query endpoint,
	// <workspace-endpoint>/api/v1/query.
	queryURL string
}

// NewPrometheusQuery builds an AMP query client against a workspace endpoint.
//
// endpoint is the value managed-monitoring publishes to SSM as
// .../managed-monitoring/amp_endpoint — the workspace base URL, which the AMP
// API emits with a trailing slash (the remote-write URL is built by
// concatenating "api/v1/remote_write" onto it). Both forms are accepted here so
// a future un-slashed value cannot produce a double or missing separator.
//
// It is constructed after startup rather than inside New because the endpoint
// arrives from SSM, and reading SSM needs the client New returns.
func NewPrometheusQuery(cfg aws.Config, endpoint string) (PrometheusQuery, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, fmt.Errorf("amp query endpoint is empty")
	}
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("parse amp endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("amp endpoint %q must be https", endpoint)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/query"
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("aws config carries no credential provider")
	}
	return &ampQueryClient{
		httpClient: &http.Client{Timeout: promQueryTimeout},
		signer:     v4.NewSigner(),
		creds:      cfg.Credentials,
		region:     cfg.Region,
		queryURL:   u.String(),
	}, nil
}

// promInstantResponse is the subset of the Prometheus HTTP API's response the
// reconciler reads. On error the API still returns JSON, with status "error" and
// a machine-readable errorType.
type promInstantResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			// value is [<unix seconds>, "<sample as string>"] — Prometheus
			// renders sample values as strings so float precision survives the
			// JSON round-trip.
			Value []any `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (c *ampQueryClient) QueryScalar(ctx context.Context, expr string) (float64, bool, error) {
	// POST rather than GET: burn-rate expressions run long, and a query string
	// has a length ceiling that a histogram-bucket ratio can reach.
	body := url.Values{"query": {expr}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.queryURL, strings.NewReader(body))
	if err != nil {
		return 0, false, fmt.Errorf("build amp query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("retrieve credentials for amp query: %w", err)
	}
	// SigV4 covers the body, so the hash is of the exact bytes sent.
	sum := sha256.Sum256([]byte(body))
	if err := c.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), ampSigningService, c.region, time.Now().UTC()); err != nil {
		return 0, false, fmt.Errorf("sign amp query: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("amp query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the read: a well-formed instant query answers in bytes, and an
	// unbounded read of a misrouted endpoint's response would be a memory
	// hazard on a reconcile worker.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false, fmt.Errorf("read amp query response: %w", err)
	}
	return decodeInstantScalar(resp.StatusCode, raw)
}

// decodeInstantScalar turns an AMP instant-query response into a single sample.
// Split out from the transport so the decode rules — which carry the no-data vs.
// healthy-zero distinction the control loop depends on — are unit-testable
// without signing or a server.
func decodeInstantScalar(statusCode int, raw []byte) (float64, bool, error) {
	var parsed promInstantResponse
	// Decode before checking the status code: the API reports query errors as a
	// 400 with a useful errorType, and that message is far more actionable than
	// the bare status.
	decodeErr := json.Unmarshal(raw, &parsed)
	if decodeErr == nil && parsed.Status == "error" {
		return 0, false, fmt.Errorf("amp query error (%s): %s", parsed.ErrorType, parsed.Error)
	}
	if statusCode < 200 || statusCode > 299 {
		return 0, false, fmt.Errorf("amp query http %d: %s", statusCode, truncate(string(raw), 300))
	}
	if decodeErr != nil {
		return 0, false, fmt.Errorf("decode amp query response: %w", decodeErr)
	}
	if parsed.Status != "success" {
		return 0, false, fmt.Errorf("amp query returned status %q", parsed.Status)
	}
	if parsed.Data.ResultType != "vector" {
		return 0, false, fmt.Errorf("amp query returned resultType %q; the burn-rate expressions always aggregate to a vector", parsed.Data.ResultType)
	}
	if len(parsed.Data.Result) == 0 {
		// Query is valid, nothing matched. Not an error and not a zero.
		return 0, false, nil
	}
	v := parsed.Data.Result[0].Value
	if len(v) < 2 {
		return 0, false, fmt.Errorf("amp query sample has %d elements, want [timestamp, value]", len(v))
	}
	s, ok := v[1].(string)
	if !ok {
		return 0, false, fmt.Errorf("amp query sample value is %T, want a string", v[1])
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse amp sample %q: %w", s, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// A ratio with no denominator. Report it as absent rather than let a
		// NaN propagate into a burn-rate comparison, where every comparison
		// against it is false and the breach would silently never fire.
		return 0, false, nil
	}
	return f, true, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
