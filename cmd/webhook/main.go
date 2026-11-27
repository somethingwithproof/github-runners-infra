// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/aws"
	"github.com/thomasvincent/github-runners-infra/internal/azure"
	"github.com/thomasvincent/github-runners-infra/internal/budget"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	"github.com/thomasvincent/github-runners-infra/internal/gcp"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/i18n"
	"github.com/thomasvincent/github-runners-infra/internal/linode"
	"github.com/thomasvincent/github-runners-infra/internal/oci"
	"github.com/thomasvincent/github-runners-infra/internal/router"
	"github.com/thomasvincent/github-runners-infra/internal/state"
	"github.com/thomasvincent/github-runners-infra/internal/vultr"
	"github.com/thomasvincent/github-runners-infra/internal/webhook"
)

func main() {
	fs := flag.NewFlagSet("webhook", flag.ContinueOnError)
	lang := fs.String("lang", "", i18n.T("flag.lang"))
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	i18n.Init(*lang)

	if err := run(); err != nil {
		log.Printf("ERROR: %v", err)
		os.Exit(1)
	}
}

func run() error {
	processCtx, stopProcess := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopProcess()
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return i18n.Wrap(err, "webhook.err.invalid_config", err)
	}
	privateKey, err := os.ReadFile(cfg.privateKeyPath)
	if err != nil {
		return i18n.Wrap(err, "webhook.err.read_private_key", err)
	}
	stateStore, err := state.OpenFileStore(
		cfg.stateFile,
		state.WithDeletedRetention(cfg.deletedRetention),
	)
	if err != nil {
		return i18n.Wrap(err, "webhook.err.open_state", err)
	}
	defer func() {
		if err := stateStore.Close(); err != nil {
			log.Print(i18n.T("webhook.warn.close_state", err))
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(processCtx, 30*time.Second)
	computeClient, closeCompute, err := newComputeClient(startupCtx, processCtx)
	cancelStartup()
	if err != nil {
		return i18n.Wrap(err, "webhook.err.create_compute", err)
	}
	defer closeCompute()

	budgetTracker, err := budget.NewTracker(budget.Config{
		DailyCapUSD:     cfg.budgetDailyUSD,
		MonthlyCapUSD:   cfg.budgetMonthlyUSD,
		RepoDailyCapUSD: cfg.budgetRepoDailyUSD,
		StatePath:       cfg.budgetStateFile,
	})
	if err != nil {
		return i18n.Wrap(err, "webhook.err.invalid_config", err)
	}

	githubClient := &gh.App{
		AppID:               cfg.appID,
		InstallationID:      cfg.installationID,
		PrivateKey:          privateKey,
		AllowedRepositories: cfg.allowedRepositories,
		ReadWorkflowRuns:    true, // Current job demand always requires Actions read.
	}
	if err := validateGitHubTokenScope(processCtx, githubClient.ValidateTokenScope); err != nil {
		return i18n.Wrap(err, "webhook.err.validate_token", err)
	}
	handler, err := webhook.NewHandler(webhook.Config{
		WebhookSecret:             []byte(cfg.webhookSecret),
		GitHubClient:              githubClient,
		ComputeClient:             computeClient,
		BudgetTracker:             budgetTracker,
		HourlyRateOverrides:       cfg.hourlyRateOverrides,
		DefaultInstanceSize:       defaultInstanceSizeForEnv(computeClient.Provider()),
		DefaultSpot:               cfg.runnerSpot,
		Store:                     stateStore,
		RequiredLabel:             cfg.requiredLabel,
		AllowedLabels:             cfg.allowedLabels,
		LabelSizeMap:              cfg.labelSizeMap,
		AllowedRepositories:       cfg.allowedRepositories,
		AllowedPublicRepositories: cfg.allowedPublicRepositories,
		RunnerVersion:             cfg.runnerVersion,
		RunnerSHA256:              cfg.runnerSHA256,
		ChefInstallerSHA256:       cfg.chefInstallerSHA256,
		RunnerGroupID:             cfg.runnerGroupID,
		WorkerCount:               cfg.workerCount,
		MaxLiveRunners:            cfg.maxLiveRunners,
		MaxAttempts:               cfg.maxAttempts,
		MaxRunnerAge:              cfg.maxRunnerAge,
		CancelledRunnerTTL:        cfg.cancelledRunnerTTL,
		RegistrationTimeout:       cfg.registrationTimeout,
		LivenessSettleWindow:      cfg.livenessSettleWindow,
		LivenessConfirmations:     cfg.livenessConfirmations,
		DeadmanShutdownMinutes:    cfg.deadmanShutdownMinutes,
		DisableDeadmanShutdown:    cfg.disableDeadmanShutdown,
		DisableJITConfig:          !cfg.useJITConfig,
		PrebakedImage:             cfg.prebakedImage,
		EgressAllowlist:           cfg.egressAllowlist,
		InstallationID:            cfg.installationID,
	})
	if err != nil {
		return i18n.Wrap(err, "webhook.err.create_handler", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, i18n.T("webhook.http.method_not_allowed"), http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, i18n.T("webhook.http.method_not_allowed"), http.StatusMethodNotAllowed)
			return
		}
		writeBudgetMetrics(w, budgetTracker)
	})

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return serveUntilShutdown(processCtx, srv, handler.Start, handler.Wait, srv.ListenAndServe)
}

