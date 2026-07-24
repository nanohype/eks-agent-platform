/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// This is the org's only hand-written SigV4 signer — every other AMP caller
// signs through collector or Grafana config — so the request shape and the
// decode rules carry their own tests rather than riding on SDK coverage.

func testConfig() aws.Config {
	return aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secret", ""),
	}
}

func TestNewPrometheusQueryURLConstruction(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			// What managed-monitoring actually publishes: the workspace URL with
			// a trailing slash (the remote-write URL is built by bare
			// concatenation onto it).
			name:     "trailing slash",
			endpoint: "https://aps-workspaces.us-west-2.amazonaws.com/workspaces/ws-abc/",
			want:     "https://aps-workspaces.us-west-2.amazonaws.com/workspaces/ws-abc/api/v1/query",
		},
		{
			name:     "no trailing slash",
			endpoint: "https://aps-workspaces.us-west-2.amazonaws.com/workspaces/ws-abc",
			want:     "https://aps-workspaces.us-west-2.amazonaws.com/workspaces/ws-abc/api/v1/query",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := NewPrometheusQuery(testConfig(), c.endpoint)
			if err != nil {
				t.Fatalf("NewPrometheusQuery: %v", err)
			}
			// A doubled slash would be signed literally by SigV4 and come back
			// as a signature mismatch that reads like an auth failure.
			if got := q.(*ampQueryClient).queryURL; got != c.want {
				t.Errorf("queryURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNewPrometheusQueryRejectsBadEndpoints(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"blank":      "   ",
		"plain http": "http://aps-workspaces.us-west-2.amazonaws.com/workspaces/ws-abc/",
		"not a url":  "://nope",
	}
	for name, endpoint := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPrometheusQuery(testConfig(), endpoint); err == nil {
				t.Errorf("endpoint %q must be rejected at construction, not at the first query", endpoint)
			}
		})
	}
	cfg := testConfig()
	cfg.Credentials = nil
	if _, err := NewPrometheusQuery(cfg, "https://aps-workspaces.us-west-2.amazonaws.com/workspaces/ws-abc/"); err == nil {
		t.Error("a config with no credential provider must be rejected")
	}
}

func TestQueryScalarSignsAndPostsTheQuery(t *testing.T) {
	var gotAuth, gotBody, gotContentType, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1690000000,"0.0142"]}]}}`))
	}))
	defer srv.Close()

	c := &ampQueryClient{
		httpClient: srv.Client(),
		signer:     newTestSigner(),
		creds:      testConfig().Credentials,
		region:     "us-west-2",
		queryURL:   srv.URL + "/api/v1/query",
	}
	v, ok, err := c.QueryScalar(context.Background(), `sum(rate(x_errors_total[1h]))`)
	if err != nil {
		t.Fatalf("QueryScalar: %v", err)
	}
	if !ok || v != 0.0142 {
		t.Errorf("value = %v (ok=%v), want 0.0142", v, ok)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST — a long burn-rate expression can exceed a query-string ceiling", gotMethod)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q; it must be set before signing or the signature covers different headers than the request carries", gotContentType)
	}
	if !strings.Contains(gotBody, "query=sum") {
		t.Errorf("body = %q, want the form-encoded query", gotBody)
	}
	// aps, not "prometheus" and not "amp" — a wrong service name yields an
	// opaque 403.
	if !strings.Contains(gotAuth, "/us-west-2/aps/aws4_request") {
		t.Errorf("Authorization credential scope = %q, want the aps service in us-west-2", gotAuth)
	}
}

func TestDecodeInstantScalar(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    float64
		wantOK  bool
		wantErr string
	}{
		{
			name: "a sample", status: 200,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"0.5"]}]}}`,
			want:   0.5,
			wantOK: true,
		},
		{
			// Valid query, nothing matched. Not an error, and emphatically not a
			// healthy zero — the control loop must be able to tell them apart
			// before it acts.
			name: "empty result is no-data", status: 200,
			body:   `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantOK: false,
		},
		{
			// A ratio with no denominator. Letting NaN through would make every
			// burn comparison false and the breach would silently never fire.
			name: "NaN is no-data", status: 200,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"NaN"]}]}}`,
			wantOK: false,
		},
		{
			name: "Inf is no-data", status: 200,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"+Inf"]}]}}`,
			wantOK: false,
		},
		{
			// The API reports query errors as a 400 with a useful errorType; that
			// message is far more actionable than the bare status.
			name: "query error carries the errorType", status: 400,
			body:    `{"status":"error","errorType":"bad_data","error":"parse error at char 5"}`,
			wantErr: "bad_data",
		},
		{
			name: "non-json 500", status: 500,
			body:    `<html>gateway timeout</html>`,
			wantErr: "http 500",
		},
		{
			name: "unexpected result type", status: 200,
			body:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantErr: "resultType",
		},
		{
			name: "malformed sample", status: 200,
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"value":[1]}]}}`,
			wantErr: "want [timestamp, value]",
		},
		{
			name: "non-string sample value", status: 200,
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,0.5]}]}}`,
			wantErr: "want a string",
		},
		{
			name: "unparseable sample", status: 200,
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"not-a-number"]}]}}`,
			wantErr: "parse amp sample",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok, err := decodeInstantScalar(c.status, []byte(c.body))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != c.wantOK || (c.wantOK && v != c.want) {
				t.Errorf("got (%v, %v), want (%v, %v)", v, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate must leave a short string alone, got %q", got)
	}
	if got := truncate("0123456789", 4); got != "0123…" {
		t.Errorf("truncate = %q, want the bounded excerpt", got)
	}
}

// newTestSigner builds the same signer the constructor uses, so the request
// under test is signed exactly as production signs it.
func newTestSigner() *v4.Signer { return v4.NewSigner() }
