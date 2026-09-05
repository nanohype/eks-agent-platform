/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package operatorconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// record is one log line, flattened so a test can ask what it said.
type record struct {
	level int
	msg   string
	kv    string
}

// The lock lives behind the pointer, not in the sink: logr copies a LogSink by
// value on WithValues and WithName, and a mutex copied is a mutex that guards
// nothing.
type journal struct {
	mu    sync.Mutex
	items []record
}

func (j *journal) add(r record) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.items = append(j.items, r)
}

func (j *journal) all() []record {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]record(nil), j.items...)
}

type sink struct{ journal *journal }

func (s sink) Init(logr.RuntimeInfo)          {}
func (s sink) Enabled(int) bool               { return true }
func (s sink) WithValues(...any) logr.LogSink { return s }
func (s sink) WithName(string) logr.LogSink   { return s }
func (s sink) Info(level int, msg string, kv ...any) {
	s.journal.add(record{level: level, msg: msg, kv: fmt.Sprint(kv...)})
}
func (s sink) Error(_ error, msg string, kv ...any) {
	s.journal.add(record{level: -1, msg: msg, kv: fmt.Sprint(kv...)})
}

func capture() (logr.Logger, *journal) {
	j := &journal{}
	return logr.New(sink{journal: j}), j
}

func completeParams() map[string]string {
	return map[string]string{
		"agent-iam/operator_role_arn":               "arn:aws:iam::123456789012:role/op",
		"agent-iam/tenant_iam_path":                 "/tenants/",
		"agent-iam/tenant_baseline_policy_arn":      "arn:aws:iam::123456789012:policy/base",
		"agent-iam/tenant_permissions_boundary_arn": "arn:aws:iam::123456789012:policy/boundary",
		"model-artifacts/bucket_name":               "artifacts",
	}
}

// TestRequiredKeysGoThroughAssign binds the two mentions of every required key.
//
// assign owns the key -> field mapping and requiredKeys owns field -> key. A
// rename on one side and not the other would leave the operator reporting a key
// that does not exist, which sends a reader to the wrong place in SSM. So every
// key here is driven THROUGH assign and the getter must see it.
func TestRequiredKeysGoThroughAssign(t *testing.T) {
	if len(requiredKeys) == 0 {
		t.Fatal("no required key is declared, so this asserts nothing")
	}
	for _, r := range requiredKeys {
		cfg := &Config{}
		cfg.assign(r.Key, "sentinel")
		if got := r.Get(cfg); got != "sentinel" {
			t.Errorf("assign(%q) did not reach the getter declared beside it (got %q) — "+
				"the key was renamed on one side only, and a report naming it would point at "+
				"a parameter nobody publishes", r.Key, got)
		}
	}
}

// TestMissingKeysAndValidateAgree keeps the two answers about one question the
// same size: Validate says WHETHER, MissingKeys says WHAT to look at.
func TestMissingKeysAndValidateAgree(t *testing.T) {
	full := &Config{}
	for k, v := range completeParams() {
		full.assign(k, v)
	}
	if got := full.MissingKeys(); len(got) != 0 {
		t.Fatalf("a complete config reports missing keys: %v", got)
	}
	if got := full.Validate(); len(got) != 0 {
		t.Fatalf("a complete config fails Validate: %v", got)
	}
	for _, r := range requiredKeys {
		partial := &Config{}
		for k, v := range completeParams() {
			if k == r.Key {
				continue
			}
			partial.assign(k, v)
		}
		missing, invalid := partial.MissingKeys(), partial.Validate()
		if len(missing) != 1 || missing[0] != r.Key {
			t.Errorf("dropping %q gives MissingKeys %v, want exactly that key", r.Key, missing)
		}
		if len(invalid) != len(missing) {
			t.Errorf("dropping %q: Validate reports %d and MissingKeys reports %d — one of them "+
				"has a required field the other does not", r.Key, len(invalid), len(missing))
		}
	}
}

