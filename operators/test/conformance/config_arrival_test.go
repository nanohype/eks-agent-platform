/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nanohype/eks-agent-platform/operators/internal/operatorconfig"
)

// The operator is installed by the GitOps catalog as soon as a cluster exists,
// and the substrate it reads is applied afterwards by the landing zone. Nothing
// orders the two. These drive that, against a real manager and its real probe
// endpoints, because the claim is about what a POD does: an assertion about the
// poll loop's shape would prove nothing about whether the container stays up,
// whether readiness moves, or what an operator reading the log is told.

// withheldSSM publishes the substrate on the Nth sweep. Zero means never.
type withheldSSM struct {
	mu        sync.Mutex
	publishOn int
	sweeps    int
}

func (w *withheldSSM) GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	panic("GetParameter is not on the startup path")
}

func (w *withheldSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweeps++
	// An SSM path that holds nothing is not an error — it is an empty page.
	// That is the shape a cluster whose landing zone has not run yet returns,
	// and it is byte-identical to the shape a misspelled cluster name returns.
	if w.publishOn == 0 || w.sweeps < w.publishOn {
		return &ssm.GetParametersByPathOutput{}, nil
	}
	prefix := aws.ToString(in.Path)
	out := &ssm.GetParametersByPathOutput{}
	for key, value := range map[string]string{
		"agent-iam/operator_role_arn":               "arn:aws:iam::123456789012:role/op",
		"agent-iam/tenant_iam_path":                 "/tenants/",
		"agent-iam/tenant_baseline_policy_arn":      "arn:aws:iam::123456789012:policy/base",
		"agent-iam/tenant_permissions_boundary_arn": "arn:aws:iam::123456789012:policy/boundary",
		"model-artifacts/bucket_name":               "artifacts",
	} {
		out.Parameters = append(out.Parameters, ssmtypes.Parameter{
			Name: aws.String(prefix + key), Value: aws.String(value),
		})
	}
	return out, nil
}

type lines struct {
	mu    sync.Mutex
	items []string
}

func (l *lines) add(s string) { l.mu.Lock(); defer l.mu.Unlock(); l.items = append(l.items, s) }
func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.items...)
}

type collectingSink struct{ errors *lines }

