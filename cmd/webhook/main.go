package main

import (
	"context"
	"errors"
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
	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	"github.com/thomasvincent/github-runners-infra/internal/gcp"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
	"github.com/thomasvincent/github-runners-infra/internal/webhook"
)

func main() {
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
		return fmt.Errorf("invalid configuration: %w", err)
	}
	privateKey, err := os.ReadFile(cfg.privateKeyPath)
	if err != nil {
		return fmt.Errorf("read GitHub App private key file: %w", err)
	}
	stateStore, err := state.OpenFileStore(
		cfg.stateFile,
		state.WithDeletedRetention(cfg.deletedRetention),
	)
	if err != nil {
		return fmt.Errorf("open lifecycle state: %w", err)
	}
	defer func() {
		if err := stateStore.Close(); err != nil {
			log.Printf("WARN: close lifecycle state: %v", err)
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(processCtx, 30*time.Second)
	computeClient, closeCompute, err := newComputeClient(startupCtx, processCtx)
	cancelStartup()
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer closeCompute()

	githubClient := &gh.App{
		AppID:               cfg.appID,
		InstallationID:      cfg.installationID,
		PrivateKey:          privateKey,
		AllowedRepositories: cfg.allowedRepositories,
	}
	if err := validateGitHubTokenScope(processCtx, githubClient.ValidateTokenScope); err != nil {
		return fmt.Errorf("validate GitHub token scope: %w", err)
	}
	handler, err := webhook.NewHandler(webhook.Config{
		WebhookSecret:         []byte(cfg.webhookSecret),
		GitHubClient:          githubClient,
		ComputeClient:         computeClient,
		Store:                 stateStore,
		RequiredLabel:         cfg.requiredLabel,
		AllowedLabels:         cfg.allowedLabels,
		AllowedRepositories:   cfg.allowedRepositories,
		RunnerVersion:         cfg.runnerVersion,
		RunnerSHA256:          cfg.runnerSHA256,
		ChefInstallerSHA256:   cfg.chefInstallerSHA256,
		RunnerGroupID:         cfg.runnerGroupID,
		WorkerCount:           cfg.workerCount,
		MaxLiveRunners:        cfg.maxLiveRunners,
		MaxAttempts:           cfg.maxAttempts,
		MaxRunnerAge:          cfg.maxRunnerAge,
		CancelledRunnerTTL:    cfg.cancelledRunnerTTL,
		RegistrationTimeout:   cfg.registrationTimeout,
		LivenessSettleWindow:  cfg.livenessSettleWindow,
		LivenessConfirmations: cfg.livenessConfirmations,
		InstallationID:        cfg.installationID,
	})
	if err != nil {
		return fmt.Errorf("create webhook handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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
		log.Printf("Webhook listener starting on %s", srv.Addr)
		serverErr <- listen()
	}()

	var listenerErr error
	select {
	case <-processCtx.Done():
		log.Printf("Received shutdown signal; stopping ingress and workers")
	case err := <-serverErr:
		listenerErr = unexpectedListenerError(err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
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
			return fmt.Errorf("GitHub token-scope validation remained rate limited after %d attempts: %w", attempt, err)
		}
		delay := resetAt.Sub(policy.now())
		if delay < policy.minDelay {
			delay = policy.minDelay
		}
		if policy.jitter > 0 {
			delay += time.Duration(rand.Int64N(int64(policy.jitter)))
		}
		if policy.now().Add(delay).After(started.Add(policy.maxElapsed)) {
			return fmt.Errorf("GitHub token-scope validation throttle exceeds %s startup budget: %w", policy.maxElapsed, err)
		}
		retryAt := policy.now().Add(delay)
		log.Printf("WARN: GitHub token-scope validation rate limited; retrying at %s", retryAt.Format(time.RFC3339))
		if err := policy.wait(ctx, delay); err != nil {
			return err
		}
	}
	return errors.New("GitHub token-scope validation retry policy is invalid")
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
	return fmt.Errorf("HTTP server failed: %w", err)
}

func newComputeClient(startupCtx, lifetimeCtx context.Context) (webhook.ComputeClient, func(), error) {
	provider := strings.ToLower(envOrDefault("COMPUTE_PROVIDER", "digitalocean"))
	controllerID, err := requiredEnv("CONTROLLER_ID")
	if err != nil {
		return nil, noopClose, err
	}
	cloudInitPath := envOrDefault("CLOUD_INIT_PATH", "/opt/github-runners/cloud-init/runner.yaml.tmpl")
	spot, err := envBoolValue("RUNNER_SPOT", false)
	if err != nil {
		return nil, noopClose, err
	}
	noClose := noopClose

	switch provider {
	case "digitalocean":
		if spot {
			return nil, noClose, fmt.Errorf("DigitalOcean does not expose a Spot VM equivalent; unset RUNNER_SPOT")
		}
		required, err := requiredValues("DIGITALOCEAN_TOKEN", "DO_VPC_UUID", "DO_FIREWALL_ID")
		if err != nil {
			return nil, noClose, err
		}
		client, err := digitalocean.NewClient(digitalocean.Config{
			Token: required["DIGITALOCEAN_TOKEN"], Region: envOrDefault("DO_REGION", "nyc3"),
			Size: envOrDefault("DO_SIZE", "s-4vcpu-8gb"), Image: envOrDefault("DO_IMAGE", "ubuntu-24-04-x64"),
			SSHFingerprints: splitCSV(os.Getenv("DO_SSH_FINGERPRINTS")), CloudInitPath: cloudInitPath, ControllerID: controllerID,
			VPCUUID: required["DO_VPC_UUID"], FirewallID: required["DO_FIREWALL_ID"],
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
			SubnetID: required["AWS_SUBNET_ID"], SecurityGroupIDs: splitCSV(required["AWS_SECURITY_GROUP_IDS"]),
			InstanceProfileARN: os.Getenv("AWS_INSTANCE_PROFILE_ARN"), KeyName: os.Getenv("AWS_KEY_NAME"),
			CloudInitPath: cloudInitPath, ControllerID: controllerID, Spot: spot, ExternalIP: externalIP,
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
			CloudInitPath:       cloudInitPath, ControllerID: controllerID, Spot: spot, ExternalIP: externalIP,
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
			return nil, noClose, fmt.Errorf("read Azure SSH public key: %w", err)
		}
		client, err := azure.NewClient(azure.Config{
			SubscriptionID: required["AZURE_SUBSCRIPTION_ID"], ResourceGroup: required["AZURE_RESOURCE_GROUP"],
			Location: required["AZURE_LOCATION"], VMSize: required["AZURE_VM_SIZE"], Image: required["AZURE_IMAGE"],
			SubnetID: required["AZURE_SUBNET_ID"], AdminUsername: envOrDefault("AZURE_ADMIN_USERNAME", "runneradmin"),
			SSHPublicKey: strings.TrimSpace(string(sshPublicKey)), CloudInitPath: cloudInitPath, ControllerID: controllerID,
			Spot: spot,
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, noClose, nil
	default:
		return nil, noClose, fmt.Errorf("unsupported COMPUTE_PROVIDER %q (expected digitalocean, aws, gcp, or azure)", provider)
	}
}

func noopClose() {
	// Provider clients without an explicit Close method require no cleanup.
}

func closeGCPClient(client interface{ Close() error }) {
	if err := client.Close(); err != nil {
		log.Printf("ERROR: close GCP Compute client: %v", err)
	}
}

type runtimeConfig struct {
	appID, installationID              int64
	privateKeyPath, stateFile          string
	requiredLabel, listenAddr          string
	webhookSecret, runnerVersion       string
	runnerSHA256, chefInstallerSHA256  string
	allowedLabels, allowedRepositories []string
	runnerGroupID                      int64
	workerCount, maxLiveRunners        int
	maxAttempts                        int
	maxRunnerAge, cancelledRunnerTTL   time.Duration
	registrationTimeout                time.Duration
	livenessSettleWindow               time.Duration
	livenessConfirmations              int
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
	if len(cfg.allowedRepositories) == 0 {
		return cfg, fmt.Errorf("ALLOWED_REPOSITORIES must contain at least one non-empty value")
	}
	allowedLabels, err := requiredEnv("ALLOWED_LABELS")
	if err != nil {
		return cfg, err
	}
	cfg.allowedLabels = splitCSV(allowedLabels)
	if len(cfg.allowedLabels) == 0 {
		return cfg, fmt.Errorf("ALLOWED_LABELS must contain at least one non-empty value")
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
	cfg.stateFile = envOrDefault("STATE_FILE", "/var/lib/github-runners/state.json")
	cfg.requiredLabel = envOrDefault("REQUIRED_LABEL", "self-hosted")
	cfg.listenAddr = envOrDefault("LISTEN_ADDR", ":8080")
	return cfg, nil
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", fmt.Errorf("required environment variable %s is not set", key)
	}
	return value, nil
}

func requiredRawEnv(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", fmt.Errorf("required environment variable %s is not set", key)
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
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func envPositiveInt64(key string, fallback int64) (int64, error) {
	value := envOrDefault(key, strconv.FormatInt(fallback, 10))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func envPositiveInt(key string, fallback int) (int, error) {
	value := envOrDefault(key, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func envBoolValue(key string, fallback bool) (bool, error) {
	value := envOrDefault(key, strconv.FormatBool(fallback))
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
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
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}