// TestValidPath refuses only what cannot be a path, and refuses it before any call.
func TestValidPath(t *testing.T) {
	for _, ok := range []string{"development-platform", "dev_analytics", "a.b-c", "x"} {
		if err := ValidPath(ok); err != nil {
			t.Errorf("ValidPath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "has/slash", "star*", "quote'"} {
		if err := ValidPath(bad); err == nil {
			t.Errorf("ValidPath(%q) = nil, and that name cannot occupy a parameter path level", bad)
		}
	}
}

// TestAwait_RefusesTheRequestItCannotMakeWithoutCalling proves the one thing
// classified at startup is classified before the first sweep — waiting on a
// request that can never be legal is the failure mode this avoids.
func TestAwait_RefusesTheRequestItCannotMakeWithoutCalling(t *testing.T) {
	log, _ := capture()
	fake := &fakeSSM{params: completeParams()}
	a := &Awaiter{SSM: fake, ClusterName: "not a path", Region: "us-west-2", Log: log,
		Interval: time.Millisecond}
	if _, err := a.Await(context.Background()); err == nil {
		t.Fatal("Await accepted a cluster name that cannot appear in a parameter path")
	}
	if fake.calls != 0 {
		t.Errorf("Await made %d SSM call(s) for a path it could not legally ask for", fake.calls)
	}
}

// TestAwait_WaitsForALateSubstrate is the unit half of the behavioural gate: an
// absence is waited on, not concluded from.
func TestAwait_WaitsForALateSubstrate(t *testing.T) {
	log, journal := capture()
	fake := &fakeSSM{params: map[string]string{}}
	sweeps := 0
	a := &Awaiter{
		SSM: fake, ClusterName: "dev-analytics", Region: "us-west-2", Log: log,
		Interval: time.Hour, ReportAfter: time.Hour,
		now: func() time.Time { return time.Unix(0, 0) },
		sleep: func(context.Context, time.Duration) error {
			sweeps++
			if sweeps == 3 {
				fake.params = completeParams()
			}
			return nil
		},
	}
	cfg, err := a.Await(context.Background())
	if err != nil {
		t.Fatalf("Await returned an error for an absence: %v", err)
	}
	if cfg.TenantPermissionsBoundaryARN == "" {
		t.Fatal("Await returned a config that does not validate")
	}
	if sweeps != 3 {
		t.Errorf("waited %d time(s), want 3 — the substrate arrived on the fourth sweep", sweeps)
	}
	for _, r := range journal.all() {
		if r.level == -1 {
			t.Errorf("an absence younger than ReportAfter was logged at error level: %q", r.msg)
		}
	}
}

// TestAwait_ReportsPersistenceAndNamesNoCause is the property.
//
// After the declared interval the operator says what it knows — how long, what
// is missing, and which component publishes it — and it does not say why,
// because from here the two reasons look the same. A line that named one would
// be a guess wearing a fact's clothes, and it would send a reader to the wrong
// system.
func TestAwait_ReportsPersistenceAndNamesNoCause(t *testing.T) {
	log, journal := capture()
	fake := &fakeSSM{params: map[string]string{}}
	clock := time.Unix(0, 0)
	stop := errors.New("enough")
	waits := 0
	a := &Awaiter{
		SSM: fake, ClusterName: "dev-analytics", Region: "us-west-2", Log: log,
		Interval: time.Minute, ReportAfter: 10 * time.Minute,
		now: func() time.Time { return clock },
		sleep: func(context.Context, time.Duration) error {
			waits++
			clock = clock.Add(time.Minute)
			if waits > 12 {
				return stop
			}
			return nil
		},
	}
	if _, err := a.Await(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("Await = %v, want the harness's own stop", err)
	}

	var reports []record
	for _, r := range journal.all() {
		if r.level == -1 {
			reports = append(reports, r)
		}
	}
	if len(reports) == 0 {
		t.Fatal("the absence outlived ReportAfter and nothing was reported at error level")
	}
	said := reports[0].msg + " " + reports[0].kv
	for _, want := range []string{
		"tenant_permissions_boundary_arn", // what is missing
		"agent-iam",                       // which component publishes it
		"/eks-agent-platform/dev-analytics/",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the report does not name %q, so it does not say where to look.\n  said: %s", want, said)
		}
	}
	// A cause is exactly what cannot be read off an absence. These are the words
	// that would assert one; the report has to describe persistence instead.
	for _, forbidden := range []string{"misconfigur", "typo", "wrong cluster", "invalid config", "will never"} {
		if strings.Contains(strings.ToLower(said), forbidden) {
			t.Errorf("the report asserts a cause (%q) it cannot know from an absence.\n  said: %s", forbidden, said)
		}
	}
	// Reported once, then it keeps waiting. Every subsequent line at error level
	// would be the same fact, and a fact repeated every interval is noise that
	// buries the one that mattered.
	if len(reports) < 2 {
		t.Fatalf("only %d report(s); the wait continued so the state was still true", len(reports))
	}
}