func (c collectingSink) Init(logr.RuntimeInfo)          {}
func (c collectingSink) Enabled(int) bool               { return true }
func (c collectingSink) WithValues(...any) logr.LogSink { return c }
func (c collectingSink) WithName(string) logr.LogSink   { return c }
func (c collectingSink) Info(int, string, ...any)       {}
func (c collectingSink) Error(_ error, msg string, kv ...any) {
	c.errors.add(msg + " " + fmt.Sprint(kv...))
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

func probe(t *testing.T, addr, path string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + path) //nolint:noctx // a local probe in a test
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// startOperator brings up a real manager wired the way cmd/main.go wires it on
// the AWS path, and returns the probe address plus what Start eventually did.
func startOperator(t *testing.T, ssmClient *withheldSSM, log logr.Logger, reportAfter time.Duration) (string, *atomic.Int32, func() (error, bool)) {
	t.Helper()
	addr := freePort(t)
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: addr,
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	var wired atomic.Int32
	arrival := &operatorconfig.Arrival{
		Awaiter: &operatorconfig.Awaiter{
			SSM:         ssmClient,
			ClusterName: "dev-analytics",
			Environment: "dev",
			Region:      "us-west-2",
			Interval:    20 * time.Millisecond,
			ReportAfter: reportAfter,
			Log:         log,
		},
		Wire: func(context.Context, *operatorconfig.Config) error {
			wired.Add(1)
			return nil
		},
	}
	if err := mgr.Add(arrival); err != nil {
		t.Fatalf("add the arrival runnable: %v", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if err := mgr.AddReadyzCheck("readyz", arrival.Readyz); err != nil {
		t.Fatalf("readyz: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()

	stopped := func() (error, bool) {
		select {
		case err := <-done:
			return err, true
		default:
			return nil, false
		}
	}

	// The manager is up once its probe endpoint answers at all. An exit during
	// this window is reported as the exit it is.
	//
	// A TRUE FAILURE MESSAGE THAT NAMES A SYMPTOM RATHER THAN A CAUSE SENDS THE
	// READER TO THE WRONG COMPONENT. Without the exit check below, a manager
	// that died here failed this harness with "the manager never served
	// /healthz" — accurate, and it points at the probe server, which is fine.
	// The cause was that the process had ended.
	//
	// That is the same defect these tests exist to assert about the operator:
	// "still absent after N" is a fact and "misconfigured" is a symptom read as
	// a cause, and the second sends whoever reads it to the wrong system. A
	// harness for that property has to hold itself to it.
	deadline := time.Now().Add(30 * time.Second)
	for probe(t, addr, "/healthz") != http.StatusOK {
		if err, done := stopped(); done {
			t.Fatalf("the manager exited before serving /healthz: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the manager never served /healthz")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return addr, &wired, stopped
}

// TestSubstrateArrivingLate_ReachesReadyWithoutRestarting is the first half.
//
// The values are absent for the first sweeps and then published. The pod must
// stay up through that and become ready by itself — no exit, so no restart, and
// no backoff between the substrate landing and this operator using it.
func TestSubstrateArrivingLate_ReachesReadyWithoutRestarting(t *testing.T) {
	log := logr.New(collectingSink{errors: &lines{}})
	ssmClient := &withheldSSM{publishOn: 4}
	addr, wired, stopped := startOperator(t, ssmClient, log, time.Hour)

	if code := probe(t, addr, "/readyz"); code == http.StatusOK {
		t.Fatal("/readyz answered OK before the substrate was published; readiness is not " +
			"reporting on the thing that is absent")
	}
	if n := wired.Load(); n != 0 {
		t.Fatalf("the reconcilers were wired %d time(s) with no config — a control plane over a "+
			"dead data path is indistinguishable from a working one", n)
	}

	deadline := time.Now().Add(30 * time.Second)
	for probe(t, addr, "/readyz") != http.StatusOK {
		if err, done := stopped(); done {
			t.Fatalf("the manager exited while waiting for the substrate (%v) — that is the "+
				"restart loop this change removes", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("/readyz never became OK after the substrate was published")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err, done := stopped(); done {
		t.Fatalf("the manager exited after becoming ready: %v", err)
	}
	if code := probe(t, addr, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d; a pod that was waiting for a value was never unhealthy, and a "+
			"liveness probe answering for the substrate would have restarted it", code)
	}
	if n := wired.Load(); n != 1 {
		t.Errorf("the reconcilers were wired %d time(s), want exactly 1", n)
	}
}

// TestSubstrateNeverArriving_ReportsWhatIsMissingAndStaysUp is the second half,
// and the property.
//
// Nothing is ever published. The operator cannot tell whether the values are
// late or whether it is pointed somewhere they will never appear, so it says
// what it knows — how long, what is missing, which component publishes it — and
// it does not exit on a cause it cannot read from an absence.
func TestSubstrateNeverArriving_ReportsWhatIsMissingAndStaysUp(t *testing.T) {
	errs := &lines{}
	log := logr.New(collectingSink{errors: errs})
	ssmClient := &withheldSSM{publishOn: 0}
	addr, wired, stopped := startOperator(t, ssmClient, log, 50*time.Millisecond)

	deadline := time.Now().Add(30 * time.Second)
	var report string
	for report == "" {
		for _, line := range errs.all() {
			if strings.Contains(line, "still absent") {
				report = line
				break
			}
		}
		if err, done := stopped(); done {
			t.Fatalf("the manager exited on an absence (%v) — the two reasons an absence has "+
				"look the same from here, so exiting picks one of them without evidence", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the absence outlived the reporting interval and nothing was reported")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, want := range []string{
		"tenant_permissions_boundary_arn",    // what is missing
		"agent-iam",                          // which component publishes it
		"/eks-agent-platform/dev-analytics/", // where to look
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not name %q, so it does not say where to look.\n  said: %s", want, report)
		}
	}
	for _, forbidden := range []string{"misconfigur", "typo", "wrong cluster", "will never"} {
		if strings.Contains(strings.ToLower(report), forbidden) {
			t.Errorf("the report asserts %q, which an absence cannot establish.\n  said: %s", forbidden, report)
		}
	}

	if code := probe(t, addr, "/readyz"); code == http.StatusOK {
		t.Error("/readyz answered OK with no substrate and no reconciler registered")
	}
	if code := probe(t, addr, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d; the process is running and waiting, which is not unhealthy", code)
	}
	if n := wired.Load(); n != 0 {
		t.Errorf("the reconcilers were wired %d time(s) with nothing published", n)
	}
	if err, done := stopped(); done {
		t.Fatalf("the manager exited: %v", err)
	}
}
