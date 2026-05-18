/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
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
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var leaderElectionID string
	var budgetRequeueInterval time.Duration
	// Per-reconciler worker concurrency. Each value is wired into the
	// corresponding reconciler's SetupWithManager via MaxConcurrentReconciles
	// so the operator chart's values.yaml — reconcilers.<x>.concurrent —
	// becomes real `--<x>-workers` flags on the binary.
	var platformWorkers int
	var gatewayWorkers int
	var runtimeWorkers int
	var budgetWorkers int
	var evalWorkers int
	var tenantWorkers int
	var tenantRequeueInterval time.Duration

	// AWS substrate config — these resolve to operatorconfig.Config and the
	// AWS SDK clients at startup. environment + region come from flags or
	// AGENTS_ENVIRONMENT / AGENTS_REGION env vars; everything else flows
	// from SSM under /eks-agent-platform/<environment>/.
	var environment string
	var region string
	var oidcProviderARN string
	var oidcIssuerHost string
	var disableAWS bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "eks-agent-platform.agents.stxkxs.io", "Leader election lock name.")
	flag.DurationVar(&budgetRequeueInterval, "budget-requeue-interval", time.Hour, "How often the budget reconciler ticks.")
	flag.IntVar(&platformWorkers, "platform-workers", 3, "MaxConcurrentReconciles for the Platform reconciler.")
	flag.IntVar(&gatewayWorkers, "gateway-workers", 3, "MaxConcurrentReconciles for the ModelGateway reconciler.")
	flag.IntVar(&runtimeWorkers, "runtime-workers", 5, "MaxConcurrentReconciles for the AgentFleet (runtime) reconciler.")
	flag.IntVar(&budgetWorkers, "budget-workers", 1, "MaxConcurrentReconciles for the Budget reconciler.")
	flag.IntVar(&evalWorkers, "eval-workers", 2, "MaxConcurrentReconciles for the EvalSuite reconciler.")
	flag.IntVar(&tenantWorkers, "tenant-workers", 1, "MaxConcurrentReconciles for the Tenant reconciler.")
	flag.DurationVar(&tenantRequeueInterval, "tenant-requeue-interval", 5*time.Minute, "How often the Tenant reconciler re-aggregates owned Platforms.")
	flag.StringVar(&environment, "environment", os.Getenv("AGENTS_ENVIRONMENT"), "Environment name (dev/staging/production). Drives SSM-config path.")
	flag.StringVar(&region, "region", os.Getenv("AGENTS_REGION"), "AWS region. Defaults to credential-chain region if empty.")
	flag.StringVar(&oidcProviderARN, "oidc-provider-arn", os.Getenv("AGENTS_OIDC_PROVIDER_ARN"), "EKS cluster OIDC provider ARN; used in tenant IRSA trust policies.")
	flag.StringVar(&oidcIssuerHost, "oidc-issuer-host", os.Getenv("AGENTS_OIDC_ISSUER_HOST"), "EKS OIDC issuer host (oidc.eks.<region>.amazonaws.com/id/<id>).")
	flag.BoolVar(&disableAWS, "disable-aws", false, "Skip AWS client init + SSM config load (k8s-side reconciliation only).")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// AWS client + SSM config bootstrap. If disable-aws is set (unit/dev
	// path) we skip both; the reconcilers see r.IAM == nil and short-circuit
	// the AWS-side steps.
	var awsClients *awsclients.Clients
	var opConfig *operatorconfig.Config
	if !disableAWS {
		ctx := context.Background()
		var awsErr error
		awsClients, awsErr = awsclients.New(ctx, region)
		if awsErr != nil {
			setupLog.Error(awsErr, "unable to build AWS clients")
			os.Exit(1)
		}
		opConfig, awsErr = operatorconfig.Load(ctx, awsClients.SSM, environment, region)
		if awsErr != nil {
			setupLog.Error(awsErr, "unable to load operator config from SSM", "environment", environment)
			os.Exit(1)
		}
		if missing := opConfig.Validate(); len(missing) > 0 {
			setupLog.Info("operator config has missing fields; reconcilers may degrade", "missing", missing)
		}
		setupLog.Info("AWS substrate loaded", "environment", environment, "region", region,
			"operatorRoleARN", opConfig.OperatorRoleARN, "tenantIAMPath", opConfig.TenantIAMPath)
	} else {
		setupLog.Info("--disable-aws set; running without AWS clients (k8s-side only)")
		opConfig = &operatorconfig.Config{Environment: environment, Region: region}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	platformReconciler := &controller.PlatformReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Concurrency: platformWorkers,
	}
	if awsClients != nil {
		platformReconciler.IAM = awsClients.IAM
		platformReconciler.KMS = awsClients.KMS
		platformReconciler.S3 = awsClients.S3
		platformReconciler.IAMCfg = controller.IAMConfig{
			TenantIAMPath:           opConfig.TenantIAMPath,
			TenantBaselinePolicyARN: opConfig.TenantBaselinePolicyARN,
			OIDCProviderARN:         oidcProviderARN,
			OIDCIssuerHost:          oidcIssuerHost,
			Environment:             environment,
		}
		platformReconciler.AWSCfg = controller.PlatformAWSConfig{
			// cmk-data ARN isn't in operatorconfig today; the operator reads
			// it from an env var. Future work: publish it to SSM alongside
			// the other agent-iam outputs so the env-var override becomes
			// the dev escape hatch rather than the default.
			DataKMSKeyARN:       os.Getenv("AGENTS_DATA_KMS_KEY_ARN"),
			ArtifactsBucketName: opConfig.ArtifactsBucketName,
			Environment:         environment,
		}
	}
	if err := platformReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register reconciler", "controller", "Platform")
		os.Exit(1)
	}
	gatewayReconciler := &controller.ModelGatewayReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Concurrency: gatewayWorkers,
	}
	if opConfig != nil {
		gatewayReconciler.GuardrailID = opConfig.BaselineGuardrailID
		gatewayReconciler.GuardrailVersion = opConfig.BaselineGuardrailVersion
	}
	if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register reconciler", "controller", "ModelGateway")
		os.Exit(1)
	}
	if err := (&controller.AgentFleetReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Concurrency: runtimeWorkers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register reconciler", "controller", "AgentFleet")
		os.Exit(1)
	}
	budgetReconciler := &controller.BudgetReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Concurrency:     budgetWorkers,
		RequeueInterval: budgetRequeueInterval,
	}
	if awsClients != nil {
		budgetReconciler.Athena = awsClients.Athena
		budgetReconciler.CloudWatch = awsClients.CloudWatch
		budgetReconciler.EventBridge = awsClients.EventBridge
		budgetReconciler.AthenaCfg = controller.AthenaConfig{
			Workgroup:     opConfig.AthenaWorkgroup,
			Database:      opConfig.AthenaDatabase,
			ResultsBucket: opConfig.AthenaResultsBucket,
			CURTableName:  opConfig.CURTableName,
		}
		budgetReconciler.KillSwitchEventBusName = opConfig.KillSwitchEventBusName
	}
	if err := budgetReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register reconciler", "controller", "Budget")
		os.Exit(1)
	}
	evalReconciler := &controller.EvalReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Concurrency: evalWorkers,
	}
	if opConfig != nil {
		evalReconciler.RunnerNamespace = opConfig.EvalRunnerNamespace
	}
	if err := evalReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register reconciler", "controller", "Eval")
		os.Exit(1)
	}
	if err := (&controller.TenantReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Concurrency:     tenantWorkers,
		RequeueInterval: tenantRequeueInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register reconciler", "controller", "Tenant")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", version())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// buildVersion is overridden at link time via -ldflags="-X main.buildVersion=...".
// Lowercase so revive doesn't flag it as exported-from-package-main (which
// would be unreachable anyway).
var buildVersion = "dev"

func version() string {
	return fmt.Sprintf("eks-agent-platform-operator/%s", buildVersion)
}