func serveUntilShutdown(
	processCtx context.Context,
	srv *http.Server,
	startWorkers func(context.Context),
	waitWorkers func(),
	listen func() error,
) error {
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	startWorkers(workerCtx)

	serverErr := make(chan error, 1)
	go func() {
		log.Print(i18n.T("webhook.log.starting", srv.Addr))
		serverErr <- listen()
	}()

	var listenerErr error
	select {
	case <-processCtx.Done():
		log.Print(i18n.T("webhook.log.shutdown"))
	case err := <-serverErr:
		listenerErr = unexpectedListenerError(err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Print(i18n.T("webhook.log.http_shutdown", err))
	}
	cancelWorkers()
	waitWorkers()
	return listenerErr
}

func validateGitHubTokenScope(ctx context.Context, validate func(context.Context) error) error {
	return validateGitHubTokenScopeWithPolicy(ctx, validate, tokenValidationRetryPolicy{
		minDelay: 5 * time.Second, jitter: time.Second, maxElapsed: 5 * time.Minute, maxAttempts: 5,
		now: time.Now, wait: waitForTokenRetry,
	})
}

type tokenValidationRetryPolicy struct {
	minDelay, jitter, maxElapsed time.Duration
	maxAttempts                  int
	now                          func() time.Time
	wait                         func(context.Context, time.Duration) error
}

func validateGitHubTokenScopeWithPolicy(
	ctx context.Context,
	validate func(context.Context) error,
	policy tokenValidationRetryPolicy,
) error {
	started := policy.now()
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := validate(ctx)
		if err == nil {
			return nil
		}
		resetAt, rateLimited := gh.RateLimitReset(err)
		if !rateLimited {
			return err
		}
		if attempt == policy.maxAttempts {
			return i18n.Wrap(err, "webhook.err.rate_limited_attempts", attempt, err)
		}
		delay := resetAt.Sub(policy.now())
		if delay < policy.minDelay {
			delay = policy.minDelay
		}
		if policy.jitter > 0 {
			delay += time.Duration(rand.Int64N(int64(policy.jitter)))
		}
		if policy.now().Add(delay).After(started.Add(policy.maxElapsed)) {
			return i18n.Wrap(err, "webhook.err.rate_limited_budget", policy.maxElapsed, err)
		}
		retryAt := policy.now().Add(delay)
		log.Print(i18n.T("webhook.warn.rate_limited_retry", retryAt.Format(time.RFC3339)))
		if err := policy.wait(ctx, delay); err != nil {
			return err
		}
	}
	return errors.New(i18n.T("webhook.err.invalid_retry_policy"))
}

func waitForTokenRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func unexpectedListenerError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return i18n.Wrap(err, "webhook.err.http_server_failed", err)
}

