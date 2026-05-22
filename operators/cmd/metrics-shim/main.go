/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Command metrics-shim bridges KEDA's metrics-api scaler to the Managed
// Agents work-queue stats endpoint. KEDA's metrics-api scaler sends at
// most one auth header; the Anthropic work/stats endpoint needs three
// (x-api-key, anthropic-version, anthropic-beta). The shim holds the org
// API key, calls the upstream with all three, and re-serves the queue
// depth as plain in-cluster JSON the scaler can read.
//
// It ships as a second binary in the operator image; the SandboxPool
// reconciler runs one shim Deployment per autoscaled pool.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

const (
	listenAddr        = ":8080"
	anthropicAPIBase  = "https://api.anthropic.com"
	anthropicVersion  = "2023-06-01"
	anthropicBeta     = "managed-agents-2026-04-01"
	upstreamTimeout   = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	maxErrBodyBytes   = 512
)

func main() {
	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if envID == "" || apiKey == "" {
		log.Fatal("metrics-shim: ANTHROPIC_ENVIRONMENT_ID and ANTHROPIC_API_KEY must both be set")
	}

	statsURL := fmt.Sprintf("%s/v1/environments/%s/work/stats", anthropicAPIBase, envID)
	client := &http.Client{Timeout: upstreamTimeout}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		depth, err := queueDepth(client, statsURL, apiKey)
		if err != nil {
			// Fail the scrape with a non-2xx. KEDA then holds the
			// replica count rather than reading a misleading depth:0
			// and scaling the pool to zero on a transient API error.
			log.Printf("metrics-shim: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]int64{"depth": depth}); err != nil {
			log.Printf("metrics-shim: write response: %v", err)
		}
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	// #nosec G706 -- envID is an operator-set config value, not request data.
	log.Printf("metrics-shim: serving work-queue depth for environment %s on %s", envID, listenAddr)
	log.Fatal(srv.ListenAndServe())
}

// queueDepth calls the Managed Agents work-stats endpoint and returns the
// pending-work depth. It errors unless the upstream answered 200 with a
// numeric top-level `depth` — the caller turns any error into a failed
// scrape rather than reporting a misleading zero.
func queueDepth(client *http.Client, url, apiKey string) (int64, error) {
	// #nosec G704 -- url is operator-controlled: a constant API base
	// joined to the CRD-supplied environment ID, never external input.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", anthropicBeta)

	// #nosec G704 -- the request target is the operator-controlled work/stats URL.
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call work/stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		return 0, fmt.Errorf("work/stats returned HTTP %d: %s", resp.StatusCode, body)
	}

	var stats struct {
		Depth *int64 `json:"depth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, fmt.Errorf("decode work/stats: %w", err)
	}
	if stats.Depth == nil {
		return 0, errors.New("work/stats response has no numeric depth")
	}
	return *stats.Depth, nil
}
