package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	"github.com/thomasvincent/github-runners-infra/internal/envutil"
	"github.com/thomasvincent/github-runners-infra/internal/gcp"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/provider"
	"github.com/thomasvincent/github-runners-infra/internal/webhook"
)

func main() {
	appID, err := strconv.ParseInt(envutil.MustEnv("APP_ID"), 10, 64)
	if err != nil {
		log.Fatalf("Invalid APP_ID: %v", err)
	}

	installID, err := strconv.ParseInt(envutil.MustEnv("APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		log.Fatalf("Invalid APP_INSTALLATION_ID: %v", err)
	}

	// Only support file-based private key loading (#5)
	keyPath := envutil.MustEnv("APP_PRIVATE_KEY_FILE")
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatalf("Failed to read private key file %s: %v", keyPath, err)
	}

	webhookSecret := []byte(envutil.MustEnv("WEBHOOK_SECRET"))
	requiredLabel := envutil.OrDefault("REQUIRED_LABEL", "self-hosted")
	listenAddr := envutil.OrDefault("LISTEN_ADDR", ":8080")

	githubApp := &gh.App{
		AppID:          appID,
		InstallationID: installID,
		PrivateKey:     privateKey,
	}

	prov, doToken, err := buildProvisioner(context.Background())
	if err != nil {
		log.Fatalf("Failed to create runner provisioner: %v", err)
	}

	handler := webhook.NewHandler(webhook.Config{
		WebhookSecret: webhookSecret,
		GitHubApp:     githubApp,
		Provisioner:   prov,
		DOToken:       doToken,
		RequiredLabel: requiredLabel,
	})

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Server with timeouts (#4)
	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown: finish in-flight provisioning on SIGTERM/SIGINT
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("Webhook listener starting on %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-shutdownCh
	log.Printf("Shutdown signal received, draining in-flight requests...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
	log.Printf("Server stopped")
}

// buildProvisioner selects the runner backend from RUNNER_PROVIDER (default
// "digitalocean"). It returns the DO token separately because the DO startup
// path needs it for self-deletion; GCP uses the instance service account, so
// the token is empty there.
func buildProvisioner(ctx context.Context) (provider.Provisioner, string, error) {
	switch strings.ToLower(envutil.OrDefault("RUNNER_PROVIDER", "digitalocean")) {
	case "gcp":
		startupPath := envutil.OrDefault("GCP_STARTUP_SCRIPT", "cloud-init/runner-gcp.sh.tmpl")
		client, err := gcp.NewClient(ctx, gcp.Config{
			Project:           envutil.MustEnv("GCP_PROJECT"),
			Zone:              envutil.MustEnv("GCP_ZONE"),
			MachineType:       envutil.OrDefault("GCP_MACHINE_TYPE", "e2-custom-4-8192"),
			RunnerSA:          os.Getenv("GCP_RUNNER_SA"),
			Network:           os.Getenv("GCP_NETWORK"),
			Subnet:            os.Getenv("GCP_SUBNET"),
			SourceImage:       os.Getenv("GCP_IMAGE"),
			StartupScriptPath: startupPath,
		})
		if err != nil {
			return nil, "", err
		}
		return client, "", nil

	default:
		doToken := envutil.MustEnv("DIGITALOCEAN_TOKEN")
		cloudInitPath := envutil.OrDefault("CLOUD_INIT_PATH", "cloud-init/runner.yaml.tmpl")
		var sshFingerprints []string
		if fp := os.Getenv("DO_SSH_FINGERPRINTS"); fp != "" {
			sshFingerprints = strings.Split(fp, ",")
		}
		client, err := digitalocean.NewClient(digitalocean.Config{
			Token:           doToken,
			Region:          envutil.OrDefault("DO_REGION", "nyc3"),
			Size:            envutil.OrDefault("DO_SIZE", "s-4vcpu-8gb"),
			CloudInitPath:   cloudInitPath,
			SSHFingerprints: sshFingerprints,
		})
		if err != nil {
			return nil, "", err
		}
		return client, doToken, nil
	}
}
