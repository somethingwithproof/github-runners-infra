package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	appID := mustConfig(requiredPositiveInt64("APP_ID"))
	installationID := mustConfig(requiredPositiveInt64("APP_INSTALLATION_ID"))
	privateKeyPath := mustConfig(requiredEnv("APP_PRIVATE_KEY_FILE"))
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("read GitHub App private key file: %w", err)
	}
	allowedRepositories := splitCSV(mustConfig(requiredEnv("ALLOWED_REPOSITORIES")))

	stateStore, err := state.OpenFileStore(
		envOrDefault("STATE_FILE", "/var/lib/github-runners/state.json"),
		state.WithDeletedRetention(mustConfig(envDuration("DELETED_RECORD_RETENTION", 24*time.Hour))),
	)
	if err != nil {
		return fmt.Errorf("open lifecycle state: %w", err)
	}
	defer func() {
		if err := stateStore.Close(); err != nil {
			log.Printf("WARN: close lifecycle state: %v", err)
		}
	}()

	computeClient, closeCompute, err := newComputeClient(processCtx)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer closeCompute()

	githubClient := &gh.App{
		AppID:               appID,
		InstallationID:      installationID,
		PrivateKey:          privateKey,
		AllowedRepositories: allowedRepositories,
	}
	if err := githubClient.ValidateTokenScope(processCtx); err != nil {
		return fmt.Errorf("validate GitHub token scope: %w", err)
	}
	handler, err := webhook.NewHandler(webhook.Config{
		WebhookSecret:       []byte(mustConfig(requiredEnv("WEBHOOK_SECRET"))),
		GitHubClient:        githubClient,
		ComputeClient:       computeClient,
		Store:               stateStore,
		RequiredLabel:       envOrDefault("REQUIRED_LABEL", "self-hosted"),
		AllowedLabels:       splitCSV(mustConfig(requiredEnv("ALLOWED_LABELS"))),
		AllowedRepositories: allowedRepositories,
		RunnerVersion:       mustConfig(requiredEnv("RUNNER_VERSION")),
		RunnerSHA256:        mustConfig(requiredEnv("RUNNER_SHA256")),
		ChefInstallerSHA256: mustConfig(requiredEnv("CHEF_INSTALLER_SHA256")),
		RunnerGroupID:       mustConfig(envPositiveInt64("RUNNER_GROUP_ID", 1)),
		WorkerCount:         mustConfig(envPositiveInt("WORKER_COUNT", 4)),
		MaxLiveRunners:      mustConfig(envPositiveInt("MAX_LIVE_RUNNERS", 20)),
		MaxAttempts:         mustConfig(envPositiveInt("MAX_ATTEMPTS", 5)),
		MaxRunnerAge:        mustConfig(envDuration("MAX_RUNNER_AGE", 6*time.Hour)),
		CancelledRunnerTTL:  mustConfig(envDuration("CANCELLED_RUNNER_TTL", 5*time.Minute)),
		InstallationID:      installationID,
	})
	if err != nil {
		return fmt.Errorf("create webhook handler: %w", err)
	}

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	handler.Start(workerCtx)

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
		Addr:              envOrDefault("LISTEN_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Webhook listener starting on %s", srv.Addr)
		serverErr <- srv.ListenAndServe()
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
	handler.Wait()
	return listenerErr
}

func unexpectedListenerError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("HTTP server failed: %w", err)
}

func newComputeClient(ctx context.Context) (webhook.ComputeClient, func(), error) {
	provider := strings.ToLower(envOrDefault("COMPUTE_PROVIDER", "digitalocean"))
	controllerID, err := requiredEnv("CONTROLLER_ID")
	if err != nil {
		return nil, func() {}, err
	}
	cloudInitPath := envOrDefault("CLOUD_INIT_PATH", "/opt/github-runners/cloud-init/runner.yaml.tmpl")
	spot, err := envBoolValue("RUNNER_SPOT", false)
	if err != nil {
		return nil, func() {}, err
	}
	noClose := func() {}

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
		return client, noClose, err
	case "aws":
		required, err := requiredValues("AWS_REGION", "AWS_AMI_ID", "AWS_INSTANCE_TYPE", "AWS_SUBNET_ID", "AWS_SECURITY_GROUP_IDS")
		if err != nil {
			return nil, noClose, err
		}
		externalIP, err := envBoolValue("AWS_EXTERNAL_IP", false)
		if err != nil {
			return nil, noClose, err
		}
		client, err := aws.NewClient(ctx, aws.Config{
			Region: required["AWS_REGION"], AMI: required["AWS_AMI_ID"], InstanceType: required["AWS_INSTANCE_TYPE"],
			SubnetID: required["AWS_SUBNET_ID"], SecurityGroupIDs: splitCSV(required["AWS_SECURITY_GROUP_IDS"]),
			InstanceProfileARN: os.Getenv("AWS_INSTANCE_PROFILE_ARN"), KeyName: os.Getenv("AWS_KEY_NAME"),
			CloudInitPath: cloudInitPath, ControllerID: controllerID, Spot: spot, ExternalIP: externalIP,
		})
		return client, noClose, err
	case "gcp":
		required, err := requiredValues("GCP_PROJECT_ID", "GCP_ZONE", "GCP_MACHINE_TYPE", "GCP_SOURCE_IMAGE", "GCP_SUBNETWORK")
		if err != nil {
			return nil, noClose, err
		}
		externalIP, err := envBoolValue("GCP_EXTERNAL_IP", false)
		if err != nil {
			return nil, noClose, err
		}
		client, err := gcp.NewClient(ctx, gcp.Config{
			ProjectID: required["GCP_PROJECT_ID"], Zone: required["GCP_ZONE"], MachineType: required["GCP_MACHINE_TYPE"],
			SourceImage: required["GCP_SOURCE_IMAGE"], Subnetwork: required["GCP_SUBNETWORK"],
			CloudInitPath: cloudInitPath, ControllerID: controllerID, Spot: spot, ExternalIP: externalIP,
		})
		if err != nil {
			return nil, noClose, err
		}
		return client, func() {
			if err := client.Close(); err != nil {
				log.Printf("Close GCP Compute client: %v", err)
			}
		}, nil
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
		return client, noClose, err
	default:
		return nil, noClose, fmt.Errorf("unsupported COMPUTE_PROVIDER %q (expected digitalocean, aws, gcp, or azure)", provider)
	}
}

func mustConfig[T any](value T, err error) T {
	if err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}
	return value
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
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