func newComputeClient(startupCtx, lifetimeCtx context.Context) (webhook.ComputeClient, func(), error) {
	controllerID, err := requiredEnv("CONTROLLER_ID")
	if err != nil {
		return nil, noopClose, err
	}
	cloudInitPath := envOrDefault("CLOUD_INIT_PATH", "/opt/github-runners/cloud-init/runner.yaml.tmpl")
	spot, err := envBoolValue("RUNNER_SPOT", false)
	if err != nil {
		return nil, noopClose, err
	}
	spotFallbackOnDemand, err := envBoolValue("SPOT_FALLBACK_ON_DEMAND", false)
	if err != nil {
		return nil, noopClose, err
	}

	if providers := splitCSV(os.Getenv("CLOUD_PROVIDERS")); len(providers) > 0 {
		return newMultiCloudRouter(startupCtx, lifetimeCtx, providers, controllerID, cloudInitPath, spot, spotFallbackOnDemand)
	}
	provider := strings.ToLower(envOrDefault("COMPUTE_PROVIDER", "digitalocean"))
	return newSingleComputeClient(startupCtx, lifetimeCtx, provider, controllerID, cloudInitPath, spot, spotFallbackOnDemand)
}

func newMultiCloudRouter(
	startupCtx, lifetimeCtx context.Context,
	providers []string,
	controllerID, cloudInitPath string,
	spot, spotFallbackOnDemand bool,
) (webhook.ComputeClient, func(), error) {
	strategy := router.Strategy(strings.ToLower(envOrDefault("ROUTER_STRATEGY", string(router.StrategyPriority))))
	overrides, err := router.ParseHourlyRateOverrides(os.Getenv("ROUTER_HOURLY_RATES"))
	if err != nil {
		return nil, noopClose, err
	}

	backends := make([]router.Backend, 0, len(providers))
	closers := make([]func(), 0, len(providers))
	closeAll := func() {
		for _, fn := range closers {
			fn()
		}
	}

	for _, rawProvider := range providers {
		name := strings.ToLower(strings.TrimSpace(rawProvider))
		backendSpot := spot
		if backendSpot && spotFallbackOnDemand && !router.ProviderSupportsSpot(name) {
			backendSpot = false
		}
		// Disable per-backend on-demand fallback so the router exhausts spot
		// capacity across all configured clouds before falling back to on-demand.
		client, closeFn, err := newSingleComputeClient(startupCtx, lifetimeCtx, name, controllerID, cloudInitPath, backendSpot, false)
		if err != nil {
			closeAll()
			return nil, noopClose, fmt.Errorf("initialize provider %q for CLOUD_PROVIDERS: %w", name, err)
		}
		closers = append(closers, closeFn)
		backends = append(backends, router.Backend{
			Name:        name,
			DefaultSize: defaultInstanceSizeForEnv(name),
			Provider:    client,
		})
	}

	multi, err := router.New(router.Config{
		Strategy:             strategy,
		Backends:             backends,
		HourlyRates:          overrides,
		DefaultSpot:          spot,
		SpotFallbackOnDemand: spotFallbackOnDemand,
	})
	if err != nil {
		closeAll()
		return nil, noopClose, err
	}
	return multi, closeAll, nil
}

