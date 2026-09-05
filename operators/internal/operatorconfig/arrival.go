/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package operatorconfig

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// WHY WAITING IS THE DEFAULT, AND WHY NOTHING HERE NAMES A CAUSE
//
// The operator's substrate is published by a different system on a different
// clock. The GitOps catalog installs this operator as soon as the cluster
// exists; the IAM roles and the SSM parameters it reads arrive later, from the
// landing zone. Nothing orders the two and nothing should have to.
//
// So at startup a required value can be absent for two reasons that want
// opposite answers: it has not been created yet, and it is never going to be.
// AT A SINGLE INSTANT THOSE ARE THE SAME OBSERVATION. A cluster name with a
// typo produces a sweep that returns nothing; so does a correct cluster name
// read before the landing zone applied. The bytes are identical. An operator
// that concludes "misconfigured" from one look has invented information it does
// not have, and it sends whoever reads that line to the wrong system.
//
// The only thing that separates the two is time. This waits, and after a
// declared interval it reports a FACT — still absent after N, this is what is
// missing, this is the component whose prefix it sits under. Persistence is the
// discriminator, and it is reported as persistence rather than translated into
// a cause.
//
// The asymmetry is what makes waiting the default rather than a preference:
// over-waiting costs a bounded delay and a loud report, and concluding wrongly
// costs an operator a debugging session against a configuration that is correct.
//
// WHAT IS CLASSIFIED AT STARTUP, AND WHAT CANNOT BE
//
// One shape settles immediately, and it is the one the operator owns: the
// request it is about to make. The SSM path is built here, from the cluster
// name, so a cluster name that cannot appear in a parameter path makes a
// request no amount of waiting will make legal. That is refused before the
// first call rather than retried forever.
//
// Everything else is a fact about the answer rather than the question, and none
// of it settles at startup:
//
//   - An empty sweep. A path that does not exist is not an error to SSM; it is
//     an empty page. Absent-because-early and absent-because-wrong arrive the
//     same way.
//   - A credentials failure. The role this pod assumes may not exist yet, or
//     the ServiceAccount may name one that never will.
//   - AccessDenied. The policy may not be attached yet, or may be attached to
//     something else.
//   - A value that is present and wrong. A permissions-boundary ARN naming the
//     wrong policy is well-formed, and IAM accepts it. Nothing observable here
//     separates it from the right one.
//
// Fields whose absence is merely degrading are not in this set at all — they
// are the reconcilers' business, and Validate is what draws the line.

// requiredKey binds a required Config field to the SSM key that carries it.
//
// Validate answers WHETHER something is missing; this answers WHAT to go and
// look at, which is the SSM key rather than the Go field name. The component
// that publishes it is the key's first segment, because that is how the subtree
// is laid out — derived from the key rather than recorded a second time beside
// it.
//
// assign keeps its switch, so this is a second mention of the same key. A test
// in this package drives every key here THROUGH assign and requires the getter
// to see it, so the two cannot drift into disagreeing about what a key means.
type requiredKey struct {
	Key   string
	Field string
	Get   func(*Config) string
}

// WHAT MAKES A VALUE REQUIRED
//
// Each of these is a value whose ABSENCE MAKES THE OPERATOR DO SOMETHING OTHER
// THAN WHAT THE SUBSTRATE SPECIFIED, silently. Empty, the boundary is not set on
// a tenant role, the baseline policy is not attached, the bucket policy is not
// written, and the role is created at a path this code picked instead of the one
// the landing zone published. Every one of those is a smaller grant or a
// different placement than the substrate asked for, with nothing failing.
//
// That is the test, and it is not "the substrate publishes it". The operator's
// own role ARN is published beside these four and is not here: no code path in
// this binary reads it, so its absence changes nothing the operator does, and
// requiring it would refuse to reconcile any tenant over a value that reconciles
// nothing. Its consumer is the kill-switch component, which reads the parameter
// out of SSM itself.
//
// A test in cmd drives every key here from a parameter store through Load and
// into the config struct a reconciler is given, so a key that reaches no
// consumer fails rather than sitting in this list.
var requiredKeys = []requiredKey{
	{"agent-iam/tenant_iam_path", "TenantIAMPath", func(c *Config) string { return c.TenantIAMPath }},
	{"agent-iam/tenant_baseline_policy_arn", "TenantBaselinePolicyARN", func(c *Config) string { return c.TenantBaselinePolicyARN }},
	{"agent-iam/tenant_permissions_boundary_arn", "TenantPermissionsBoundaryARN", func(c *Config) string { return c.TenantPermissionsBoundaryARN }},
	{"model-artifacts/bucket_name", "ArtifactsBucketName", func(c *Config) string { return c.ArtifactsBucketName }},
}

// RequiredKeys is the SSM keys whose absence stops the operator reconciling.
func RequiredKeys() []string {
	out := make([]string, 0, len(requiredKeys))
	for _, r := range requiredKeys {
		out = append(out, r.Key)
	}
	return out
}

// MissingKeys is what to go and look at, for a Config that does not validate.
//
// Empty when Validate is empty, and the two are held to that by a test: a
// required field with no key here would be reported as missing by one and not
// by the other.
func (c *Config) MissingKeys() []string {
	missing := []string{}
	for _, r := range requiredKeys {
		if r.Get(c) == "" {
			missing = append(missing, r.Key)
		}
	}
	return missing
}

// producerOf is the component that publishes a key: the first segment of the
// key, which is the component's own prefix in the subtree.
func producerOf(key string) string {
	if component, _, found := strings.Cut(key, "/"); found {
		return component
	}
	return key
}

