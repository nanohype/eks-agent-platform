/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// NewTenantCmd wires `agentctl tenant <subcommand>`.
func NewTenantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Onboard + manage tenants",
	}
	cmd.AddCommand(newTenantInitCmd(), newTenantListCmd(), newTenantGetCmd())
	return cmd
}

func newTenantInitCmd() *cobra.Command {
	var (
		persona     string
		displayName string
		namespace   string
		schedule    string
		slack       string
	)
	cmd := &cobra.Command{
		Use:   "init NAME",
		Short: "Scaffold a tenant CR set (Tenant + Platform + Budget + Gateway + Fleet + Eval) with persona-flexed defaults",
		Long: `Emits a multi-document YAML on stdout. Pipe to kubectl to apply:

    agentctl tenant init acme --persona sales-ops | kubectl apply -f -

Persona-flexed defaults choose model routes, system prompts, budget,
and scaling bounds. List supported personas:

    agentctl persona list`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			res, err := ScaffoldTenant(ScaffoldOptions{
				TenantName:   args[0],
				DisplayName:  displayName,
				Persona:      persona,
				Namespace:    namespace,
				Schedule:     schedule,
				SlackChannel: slack,
			})
			if err != nil {
				return err
			}
			b, err := res.Render()
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&persona, "persona", "generic", "one of: sales-ops, support, finance, ops, founder, eng, marketing, legal, generic")
	cmd.Flags().StringVar(&displayName, "display-name", "", "human-readable name (defaults to NAME)")
	cmd.Flags().StringVar(&namespace, "namespace", "eks-agent-platform", "namespace for the Platform/Budget/Gateway/Fleet/Eval CRs")
	cmd.Flags().StringVar(&schedule, "schedule", "", "EvalSuite cron schedule (empty = manual only)")
	cmd.Flags().StringVar(&slack, "slack", "", "Slack channel for tenant notifications (e.g. #acme-ops)")
	return cmd
}

func newTenantListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all tenants in the connected cluster with roll-up state",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := newClusterClient()
			if err != nil {
				return err
			}
			ctx := context.Background()
			var list platformv1alpha1.TenantList
			if err := c.List(ctx, &list); err != nil {
				return fmt.Errorf("list tenants: %w", err)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tPERSONA\tPLATFORMS\tREADY\tSUSPENDED\tSPEND\tPCT")
			for i := range list.Items {
				t := &list.Items[i]
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%d\n",
					t.Name, t.Spec.PrimaryPersona,
					t.Status.PlatformCount, t.Status.ReadyPlatformCount, t.Status.SuspendedPlatformCount,
					t.Status.AggregateSpendUsd, t.Status.PercentOfBudget,
				)
			}
			return tw.Flush()
		},
	}
}

func newTenantGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get NAME",
		Short: "Show one tenant + its owned Platforms + per-platform spend",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := newClusterClient()
			if err != nil {
				return err
			}
			ctx := context.Background()
			name := args[0]

			var t platformv1alpha1.Tenant
			if err := c.Get(ctx, types.NamespacedName{Name: name}, &t); err != nil {
				return fmt.Errorf("get tenant %s: %w", name, err)
			}
			fmt.Printf("Tenant:       %s\n", t.Name)
			fmt.Printf("Display:      %s\n", t.Spec.DisplayName)
			fmt.Printf("Persona:      %s\n", t.Spec.PrimaryPersona)
			fmt.Printf("Phase:        %s\n", t.Status.Phase)
			fmt.Printf("Platforms:    %d (ready=%d suspended=%d)\n", t.Status.PlatformCount, t.Status.ReadyPlatformCount, t.Status.SuspendedPlatformCount)
			fmt.Printf("Budget:       %s usd (%d%% of cap %s)\n", t.Status.AggregateSpendUsd, t.Status.PercentOfBudget, t.Spec.AggregateMonthlyBudgetUsd)
			if t.Spec.Contact.SlackChannel != "" {
				fmt.Printf("Slack:        %s\n", t.Spec.Contact.SlackChannel)
			}

			var platforms platformv1alpha1.PlatformList
			if err := c.List(ctx, &platforms); err != nil {
				return fmt.Errorf("list platforms: %w", err)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "\nPLATFORM\tNAMESPACE\tPERSONA\tPHASE\tSUSPENDED-REASON")
			for i := range platforms.Items {
				p := &platforms.Items[i]
				if p.Spec.Tenant != name {
					continue
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Namespace, p.Spec.Persona, p.Status.Phase, p.Status.SuspendedReason)
			}
			return tw.Flush()
		},
	}
}

// NewStatusCmd is a one-call cluster-wide health summary.
func NewStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Cluster-wide one-liner: tenants + platforms + suspensions + aggregate spend",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := newClusterClient()
			if err != nil {
				return err
			}
			ctx := context.Background()
			var tenants platformv1alpha1.TenantList
			if err := c.List(ctx, &tenants); err != nil {
				return fmt.Errorf("list tenants: %w", err)
			}
			var platforms platformv1alpha1.PlatformList
			if err := c.List(ctx, &platforms); err != nil {
				return fmt.Errorf("list platforms: %w", err)
			}
			ready, suspended := 0, 0
			for i := range platforms.Items {
				switch platforms.Items[i].Status.Phase {
				case "Ready":
					ready++
				case "Suspended":
					suspended++
				}
			}
			fmt.Printf("%d tenants  |  %d platforms (%d ready, %d suspended)\n",
				len(tenants.Items), len(platforms.Items), ready, suspended)
			return nil
		},
	}
}

// NewPersonaCmd surfaces the catalog of persona-flexed defaults.
func NewPersonaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "persona",
		Short: "Explore the persona-flexed defaults the scaffolder uses",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List supported personas",
		RunE: func(_ *cobra.Command, _ []string) error {
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "PERSONA\tLABEL\tPRIMARY ROUTE\tSECONDARY\tBUDGET-USD")
			for _, p := range ListPersonas() {
				sec := p.SecondaryRouteName
				if sec == "" {
					sec = "-"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.DisplayLabel, p.PrimaryRouteName, sec, p.MonthlyBudgetUsd)
			}
			return tw.Flush()
		},
	})
	return cmd
}

// NewVersionCmd prints the CLI version (set at link time).
func NewVersionCmd(buildVersion string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print agentctl build version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("agentctl", buildVersion)
		},
	}
}

// newClusterClient builds a controller-runtime client from KUBECONFIG
// (or in-cluster config when running inside a pod). Surfaces a clear
// error when neither path is configured, rather than the cryptic
// in-cluster error controller-runtime defaults to.
func newClusterClient() (client.Client, error) {
	if !hasKubeconfigContext() {
		return nil, fmt.Errorf("no Kubernetes context available: set KUBECONFIG, log in with `aws eks update-kubeconfig`, or run inside a pod with a ServiceAccount")
	}
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	if err := governancev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// hasKubeconfigContext returns true when either KUBECONFIG is set, the
// default ~/.kube/config exists, or the process appears to be running
// inside a pod with a mounted ServiceAccount token. Cheap pre-check so
// CLI errors are actionable.
func hasKubeconfigContext() bool {
	if os.Getenv("KUBECONFIG") != "" {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(home + "/.kube/config"); err == nil {
			return true
		}
	}
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		return true
	}
	return false
}
