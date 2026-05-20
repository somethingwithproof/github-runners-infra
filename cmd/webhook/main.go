package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/webhook"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	appID, err := strconv.ParseInt(mustEnv("APP_ID"), 10, 64)
	if err != nil {
		slog.Error("invalid APP_ID", "error", err)
		os.Exit(1)
	}

	installID, err := strconv.ParseInt(mustEnv("APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		slog.Error("invalid APP_INSTALLATION_ID", "error", err)
		os.Exit(1)
	}

	keyPath := mustEnv("APP_PRIVATE_KEY_FILE")
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		slog.Error("failed to read private key", "path", keyPath, "error", err)
		os.Exit(1)
	}

	webhookSecret := []byte(mustEnv("WEBHOOK_SECRET"))
	doToken := mustEnv("DIGITALOCEAN_TOKEN")

	cloudInitPath := envOrDefault("CLOUD_INIT_PATH", "cloud-init/runner.yaml.tmpl")
	region := envOrDefault("DO_REGION", "nyc3")
	size := envOrDefault("DO_SIZE", "s-4vcpu-8gb")
	requiredLabel := envOrDefault("REQUIRED_LABEL", "self-hosted")
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")
	callbackURL := os.Getenv("CALLBACK_URL")
	destroySecret := os.Getenv("DESTROY_SECRET")

	var sshFingerprints []string
	if fp := os.Getenv("DO_SSH_FINGERPRINTS"); fp != "" {
		sshFingerprints = strings.Split(fp, ",")
	}

	githubApp := &gh.App{
		AppID:          appID,
		InstallationID: installID,
		PrivateKey:     privateKey,
	}

	// Auto-detect runner version if not explicitly set
	runnerVersion := os.Getenv("RUNNER_VERSION")
	if runnerVersion == "" {
		if detected, err := fetchLatestRunnerVersion(); err != nil {
			slog.Warn("could not detect runner version, using default", "error", err)
			runnerVersion = "2.331.0"
		} else {
			runnerVersion = detected
			slog.Info("auto-detected runner version", "version", runnerVersion)
		}
	}

	var maxActiveDroplets int
	if v := os.Getenv("MAX_ACTIVE_DROPLETS"); v != "" {
		maxActiveDroplets, _ = strconv.Atoi(v)
	}

	doClient, err := digitalocean.NewClient(digitalocean.Config{
		Token:           doToken,
		Region:          region,
		Size:            size,
		CloudInitPath:   cloudInitPath,
		SSHFingerprints: sshFingerprints,
	})
	if err != nil {
		slog.Error("failed to create DO client", "error", err)
		os.Exit(1)
	}

	handler := webhook.NewHandler(webhook.Config{
		WebhookSecret:     webhookSecret,
		GitHubApp:         githubApp,
		DOClient:          doClient,
		DOToken:           doToken,
		RequiredLabel:     requiredLabel,
		RunnerVersion:     runnerVersion,
		CallbackURL:       callbackURL,
		DestroySecret:     destroySecret,
		MaxActiveDroplets: maxActiveDroplets,
	})

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/destroy", handler.HandleDestroy)
	mux.HandleFunc("/destroy/hmac", handler.HandleDestroyHMAC)
	mux.Handle("/metrics", promhttp.Handler())

	// Deep health check: verifies GitHub JWT + DO API connectivity
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if _, err := githubApp.GenerateJWT(); err != nil {
			http.Error(w, fmt.Sprintf("github: %v", err), http.StatusServiceUnavailable)
			return
		}
		if err := doClient.Ping(ctx); err != nil {
			http.Error(w, fmt.Sprintf("digitalocean: %v", err), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("webhook listener starting", "addr", listenAddr, "runner_version", runnerVersion)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-shutdownCh
	slog.Info("shutdown signal received, draining...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	done := make(chan struct{})
	go func() {
		handler.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("all provisioning goroutines completed")
	case <-ctx.Done():
		slog.Warn("timed out waiting for provisioning goroutines")
	}

	slog.Info("server stopped")
}

func fetchLatestRunnerVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return strings.TrimPrefix(release.TagName, "v"), nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
