/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
	"github.com/nanohype/eks-agent-platform/operators/internal/operatorconfig"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(governancev1alpha1.AddToScheme(scheme))
}

// metricsServerOptions builds the manager's metrics-server config. When secure,
// the endpoint serves over HTTPS (controller-runtime generates a self-signed
// cert in-memory when no cert dir is mounted) and every request runs through
// the built-in authentication+authorization filter — the scrape must present a
// bearer token that passes TokenReview and a SubjectAccessReview on the
// /metrics nonResourceURL. An unauthenticated scrape is rejected at the HTTP
// layer (401/403), not only by the NetworkPolicy. Insecure mode (plaintext, no
// filter) exists for local/dev runs where no kube-apiserver is reachable to
// review tokens.
func metricsServerOptions(secure bool, addr string) server.Options {
	opts := server.Options{BindAddress: addr}
	if secure {
		opts.SecureServing = true
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	return opts
}

// Leader election is the manager's own need rather than any controller's, so
// controller-gen generates nothing for it from the reconcilers' markers. Without
// this the operator starts, wins no lease and reconciles nothing, on a pod that
// reports Running — so the grant is declared here, beside the option that
// requires it, rather than living only in the chart where nothing ties it to a
// reason.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// reconcileFloor is the ceiling on one Reconcile call when nothing configured is
// longer, and reconcileCeiling is what the manager is actually given.
//
// The bound belongs on the reconcile rather than on the client. rest.Config.Timeout
// is the obvious-looking place and is the wrong one: client-go copies it onto the
// http.Client the REST client uses and reads it back for every request, and a
// watch is a request. A 30s value would tear down and re-establish every informer
// twice a minute, turning a timeout into a re-list storm against the API server
// the operator depends on.
//
// What bounds a watch is the reflector, and it does so without this operator
// setting anything: client-go puts TimeoutSeconds on each watch request,
// randomized between a five-minute floor and twice that, under the comment "We
// want to avoid situations of hanging watchers. Stop any watchers that do not
// receive any events within the timeout window." The apiserver closes the stream
// at that point and the reflector opens a new one.
//
// A reconcile is what was unbounded, and the host API-server calls it makes are
// the calls with no shorter bound of their own — every other remote this operator
// talks to carries one per request. So this ceiling is not a backstop behind
// tighter limits; for the host client it is the only limit there is, which is why
// it is generous rather than tight.
//
// It must still exceed every bound a reconcile can wait on, and one of those is
// derived rather than declared: the budget reconciler bounds its Athena poll at
// half its requeue interval, so an operator that widens the interval widens that
// bound with it. A ceiling below it would cancel the cost query every tick, and a
// cost query that never completes is a budget cap that is never enforced while
// the scan is billed each time. The ceiling is therefore computed from the same
// flag, not fixed.
const reconcileFloor = 10 * time.Minute

// reconcileCeiling is the per-reconcile deadline for a given configuration: the
// floor, or the longest configured inner bound plus the floor, whichever is
// larger. The floor is the margin — a reconcile is more than its slowest call.
func reconcileCeiling(budgetRequeueInterval time.Duration) time.Duration {
	if inner := controller.BudgetQueryTimeout(budgetRequeueInterval); inner > 0 {
		return inner + reconcileFloor
	}
	return reconcileFloor
}

// controllerOptions carries the per-reconcile ceiling to every controller the
// manager builds. Extracted so a test can read it: the option defaults to zero,
// zero disables the timeout entirely, and at the call site zero is
// indistinguishable from nobody having set it.
func controllerOptions(budgetRequeueInterval time.Duration) crconfig.Controller {
	return crconfig.Controller{ReconciliationTimeout: reconcileCeiling(budgetRequeueInterval)}
}

func main() {
	var metricsAddr string
	var secureMetrics bool
	var probeAddr string
	var enableLeaderElection bool
	var leaderElectionID string
	var budgetRequeueInterval time.Duration
	var killSwitchGraceIntervals int
	var killSwitchMaxRefires int
	var sloRequeueInterval time.Duration
	var sloHoldGraceIntervals int
	// Per-reconciler worker concurrency. Each value is wired into the
	// corresponding reconciler's SetupWithManager via MaxConcurrentReconciles
	// so the operator chart's values.yaml — reconcilers.<x>.concurrent —
	// becomes real `--<x>-workers` flags on the binary.
	var platformWorkers int
	var gatewayWorkers int
	var runtimeWorkers int
	var sandboxWorkers int
	var agentSandboxWorkers int
	var budgetWorkers int
	var sloWorkers int
	var evalWorkers int
	var tenantWorkers int
	var tenantRequeueInterval time.Duration
	var configPollInterval time.Duration
	var configAbsentReportAfter time.Duration

	// shimImage is the operator image; the SandboxPool reconciler runs it
	// as the KEDA metrics bridge (the /metrics-shim binary) for queue-depth
	// autoscaling. Empty disables autoscaling.
	var shimImage string

	// AWS substrate config — these resolve to operatorconfig.Config and the
	// AWS SDK clients at startup. environment + region come from flags or
	// AGENTS_ENVIRONMENT / AGENTS_REGION env vars; everything else flows
	// from SSM under /eks-agent-platform/<cluster-name>/.
	var environment string
	var clusterName string
	var region string
	var networkEngine string
	var disableAWS bool

	// vcluster hard-isolation tier config. The operator declares a per-Platform
	// vcluster as an ArgoCD Application pinned to this chart version; the
	// init-charts JSON bootstraps KEDA inside each vcluster. See
	// docs/adr/0008-vcluster-isolation-tier.md.
	var vclusterChartRepo string
	var vclusterChartVersion string
	var vclusterInitChartsJSON string

	// Org-dimension tag values stamped on tenant roles (resource-tagging
	// standard). Env-level constants for the cluster the operator serves;
	// tenantRoleTags falls back to landing-zone env.hcl defaults when unset.
	var costCenter string
	var businessUnit string
	var dataClassification string
	var compliance string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics over HTTPS and require each scrape to pass authentication + authorization (TokenReview + a SubjectAccessReview on the /metrics nonResourceURL). Set false only for local/dev runs.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "eks-agent-platform.nanohype.dev", "Leader election lock name.")
	flag.DurationVar(&budgetRequeueInterval, "budget-requeue-interval", time.Hour, "How often the budget reconciler ticks.")
	flag.IntVar(&killSwitchGraceIntervals, "killswitch-grace-intervals", 3, "How many budget-requeue-intervals to wait after firing before treating an un-suspended platform as an unrouted breach.")
	flag.DurationVar(&sloRequeueInterval, "slo-requeue-interval", 5*time.Minute, "How often the SLOPolicy reconciler evaluates burn rate. Must not exceed the shortest burn window (5m) or a fast burn is stepped over entirely.")
	flag.IntVar(&sloHoldGraceIntervals, "slo-hold-grace-intervals", 2, "How many slo-requeue-intervals an engaged rollout hold may go unobserved on the tenant's AppProject before the reconciler reports it as not landing.")
	flag.IntVar(&killSwitchMaxRefires, "killswitch-max-refires", 5, "Maximum times a single unrouted breach is re-published before the reconciler stops re-firing (the KillSwitchUnrouted condition + alert stay set).")
	flag.IntVar(&platformWorkers, "platform-workers", 3, "MaxConcurrentReconciles for the Platform reconciler.")
	flag.IntVar(&gatewayWorkers, "gateway-workers", 3, "MaxConcurrentReconciles for the ModelGateway reconciler.")
	flag.IntVar(&runtimeWorkers, "runtime-workers", 5, "MaxConcurrentReconciles for the AgentFleet (runtime) reconciler.")
	flag.IntVar(&sandboxWorkers, "sandbox-workers", 3, "MaxConcurrentReconciles for the SandboxPool reconciler.")
	flag.IntVar(&agentSandboxWorkers, "agentsandbox-workers", 5, "MaxConcurrentReconciles for the AgentSandbox reconciler.")
	flag.IntVar(&budgetWorkers, "budget-workers", 1, "MaxConcurrentReconciles for the Budget reconciler.")
	flag.IntVar(&sloWorkers, "slo-workers", 2, "MaxConcurrentReconciles for the SLOPolicy reconciler.")
	flag.IntVar(&evalWorkers, "eval-workers", 2, "MaxConcurrentReconciles for the EvalSuite reconciler.")
	flag.IntVar(&tenantWorkers, "tenant-workers", 1, "MaxConcurrentReconciles for the Tenant reconciler.")
	flag.DurationVar(&tenantRequeueInterval, "tenant-requeue-interval", 5*time.Minute, "How often the Tenant reconciler re-aggregates owned Platforms.")
	flag.DurationVar(&configPollInterval, "config-poll-interval", 15*time.Second,
		"How often to re-read the operator substrate from SSM while it is incomplete. This bounds the "+
			"idle time between the substrate arriving and this operator noticing, which is the cost being "+
			"removed: exiting instead hands that interval to CrashLoopBackOff, whose backoff is exponential, "+
			"caps at five minutes, and is unrelated to how long the dependency actually took.")
	flag.DurationVar(&configAbsentReportAfter, "config-absent-report-after", 15*time.Minute,
		"How long the substrate may be absent before the wait is reported at error level. It is not a "+
			"deadline and nothing changes when it passes except the volume: the operator keeps waiting, "+
			"because absence at any single moment cannot tell a substrate that has not been applied yet "+
			"from one that never will be. What crossing this reports is the persistence, which is the only "+
			"thing that does separate them. Set it above the ordering window between the catalog that "+
			"installs this operator and the landing zone that publishes what it reads.")
	flag.StringVar(&shimImage, "shim-image", os.Getenv("AGENTS_SHIM_IMAGE"), "Operator image used for the SandboxPool KEDA metrics bridge. Empty disables queue-depth autoscaling.")
	flag.StringVar(&environment, "environment", os.Getenv("AGENTS_ENVIRONMENT"), "Environment name (development/staging/production). Stamped on every operator-created object as the platform.nanohype.dev/environment label, and on the pods it builds as the OTel deployment.environment resource attribute. Omitted from both when unset rather than emitted empty.")
	flag.StringVar(&clusterName, "cluster-name", os.Getenv("AGENTS_CLUSTER_NAME"), "Full EKS cluster name this operator serves (e.g. dev-analytics). Keys the SSM-config subtree and prefixes every resource the operator mints, isolating co-located sibling clusters.")
	flag.StringVar(&region, "region", os.Getenv("AGENTS_REGION"), "AWS region. Defaults to credential-chain region if empty.")
	flag.StringVar(&costCenter, "cost-center", os.Getenv("AGENTS_COST_CENTER"), "Org cost-center tag stamped on tenant roles (resource-tagging standard).")
	flag.StringVar(&businessUnit, "business-unit", os.Getenv("AGENTS_BUSINESS_UNIT"), "Org business-unit tag stamped on tenant roles.")
	flag.StringVar(&dataClassification, "data-classification", os.Getenv("AGENTS_DATA_CLASSIFICATION"), "Org data-classification tag stamped on tenant roles.")
	flag.StringVar(&compliance, "compliance", os.Getenv("AGENTS_COMPLIANCE"), "Org compliance tag stamped on tenant roles.")
	flag.StringVar(&networkEngine, "network-engine", os.Getenv("AGENTS_NETWORK_ENGINE"), "Network policy engine for tenant/fleet egress: cilium (default; required so Pod Identity creds-endpoint egress is allowed) or kubernetes.")
	flag.BoolVar(&disableAWS, "disable-aws", false, "Skip AWS client init + SSM config load (k8s-side reconciliation only).")
	flag.StringVar(&vclusterChartRepo, "vcluster-chart-repo", os.Getenv("AGENTS_VCLUSTER_CHART_REPO"), "vcluster Helm chart repository the operator declares per vcluster-tier Platform (default https://charts.loft.sh).")
	flag.StringVar(&vclusterChartVersion, "vcluster-chart-version", os.Getenv("AGENTS_VCLUSTER_CHART_VERSION"), "Pinned vcluster chart version (targetRevision). Renovate proposes bumps; a human reviews — never auto-upgraded.")
	flag.StringVar(&vclusterInitChartsJSON, "vcluster-init-charts", os.Getenv("AGENTS_VCLUSTER_INIT_CHARTS"), "JSON array of Helm charts bootstrapped INSIDE each per-Platform vcluster (KEDA) via experimental.deploy.vcluster.helm. Empty ships a containment-only vcluster — a fleet still runs, at its static replica count.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if networkEngine == "" {
		networkEngine = controller.NetworkEngineCilium
	}

	// The cluster name qualifies every identity the operator mints or queries —
	// tenant and session role names, and the PlatformId cost-attribution tag the
	// budget reconciler both stamps and filters on. Empty, none of those fail:
	// they render one hyphen shorter and keep working, so a Platform's roles and
	// its CUR rows silently drop the discriminator that keeps two clusters in one
	// account apart. Refuse rather than mint them.
	//
	// Gated on the AWS path, because that is where the discriminator does work.
	// Under --disable-aws there is no IAM client, so no role and no tag is ever
	// created, and the chart ships clusterName empty by default for exactly that
	// shape — the local kind install sets --disable-aws and nothing else. Refusing
	// there would turn every documented local install into a CrashLoopBackOff
	// without protecting anything. The budget reconciler holds the same line
	// unconditionally at the point a missing name would produce a number.
	if !disableAWS && clusterName == "" {
		setupLog.Error(nil, "--cluster-name (or AGENTS_CLUSTER_NAME) is required and was empty; refusing to start",
			"why", "it qualifies tenant role names and the PlatformId cost-attribution tag, and an empty value degrades silently rather than failing")
		os.Exit(1)
	}

	// AWS clients. Building them is local: it resolves the SDK's configuration
	// chain and constructs service clients, and it reaches nothing. A failure
	// here is this process's own environment, it does not become correct later,
	// and so it exits.
	//
	// The substrate those clients READ is a different question, and it is no
	// longer answered here. It is published by another system on another clock —
	// the catalog installs this operator as soon as the cluster exists, and the
	// landing zone applies the roles and parameters afterwards — so its absence at
	// this instant says nothing about whether it is coming. Waiting for it happens
	// beside a running manager, in the Arrival runnable below.
	var awsClients *awsclients.Clients
	if !disableAWS {
		var awsErr error
		awsClients, awsErr = awsclients.New(context.Background(), region)
		if awsErr != nil {
			setupLog.Error(awsErr, "unable to build AWS clients; refusing to start")
			os.Exit(1)
		}
	} else {
		setupLog.Info("--disable-aws set; running without AWS clients (k8s-side only)")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions(secureMetrics, metricsAddr),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		Controller:             controllerOptions(budgetRequeueInterval),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// vcluster tier: build the config + the client factory shared by the Platform,
	// AgentFleet, and AgentSandbox reconcilers. Defaults keep a fresh install on
	// the current stable chart line; the factory is nil-safe for the namespace
	// tier (it is only consulted when spec.isolation == vcluster).
	if vclusterChartRepo == "" {
		vclusterChartRepo = "https://charts.loft.sh"
	}
	if vclusterChartVersion == "" {
		vclusterChartVersion = "0.35.2"
	}
	vclusterCfg := controller.VClusterConfig{
		ChartRepoURL: vclusterChartRepo,
		ChartVersion: vclusterChartVersion,
	}
	if vclusterInitChartsJSON != "" {
		if err := json.Unmarshal([]byte(vclusterInitChartsJSON), &vclusterCfg.InitCharts); err != nil {
			setupLog.Error(err, "invalid --vcluster-init-charts JSON; refusing to start")
			os.Exit(1)
		}
	}
	vclusterFactory := controller.NewVClusterClientFactory(mgr.GetClient(), mgr.GetScheme())

	// Registering the reconcilers needs the substrate, so it is a function of it.
	// Nothing here runs until a Config that validates exists — a reconciler
	// registered against absent values and short-circuiting on a nil client would
	// be a control plane running over a dead data path, which looks exactly like a
	// working one for as long as anybody looks.
	wire := func(ctx context.Context, opConfig *operatorconfig.Config) error {
		platformReconciler := &controller.PlatformReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			Concurrency:   platformWorkers,
			NetworkEngine: networkEngine,
			VCluster:      vclusterFactory,
			VClusterCfg:   vclusterCfg,
			// Recorder carries the warnings a status condition alone would not
			// reach: `kubectl describe platform` and anything watching events see a
			// declared capability that resolved to no grant, without polling the CR.
			Recorder: mgr.GetEventRecorder("platform-controller"),
		}
		if awsClients != nil {
			platformReconciler.IAM = awsClients.IAM
			platformReconciler.EKS = awsClients.EKS
			platformReconciler.S3 = awsClients.S3
			platformReconciler.SSM = awsClients.SSM
			platformReconciler.Scheduler = awsClients.Scheduler
			platformReconciler.IAMCfg = iamConfigFrom(opConfig, orgTags{
				Environment:        environment,
				CostCenter:         costCenter,
				BusinessUnit:       businessUnit,
				DataClassification: dataClassification,
				Compliance:         compliance,
			})
			platformReconciler.AWSCfg = platformAWSConfigFrom(opConfig, environment)
		}
		if err := platformReconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the Platform reconciler: %w", err)
		}
		gatewayReconciler := &controller.ModelGatewayReconciler{
			Client:      mgr.GetClient(),
			Scheme:      mgr.GetScheme(),
			Concurrency: gatewayWorkers,
			Region:      region,
		}
		if opConfig != nil {
			gatewayReconciler.GuardrailID = opConfig.BaselineGuardrailID
			gatewayReconciler.GuardrailVersion = opConfig.BaselineGuardrailVersion
		}
		if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the ModelGateway reconciler: %w", err)
		}
		if err := (&controller.AgentFleetReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			Concurrency:   runtimeWorkers,
			NetworkEngine: networkEngine,
			Region:        region,
			Environment:   environment,
			VCluster:      vclusterFactory,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the AgentFleet reconciler: %w", err)
		}
		if err := (&controller.SandboxPoolReconciler{
			Client:      mgr.GetClient(),
			Scheme:      mgr.GetScheme(),
			Concurrency: sandboxWorkers,
			ShimImage:   shimImage,
			Environment: environment,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the SandboxPool reconciler: %w", err)
		}
		if err := (&controller.AgentSandboxReconciler{
			Client:      mgr.GetClient(),
			Scheme:      mgr.GetScheme(),
			Concurrency: agentSandboxWorkers,
			VCluster:    vclusterFactory,
			Environment: environment,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the AgentSandbox reconciler: %w", err)
		}
		budgetReconciler := &controller.BudgetReconciler{
			Client:          mgr.GetClient(),
			Scheme:          mgr.GetScheme(),
			Concurrency:     budgetWorkers,
			RequeueInterval: budgetRequeueInterval,
			// Identity, not an AWS client — set on every path, including --disable-aws.
			// It is guaranteed non-empty by the guard above. Setting it alongside the
			// clients would leave it empty whenever they are absent, which is the one
			// arrangement where an identity silently loses its discriminator.
			ClusterName:              clusterName,
			KillSwitchGraceIntervals: killSwitchGraceIntervals,
			KillSwitchMaxRefires:     killSwitchMaxRefires,
		}
		if awsClients != nil {
			budgetReconciler.Athena = awsClients.Athena
			budgetReconciler.CloudWatch = awsClients.CloudWatch
			budgetReconciler.EventBridge = awsClients.EventBridge
			budgetReconciler.AthenaCfg = controller.AthenaConfig{
				Workgroup:    opConfig.AthenaWorkgroup,
				Database:     opConfig.AthenaDatabase,
				CURTableName: opConfig.CURTableName,
			}
			budgetReconciler.KillSwitchEventBusName = opConfig.KillSwitchEventBusName
		}
		if err := budgetReconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the Budget reconciler: %w", err)
		}
		sloReconciler := &controller.SLOReconciler{
			Client:             mgr.GetClient(),
			Scheme:             mgr.GetScheme(),
			Concurrency:        sloWorkers,
			RequeueInterval:    sloRequeueInterval,
			HoldGraceIntervals: sloHoldGraceIntervals,
		}
		if awsClients != nil {
			sloReconciler.EventBridge = awsClients.EventBridge
			sloReconciler.KillSwitchEventBusName = opConfig.KillSwitchEventBusName
			// The AMP query client can only be built here, not in awsclients.New:
			// its endpoint comes from SSM, and reading SSM needs the clients New
			// returns. A cluster without managed-monitoring publishes no endpoint,
			// and the reconciler degrades to a MetricStoreUnavailable condition
			// rather than failing startup — enable_managed_monitoring is opt-in.
			if opConfig.AMPEndpoint != "" {
				ampQuery, err := awsclients.NewPrometheusQuery(awsClients.AWSConfig, opConfig.AMPEndpoint)
				if err != nil {
					setupLog.Error(err, "unable to build the AMP query client; SLO burn-rate evaluation is disabled", "endpoint", opConfig.AMPEndpoint)
				} else {
					sloReconciler.Prometheus = ampQuery
				}
			} else {
				setupLog.Info("no managed-monitoring AMP endpoint published for this cluster; SLO burn-rate evaluation will retry from SSM")
			}

			// The endpoint can appear after this process starts, and on a first
			// install it usually does: managed-monitoring publishes the parameter,
			// and it is applied AFTER the cluster that hosts this operator. The
			// endpoint is not a chart value, so ArgoCD sees no manifest change when
			// it lands and nothing restarts the pod — which made a nil client at
			// boot permanent, on a cluster whose AMP workspace was up.
			//
			// Re-reading only the one parameter, rather than calling
			// operatorconfig.Load again, keeps this to a single SSM GetParameter and
			// leaves every other startup-validated value exactly as validated.
			sloReconciler.ResolvePrometheus = func(ctx context.Context) (awsclients.PrometheusQuery, error) {
				endpoint, err := operatorconfig.AMPEndpoint(ctx, awsClients.SSM, clusterName, environment, region)
				if err != nil {
					return nil, err
				}
				if endpoint == "" {
					return nil, nil
				}
				return awsclients.NewPrometheusQuery(awsClients.AWSConfig, endpoint)
			}
		}
		if err := sloReconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the SLO reconciler: %w", err)
		}
		evalReconciler := &controller.EvalReconciler{
			Client:      mgr.GetClient(),
			Scheme:      mgr.GetScheme(),
			Concurrency: evalWorkers,
		}
		if opConfig != nil {
			evalReconciler.RunnerNamespace = opConfig.EvalRunnerNamespace
			evalReconciler.RunnerServiceAccount = opConfig.EvalRunnerServiceAccount
			evalReconciler.ReportsBucket = opConfig.EvalReportsBucket
		}
		if err := evalReconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the Eval reconciler: %w", err)
		}
		if err := (&controller.TenantReconciler{
			Client:          mgr.GetClient(),
			Scheme:          mgr.GetScheme(),
			Concurrency:     tenantWorkers,
			RequeueInterval: tenantRequeueInterval,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register the Tenant reconciler: %w", err)
		}
		setupLog.Info("reconcilers registered against the operator substrate",
			"clusterName", opConfig.ClusterName, "environment", opConfig.Environment, "region", opConfig.Region,
			"operatorRoleARN", opConfig.OperatorRoleARN, "tenantIAMPath", opConfig.TenantIAMPath)
		return nil
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	// healthz stays a ping. A pod waiting for a value that has not been published
	// is alive, and a liveness probe answering for the substrate would restart it
	// — which is the loop this exists to remove, rebuilt one layer down.
	//
	// readyz answers for the substrate, because that is the thing that can be
	// absent. Not ready is the honest report while it is: no reconciler is
	// registered, so nothing this operator exists to do is happening, and the
	// pod's own status carries that instead of a restart count.
	readyz := healthz.Ping
	if disableAWS {
		// No substrate to wait for: --disable-aws is the local path, where the
		// reconcilers run k8s-side only and every AWS step short-circuits.
		if err := wire(context.Background(), &operatorconfig.Config{
			ClusterName: clusterName, Environment: environment, Region: region,
		}); err != nil {
			setupLog.Error(err, "unable to register reconcilers; refusing to start")
			os.Exit(1)
		}
	} else {
		arrival := &operatorconfig.Arrival{
			Awaiter: &operatorconfig.Awaiter{
				SSM:         awsClients.SSM,
				ClusterName: clusterName,
				Environment: environment,
				Region:      region,
				Interval:    configPollInterval,
				ReportAfter: configAbsentReportAfter,
				Log:         setupLog,
			},
			Wire: wire,
		}
		if err := mgr.Add(arrival); err != nil {
			setupLog.Error(err, "unable to add the substrate-arrival runnable")
			os.Exit(1)
		}
		readyz = arrival.Readyz
	}
	if err := mgr.AddReadyzCheck("readyz", readyz); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", version())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// orgTags are the deploy-config tag values a tenant role carries. They come from
// flags rather than from the substrate, which is why they are a separate
// argument: the mapping below is what turns published values into the config a
// reconciler is given, and mixing the two would hide which is which.
type orgTags struct {
	Environment        string
	CostCenter         string
	BusinessUnit       string
	DataClassification string
	Compliance         string
}

// iamConfigFrom and platformAWSConfigFrom are the whole route from a value the
// substrate published to the config a reconciler reads.
//
// They are named rather than inline because that route is where a required value
// can go missing without anything failing: a key can be swept out of SSM,
// decoded into a Config field, listed as required — and then not carried across
// here, so the operator refuses to start without a value it never uses. A test
// in this package drives every required key from a parameter store through Load
// and into one of these, which is a claim about behaviour rather than about the
// shape of a struct literal.
//
// The reconcilers deliberately do not import operatorconfig, so this is the last
// point that can see both sides. What happens after — the config field reaching
// an AWS call — is asserted in the controller package's own tests. Neither gate
// spans the boundary alone, and the boundary is a design decision rather than an
// oversight.
func iamConfigFrom(cfg *operatorconfig.Config, tags orgTags) controller.IAMConfig {
	return controller.IAMConfig{
		TenantIAMPath:                cfg.TenantIAMPath,
		TenantBaselinePolicyARN:      cfg.TenantBaselinePolicyARN,
		TenantPermissionsBoundaryARN: cfg.TenantPermissionsBoundaryARN,
		ClusterName:                  cfg.ClusterName,
		Region:                       cfg.Region,
		Environment:                  tags.Environment,
		CostCenter:                   tags.CostCenter,
		BusinessUnit:                 tags.BusinessUnit,
		DataClassification:           tags.DataClassification,
		Compliance:                   tags.Compliance,
	}
}

func platformAWSConfigFrom(cfg *operatorconfig.Config, environment string) controller.PlatformAWSConfig {
	return controller.PlatformAWSConfig{
		ArtifactsBucketName: cfg.ArtifactsBucketName,
		Environment:         environment,
	}
}

// buildVersion is overridden at link time via -ldflags="-X main.buildVersion=...".
// Lowercase so revive doesn't flag it as exported-from-package-main (which
// would be unreachable anyway).
var buildVersion = "dev"

func version() string {
	return fmt.Sprintf("eks-agent-platform-operator/%s", buildVersion)
}