// Awaiter polls for this cluster's substrate until it is complete.
type Awaiter struct {
	SSM         awsclients.SSM
	ClusterName string
	Environment string
	Region      string

	// Interval is how often the sweep is retried. It bounds the idle time
	// between the substrate arriving and this operator noticing it, which is the
	// whole cost being removed: exiting hands that interval to CrashLoopBackOff,
	// whose backoff is exponential and unrelated to how long the dependency took.
	Interval time.Duration

	// ReportAfter is how long an absence may persist before it is reported as
	// having persisted. It is not a deadline and nothing changes when it passes
	// except the volume: the report is the same fact, said where an operator
	// will see it.
	ReportAfter time.Duration

	Log logr.Logger

	// now and sleep are injectable so a test can drive the loop without
	// spending the interval. Nothing else replaces them.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (a *Awaiter) timeNow() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Awaiter) wait(ctx context.Context, d time.Duration) error {
	if a.sleep != nil {
		return a.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Await returns once the substrate is complete, or when ctx ends.
//
// It returns an error for exactly one thing: a request this operator cannot
// legally make. Every other outcome is an absence, and an absence is waited on.
func (a *Awaiter) Await(ctx context.Context) (*Config, error) {
	if err := ValidPath(a.ClusterName); err != nil {
		return nil, err
	}

	started := a.timeNow()
	reported := false
	for attempt := 1; ; attempt++ {
		cfg, err := Load(ctx, a.SSM, a.ClusterName, a.Environment, a.Region)
		var absence string
		switch {
		case err != nil:
			absence = "the sweep did not complete: " + err.Error()
		default:
			if missing := cfg.MissingKeys(); len(missing) > 0 {
				absence = "the sweep completed and these keys are not in it: " + strings.Join(missing, ", ")
			}
		}
		if absence == "" {
			if reported {
				a.Log.Info("operator substrate arrived",
					"waited", a.timeNow().Sub(started).Round(time.Second).String(),
					"attempts", attempt)
			}
			return cfg, nil
		}

		waited := a.timeNow().Sub(started)
		if waited >= a.ReportAfter {
			// A fact, not a diagnosis. Which of the two causes this is cannot be
			// read off the absence, so it is not named here — only how long it
			// has lasted, what is missing, and who publishes it.
			a.Log.Error(nil, "operator substrate still absent; not reconciling and not ready",
				"waitedFor", waited.Round(time.Second).String(),
				"attempts", attempt,
				"absence", absence,
				"ssmPrefix", clusterPrefix(a.ClusterName),
				"publishedBy", a.publishers(cfg, err),
				"note", "this says the values have not arrived, not why. An absence this old is "+
					"no longer explained by ordering, and the two remaining explanations — not "+
					"created, or created elsewhere — look the same from here.")
			reported = true
		} else {
			a.Log.Info("waiting for the operator substrate",
				"waitedFor", waited.Round(time.Second).String(),
				"absence", absence,
				"ssmPrefix", clusterPrefix(a.ClusterName))
		}

		if err := a.wait(ctx, a.Interval); err != nil {
			return nil, err
		}
	}
}

// publishers names the components behind whatever is missing.
//
// A sweep that did not complete tells us nothing about which key is absent, so
// every component that publishes a required key is named. That is the honest
// answer: the failure was upstream of knowing.
func (a *Awaiter) publishers(cfg *Config, loadErr error) []string {
	keys := []string{}
	if loadErr != nil || cfg == nil {
		for _, r := range requiredKeys {
			keys = append(keys, r.Key)
		}
	} else {
		keys = cfg.MissingKeys()
	}
	seen := map[string]bool{}
	out := []string{}
	for _, key := range keys {
		component := producerOf(key)
		if seen[component] {
			continue
		}
		seen[component] = true
		out = append(out, component)
	}
	return out
}

// Arrival is the manager Runnable that waits for the substrate, wires the
// reconcilers that need it, and reports readiness.
//
// The manager starts first and this runs beside it, which is the whole change:
// the process stays up, /healthz answers, and /readyz says not-ready until the
// values are here. A pod that is running and not ready is a pod whose own
// status carries the fact — where a pod that exited handed the fact to
// CrashLoopBackOff, which reports a restart count and a backoff unrelated to
// what is actually being waited for.
//
// The reconcilers are registered only once the config validates. Registering
// them earlier and letting them short-circuit on a nil client would leave a
// control plane running over a dead data path, which is indistinguishable from
// a working one for as long as anybody looks.
type Arrival struct {
	Awaiter *Awaiter

	// Wire is called once, with a Config that validated. Registering the
	// reconcilers is its job; an error from it ends the manager, because a
	// reconciler that could not be registered is not an absence and waiting
	// does not fix it.
	Wire func(context.Context, *Config) error

	ready atomic.Bool
}

// Start blocks until the substrate arrives and the reconcilers are wired.
func (a *Arrival) Start(ctx context.Context) error {
	cfg, err := a.Awaiter.Await(ctx)
	if err != nil {
		return err
	}
	if err := a.Wire(ctx, cfg); err != nil {
		return err
	}
	a.ready.Store(true)
	return nil
}

// NeedLeaderElection is false: every replica polls and reports its own
// readiness. A standby that cannot say whether its config is present is a
// standby nobody can promote with confidence.
func (a *Arrival) NeedLeaderElection() bool { return false }

// Readyz is the readiness check. Not ready is the honest answer while the
// values are absent — the reconcilers are not registered, so nothing this
// operator exists to do is happening.
func (a *Arrival) Readyz(_ *http.Request) error {
	if a.ready.Load() {
		return nil
	}
	return errNotReady
}

type notReady struct{}

func (notReady) Error() string {
	return "operator substrate has not arrived: no reconciler is registered and nothing is being " +
		"reconciled. See the operator log for what is missing and which component publishes it."
}

var errNotReady = notReady{}