func newSingleComputeClient(
	startupCtx, lifetimeCtx context.Context,
	provider, controllerID, cloudInitPath string,
	spot, spotFallbackOnDemand bool,
) (webhook.ComputeClient, func(), error) {
	noClose := noopClose

	switch provider {
	case "digitalocean":
		if spot {
			return nil, noClose, fmt.Errorf("%w: %s", compute.ErrSpotUnsupported, i18n.T("webhook.err.do_no_spot"))
		}
		required, err := requiredValues("DIGITALOCEAN_TOKEN", "DO_VPC_UUID", "DO_FIREWALL_ID")
		if err != nil {
			return nil, noClose, err
		}
		client, err := digitalocean.NewClient(digitalocean.Config{
			Token: required["DIGITALOCEAN_TOKEN"], Region: envOrDefault("DO_REGION", "nyc3"),
			Size: envOrDefault("DO_SIZE", "s-4vcpu-8gb-amd"), Image: envOrDefault("DO_IMAGE", "ubuntu-24-04-x64"),
			SSHFingerprints: splitCSV(os.Getenv("DO_SSH_FINGERPRINTS")), CloudInitPath: cloudInitPath, ControllerID: controllerID,
			VPCUUID: required["DO_VPC_UUID"], FirewallID: required["DO_FIREWALL_ID"],
			ProjectID: os.Getenv("DO_PROJECT_ID"),
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	case "aws":
		required, err := requiredValues("AWS_REGION", "AWS_AMI_ID", "AWS_INSTANCE_TYPE", "AWS_SUBNET_ID", "AWS_SECURITY_GROUP_IDS")
		if err != nil {
			return nil, noClose, err
		}
		externalIP, err := envBoolValue("AWS_EXTERNAL_IP", false)
		if err != nil {
			return nil, noClose, err
		}
		client, err := aws.NewClient(startupCtx, aws.Config{
			Region: required["AWS_REGION"], AMI: required["AWS_AMI_ID"], InstanceType: required["AWS_INSTANCE_TYPE"],
			SubnetID: required["AWS_SUBNET_ID"], AlternateSubnetIDs: splitCSV(os.Getenv("AWS_ALTERNATE_SUBNET_IDS")),
			SecurityGroupIDs:   splitCSV(required["AWS_SECURITY_GROUP_IDS"]),
			InstanceProfileARN: os.Getenv("AWS_INSTANCE_PROFILE_ARN"), KeyName: os.Getenv("AWS_KEY_NAME"),
			CloudInitPath: cloudInitPath, ControllerID: controllerID,
			Spot: spot, SpotFallbackOnDemand: spotFallbackOnDemand, ExternalIP: externalIP,
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	case "gcp":
		required, err := requiredValues(
			"GCP_PROJECT_ID", "GCP_ZONE", "GCP_MACHINE_TYPE", "GCP_SOURCE_IMAGE", "GCP_SUBNETWORK",
			"GCP_RUNNER_SERVICE_ACCOUNT_EMAIL",
		)
		if err != nil {
			return nil, noClose, err
		}
		externalIP, err := envBoolValue("GCP_EXTERNAL_IP", false)
		if err != nil {
			return nil, noClose, err
		}
		// The legacy Google auth transport retained by the pinned client uses
		// its constructor context for later token refreshes. Give it the process
		// lifetime instead of the short startup-validation deadline.
		client, err := gcp.NewClient(lifetimeCtx, gcp.Config{
			ProjectID: required["GCP_PROJECT_ID"], Zone: required["GCP_ZONE"], MachineType: required["GCP_MACHINE_TYPE"],
			SourceImage: required["GCP_SOURCE_IMAGE"], Subnetwork: required["GCP_SUBNETWORK"],
			ServiceAccountEmail: required["GCP_RUNNER_SERVICE_ACCOUNT_EMAIL"],
			CloudInitPath:       cloudInitPath, ControllerID: controllerID,
			Spot: spot, SpotFallbackOnDemand: spotFallbackOnDemand, ExternalIP: externalIP,
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, func() { closeGCPClient(client) }, nil
	case "azure":
		required, err := requiredValues(
			"AZURE_SSH_PUBLIC_KEY_FILE", "AZURE_SUBSCRIPTION_ID", "AZURE_RESOURCE_GROUP", "AZURE_LOCATION",
			"AZURE_VM_SIZE", "AZURE_IMAGE", "AZURE_SUBNET_ID",
		)
		if err != nil {
			return nil, noClose, err
		}
		sshPublicKey, err := os.ReadFile(required["AZURE_SSH_PUBLIC_KEY_FILE"])
		if err != nil {
			return nil, noClose, i18n.Wrap(err, "webhook.err.read_azure_key", err)
		}
		client, err := azure.NewClient(azure.Config{
			SubscriptionID: required["AZURE_SUBSCRIPTION_ID"], ResourceGroup: required["AZURE_RESOURCE_GROUP"],
			Location: required["AZURE_LOCATION"], VMSize: required["AZURE_VM_SIZE"], Image: required["AZURE_IMAGE"],
			SubnetID: required["AZURE_SUBNET_ID"], AdminUsername: envOrDefault("AZURE_ADMIN_USERNAME", "runneradmin"),
			SSHPublicKey: strings.TrimSpace(string(sshPublicKey)), CloudInitPath: cloudInitPath, ControllerID: controllerID,
			Spot: spot, SpotFallbackOnDemand: spotFallbackOnDemand,
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	case "vultr":
		if spot {
			return nil, noClose, fmt.Errorf("%w: %s", compute.ErrSpotUnsupported, i18n.T("webhook.err.provider_no_spot", "Vultr"))
		}
		apiKey, err := requiredEnv("VULTR_API_KEY")
		if err != nil {
			return nil, noClose, err
		}
		osID, err := optionalPositiveInt("VULTR_OS_ID")
		if err != nil {
			return nil, noClose, err
		}
		client, err := vultr.NewClient(vultr.Config{
			APIKey:          apiKey,
			Region:          envOrDefault("VULTR_REGION", "ewr"),
			Plan:            envOrDefault("VULTR_PLAN", "vc2-4c-8gb"),
			OSID:            osID,
			SnapshotID:      strings.TrimSpace(os.Getenv("VULTR_SNAPSHOT_ID")),
			SSHKeyIDs:       splitCSV(os.Getenv("VULTR_SSH_KEY_IDS")),
			FirewallGroupID: strings.TrimSpace(os.Getenv("VULTR_FIREWALL_GROUP_ID")),
			VPCID:           strings.TrimSpace(os.Getenv("VULTR_VPC_ID")),
			CloudInitPath:   cloudInitPath,
			ControllerID:    controllerID,
			BaseURL:         strings.TrimSpace(os.Getenv("VULTR_API_URL")),
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	case "linode":
		if spot {
			return nil, noClose, fmt.Errorf("%w: %s", compute.ErrSpotUnsupported, i18n.T("webhook.err.provider_no_spot", "Linode"))
		}
		token, err := requiredEnv("LINODE_TOKEN")
		if err != nil {
			return nil, noClose, err
		}
		firewallID, err := optionalPositiveInt("LINODE_FIREWALL_ID")
		if err != nil {
			return nil, noClose, err
		}
		vpcSubnetID, err := optionalPositiveInt("LINODE_VPC_SUBNET_ID")
		if err != nil {
			return nil, noClose, err
		}
		var authorizedKeys []string
		if keyPath := strings.TrimSpace(os.Getenv("LINODE_SSH_PUBLIC_KEY_FILE")); keyPath != "" {
			keyBytes, readErr := os.ReadFile(keyPath)
			if readErr != nil {
				return nil, noClose, fmt.Errorf("read Linode SSH public key: %w", readErr)
			}
			if trimmed := strings.TrimSpace(string(keyBytes)); trimmed != "" {
				authorizedKeys = []string{trimmed}
			}
		}
		client, err := linode.NewClient(linode.Config{
			Token:          token,
			Region:         envOrDefault("LINODE_REGION", "us-east"),
			InstanceType:   envOrDefault("LINODE_INSTANCE_TYPE", "g6-standard-4"),
			Image:          envOrDefault("LINODE_IMAGE", "linode/ubuntu24.04"),
			AuthorizedKeys: authorizedKeys,
			FirewallID:     firewallID,
			VPCSubnetID:    vpcSubnetID,
			CloudInitPath:  cloudInitPath,
			ControllerID:   controllerID,
			BaseURL:        strings.TrimSpace(os.Getenv("LINODE_API_URL")),
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	case "oci":
		client, err := newOCIComputeClient(controllerID, cloudInitPath, spot, spotFallbackOnDemand)
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	default:
		return nil, noClose, errors.New(i18n.T("webhook.err.unsupported_provider", provider))
	}
}

func newOCIComputeClient(controllerID, cloudInitPath string, spot, spotFallbackOnDemand bool) (*oci.Client, error) {
	required, err := requiredValues(
		"OCI_TENANCY_OCID", "OCI_USER_OCID", "OCI_FINGERPRINT", "OCI_REGION",
		"OCI_COMPARTMENT_OCID", "OCI_SUBNET_OCID", "OCI_AVAILABILITY_DOMAIN", "OCI_IMAGE_OCID",
	)
	if err != nil {
		return nil, err
	}
	ocpus, err := envNonNegativeFloat("OCI_OCPUS")
	if err != nil {
		return nil, err
	}
	memoryGB, err := envNonNegativeFloat("OCI_MEMORY_GB")
	if err != nil {
		return nil, err
	}
	assignPublicIP, err := envBoolValue("OCI_ASSIGN_PUBLIC_IP", false)
	if err != nil {
		return nil, err
	}
	sshPublicKey := ""
	if keyPath := strings.TrimSpace(os.Getenv("OCI_SSH_PUBLIC_KEY_FILE")); keyPath != "" {
		keyBytes, readErr := os.ReadFile(keyPath)
		if readErr != nil {
			return nil, fmt.Errorf("read OCI SSH public key: %w", readErr)
		}
		sshPublicKey = strings.TrimSpace(string(keyBytes))
	}
	return oci.NewClient(oci.Config{
		TenancyOCID:          required["OCI_TENANCY_OCID"],
		UserOCID:             required["OCI_USER_OCID"],
		Fingerprint:          required["OCI_FINGERPRINT"],
		PrivateKeyPath:       strings.TrimSpace(os.Getenv("OCI_PRIVATE_KEY_PATH")),
		PrivateKeyPEM:        os.Getenv("OCI_PRIVATE_KEY_PEM"),
		Region:               required["OCI_REGION"],
		CompartmentOCID:      required["OCI_COMPARTMENT_OCID"],
		SubnetOCID:           required["OCI_SUBNET_OCID"],
		AvailabilityDomain:   required["OCI_AVAILABILITY_DOMAIN"],
		ImageOCID:            required["OCI_IMAGE_OCID"],
		Shape:                envOrDefault("OCI_SHAPE", "VM.Standard.E4.Flex"),
		OCPUs:                ocpus,
		MemoryGB:             memoryGB,
		SSHPublicKey:         sshPublicKey,
		AssignPublicIP:       assignPublicIP,
		Spot:                 spot,
		SpotFallbackOnDemand: spotFallbackOnDemand,
		CloudInitPath:        cloudInitPath,
		ControllerID:         controllerID,
		BaseURL:              strings.TrimSpace(os.Getenv("OCI_API_URL")),
	})
}

func noopClose() {
	// Provider clients without an explicit Close method require no cleanup.
}

func closeGCPClient(client interface{ Close() error }) {
	if err := client.Close(); err != nil {
		log.Print(i18n.T("webhook.err.close_gcp", err))
	}
}

type runtimeConfig struct {
	appID, installationID              int64
	privateKeyPath, stateFile          string
	requiredLabel, listenAddr          string
	webhookSecret, runnerVersion       string
	runnerSHA256, chefInstallerSHA256  string
	allowedLabels, allowedRepositories []string
	allowedPublicRepositories          []string
	labelSizeMap                       map[string]string
	hourlyRateOverrides                map[string]float64
	budgetDailyUSD                     float64
	budgetMonthlyUSD                   float64
	budgetRepoDailyUSD                 float64
	budgetStateFile                    string
	useJITConfig                       bool
	prebakedImage                      bool
	runnerSpot                         bool
	egressAllowlist                    []string
	runnerGroupID                      int64
	workerCount, maxLiveRunners        int
	maxAttempts                        int
	maxRunnerAge, cancelledRunnerTTL   time.Duration
	registrationTimeout                time.Duration
	livenessSettleWindow               time.Duration
	livenessConfirmations              int
	deadmanShutdownMinutes             int
	disableDeadmanShutdown             bool
	deletedRetention                   time.Duration
}

func loadRuntimeConfig() (runtimeConfig, error) {
	var cfg runtimeConfig
	var err error
	if cfg.appID, err = requiredPositiveInt64("APP_ID"); err != nil {
		return cfg, err
	}
	if cfg.installationID, err = requiredPositiveInt64("APP_INSTALLATION_ID"); err != nil {
		return cfg, err
	}
	if cfg.privateKeyPath, err = requiredEnv("APP_PRIVATE_KEY_FILE"); err != nil {
		return cfg, err
	}
	allowedRepositories, err := requiredEnv("ALLOWED_REPOSITORIES")
	if err != nil {
		return cfg, err
	}
	cfg.allowedRepositories = splitCSV(allowedRepositories)
	cfg.allowedPublicRepositories = splitCSV(os.Getenv("ALLOWED_PUBLIC_REPOSITORIES"))
	if len(cfg.allowedRepositories) == 0 {
		return cfg, errors.New(i18n.T("webhook.err.env_non_empty", "ALLOWED_REPOSITORIES"))
	}
	allowedLabels, err := requiredEnv("ALLOWED_LABELS")
	if err != nil {
		return cfg, err
	}
	cfg.allowedLabels = splitCSV(allowedLabels)
	if len(cfg.allowedLabels) == 0 {
		return cfg, errors.New(i18n.T("webhook.err.env_non_empty", "ALLOWED_LABELS"))
	}
	if cfg.webhookSecret, err = requiredRawEnv("WEBHOOK_SECRET"); err != nil {
		return cfg, err
	}
	if cfg.runnerVersion, err = requiredEnv("RUNNER_VERSION"); err != nil {
		return cfg, err
	}
	if cfg.runnerSHA256, err = requiredEnv("RUNNER_SHA256"); err != nil {
		return cfg, err
	}
	if cfg.chefInstallerSHA256, err = requiredEnv("CHEF_INSTALLER_SHA256"); err != nil {
		return cfg, err
	}
	if cfg.runnerGroupID, err = envPositiveInt64("RUNNER_GROUP_ID", 1); err != nil {
		return cfg, err
	}
	if cfg.workerCount, err = envPositiveInt("WORKER_COUNT", 4); err != nil {
		return cfg, err
	}
	if cfg.maxLiveRunners, err = envPositiveInt("MAX_LIVE_RUNNERS", 20); err != nil {
		return cfg, err
	}
	if cfg.maxAttempts, err = envPositiveInt("MAX_ATTEMPTS", 5); err != nil {
		return cfg, err
	}
	if cfg.maxRunnerAge, err = envDuration("MAX_RUNNER_AGE", 6*time.Hour); err != nil {
		return cfg, err
	}
	if cfg.cancelledRunnerTTL, err = envDuration("CANCELLED_RUNNER_TTL", 5*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.registrationTimeout, err = envDuration("RUNNER_REGISTRATION_TIMEOUT", 10*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.livenessSettleWindow, err = envDuration("LIVENESS_SETTLE_WINDOW", 2*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.livenessConfirmations, err = envPositiveInt("LIVENESS_CONFIRMATIONS", 3); err != nil {
		return cfg, err
	}
	if cfg.deletedRetention, err = envDuration("DELETED_RECORD_RETENTION", 24*time.Hour); err != nil {
		return cfg, err
	}
	if cfg.labelSizeMap, err = loadLabelSizeMap(); err != nil {
		return cfg, err
	}
	if cfg.deadmanShutdownMinutes, cfg.disableDeadmanShutdown, err = envDeadmanMinutes("RUNNER_DEADMAN_MINUTES", compute.DefaultDeadmanShutdownMinutes); err != nil {
		return cfg, err
	}
	if err = loadRoutingBudgetAndBootstrapConfig(&cfg); err != nil {
		return cfg, err
	}
	cfg.stateFile = envOrDefault("STATE_FILE", "/var/lib/github-runners/state.json")
	cfg.requiredLabel = envOrDefault("REQUIRED_LABEL", "self-hosted")
	cfg.listenAddr = envOrDefault("LISTEN_ADDR", ":8080")
	return cfg, nil
}

func loadRoutingBudgetAndBootstrapConfig(cfg *runtimeConfig) error {
	var err error
	if cfg.hourlyRateOverrides, err = router.ParseHourlyRateOverrides(os.Getenv("ROUTER_HOURLY_RATES")); err != nil {
		return err
	}
	if cfg.budgetDailyUSD, err = envNonNegativeFloat("BUDGET_DAILY_USD"); err != nil {
		return err
	}
	if cfg.budgetMonthlyUSD, err = envNonNegativeFloat("BUDGET_MONTHLY_USD"); err != nil {
		return err
	}
	if cfg.budgetRepoDailyUSD, err = envNonNegativeFloat("BUDGET_REPO_DAILY_USD"); err != nil {
		return err
	}
	cfg.budgetStateFile = strings.TrimSpace(os.Getenv("BUDGET_STATE_FILE"))
	if cfg.useJITConfig, err = envBoolValue("GITHUB_USE_JIT_CONFIG", true); err != nil {
		return err
	}
	if cfg.prebakedImage, err = envBoolValue("RUNNER_PREBAKED", false); err != nil {
		return err
	}
	if cfg.runnerSpot, err = envBoolValue("RUNNER_SPOT", false); err != nil {
		return err
	}
	if cfg.egressAllowlist, err = compute.ParseEgressAllowlist(os.Getenv("RUNNER_EGRESS_ALLOWLIST")); err != nil {
		return err
	}
	return nil
}

func envNonNegativeFloat(key string) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number", key)
	}
	return parsed, nil
}

func defaultInstanceSizeForEnv(provider string) string {
	switch provider {
	case "digitalocean":
		return envOrDefault("DO_SIZE", "s-4vcpu-8gb-amd")
	case "aws":
		return strings.TrimSpace(os.Getenv("AWS_INSTANCE_TYPE"))
	case "gcp":
		return strings.TrimSpace(os.Getenv("GCP_MACHINE_TYPE"))
	case "azure":
		return strings.TrimSpace(os.Getenv("AZURE_VM_SIZE"))
	case "vultr":
		return envOrDefault("VULTR_PLAN", "vc2-4c-8gb")
	case "linode":
		return envOrDefault("LINODE_INSTANCE_TYPE", "g6-standard-4")
	case "oci":
		shape := envOrDefault("OCI_SHAPE", "VM.Standard.E4.Flex")
		if strings.Contains(shape, ":") || !strings.HasSuffix(strings.ToLower(shape), ".flex") {
			return shape
		}
		ocpus := envOrDefault("OCI_OCPUS", "4")
		memGB := envOrDefault("OCI_MEMORY_GB", "16")
		return shape + ":" + ocpus + ":" + memGB
	default:
		return ""
	}
}

func writeBudgetMetrics(w http.ResponseWriter, tracker *budget.Tracker) {
	snap := tracker.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w,
		"# TYPE budget_blocked_total counter\nbudget_blocked_total %d\n# TYPE estimated_spend_usd gauge\nestimated_spend_usd{window=\"daily\"} %.6f\nestimated_spend_usd{window=\"monthly\"} %.6f\nestimated_spend_usd{window=\"in_flight\"} %.6f\n",
		snap.BudgetBlockedTotal, snap.DailySpendUSD, snap.MonthlySpendUSD, snap.InFlightSpendUSD,
	)
}

func loadLabelSizeMap() (map[string]string, error) {
	candidates := []struct {
		name  string
		value string
	}{
		{"LABEL_SIZE_MAP", strings.TrimSpace(os.Getenv("LABEL_SIZE_MAP"))},
		{"DO_LABEL_SIZE_MAP", strings.TrimSpace(os.Getenv("DO_LABEL_SIZE_MAP"))},
		{"OCI_LABEL_SIZE_MAP", strings.TrimSpace(os.Getenv("OCI_LABEL_SIZE_MAP"))},
	}
	var raw string
	var setNames []string
	for _, c := range candidates {
		if c.value != "" {
			raw = c.value
			setNames = append(setNames, c.name)
		}
	}
	if len(setNames) > 1 {
		return nil, fmt.Errorf("set only one of %s, not multiple", strings.Join(setNames, ", "))
	}
	return compute.ParseLabelSizeMap(raw)
}

func envDeadmanMinutes(key string, fallback int) (int, bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, false, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 || parsed > compute.MaxDeadmanShutdownMinutes {
		return 0, false, fmt.Errorf("%s must be an integer between 0 and %d", key, compute.MaxDeadmanShutdownMinutes)
	}
	if parsed == 0 {
		return 0, true, nil
	}
	return parsed, false, nil
}

func optionalPositiveInt(key string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0, errors.New(i18n.T("webhook.err.env_positive_int", key))
	}
	return parsed, nil
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", errors.New(i18n.T("webhook.err.env_missing", key))
	}
	return value, nil
}

func requiredRawEnv(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", errors.New(i18n.T("webhook.err.env_missing", key))
	}
	return value, nil
}

func requiredValues(keys ...string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := requiredEnv(key)
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func requiredPositiveInt64(key string) (int64, error) {
	value, err := requiredEnv(key)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New(i18n.T("webhook.err.env_positive_int", key))
	}
	return parsed, nil
}

func envPositiveInt64(key string, fallback int64) (int64, error) {
	value := envOrDefault(key, strconv.FormatInt(fallback, 10))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New(i18n.T("webhook.err.env_positive_int", key))
	}
	return parsed, nil
}

func envPositiveInt(key string, fallback int) (int, error) {
	value := envOrDefault(key, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, errors.New(i18n.T("webhook.err.env_positive_int", key))
	}
	return parsed, nil
}

func envBoolValue(key string, fallback bool) (bool, error) {
	value := envOrDefault(key, strconv.FormatBool(fallback))
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, errors.New(i18n.T("webhook.err.env_bool", key))
	}
	return parsed, nil
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := envOrDefault(key, fallback.String())
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, errors.New(i18n.T("webhook.err.env_duration", key))
	}
	return parsed, nil
}
