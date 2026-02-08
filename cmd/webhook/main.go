package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	callbackSecret := mustEnv("CALLBACK_SECRET")
	callbackSecretSSMPath := envOrDefault("CALLBACK_SECRET_SSM_PATH", "/github-runners/callback-secret")
	callbackURL := mustEnv("CALLBACK_URL")
	doToken := mustEnv("DIGITALOCEAN_TOKEN")

	cloudInitPath := envOrDefault("CLOUD_INIT_PATH", "cloud-init/runner.yaml.tmpl")
	region := envOrDefault("DO_REGION", "nyc3")
	size := envOrDefault("DO_SIZE", "s-4vcpu-8gb")
	requiredLabel := envOrDefault("REQUIRED_LABEL", "self-hosted")
	chefInstallerSHA256 := mustEnv("CHEF_INSTALLER_SHA256")
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

	handler, err := webhook.NewHandler(webhook.Config{
		WebhookSecret:         webhookSecret,
		GitHubApp:             githubApp,
		DOClient:              doClient,
		DOToken:               doToken,
		RequiredLabel:         requiredLabel,
		CallbackSecret:        callbackSecret,
		CallbackSecretSSMPath: callbackSecretSSMPath,
		CallbackURL:           callbackURL,
		ChefInstallerSHA256:   chefInstallerSHA256,
	})
	if err != nil {
		log.Fatalf("Failed to create webhook handler: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/callback/destroy", handler.HandleDestroy)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Server with timeouts (#4)
	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("Webhook listener starting on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
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
