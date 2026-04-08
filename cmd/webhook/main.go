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
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/webhook"
)

func main() {
	appID, err := strconv.ParseInt(mustEnv("APP_ID"), 10, 64)
	if err != nil {
		log.Fatalf("Invalid APP_ID: %v", err)
	}

	installID, err := strconv.ParseInt(mustEnv("APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		log.Fatalf("Invalid APP_INSTALLATION_ID: %v", err)
	}

	// Only support file-based private key loading (#5)
	keyPath := mustEnv("APP_PRIVATE_KEY_FILE")
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatalf("Failed to read private key file %s: %v", keyPath, err)
	}

	webhookSecret := []byte(mustEnv("WEBHOOK_SECRET"))
	doToken := mustEnv("DIGITALOCEAN_TOKEN")

	cloudInitPath := envOrDefault("CLOUD_INIT_PATH", "cloud-init/runner.yaml.tmpl")
	region := envOrDefault("DO_REGION", "nyc3")
	size := envOrDefault("DO_SIZE", "s-4vcpu-8gb")
	requiredLabel := envOrDefault("REQUIRED_LABEL", "self-hosted")
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")

	var sshFingerprints []string
	if fp := os.Getenv("DO_SSH_FINGERPRINTS"); fp != "" {
		sshFingerprints = strings.Split(fp, ",")
	}

	githubApp := &gh.App{
		AppID:          appID,
		InstallationID: installID,
		PrivateKey:     privateKey,
	}

	doClient, err := digitalocean.NewClient(digitalocean.Config{
		Token:           doToken,
		Region:          region,
		Size:            size,
		CloudInitPath:   cloudInitPath,
		SSHFingerprints: sshFingerprints,
	})
	if err != nil {
		log.Fatalf("Failed to create DO client: %v", err)
	}

	handler := webhook.NewHandler(webhook.Config{
		WebhookSecret: webhookSecret,
		GitHubApp:     githubApp,
		DOClient:      doClient,
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

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return v
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
