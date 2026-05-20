package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	doToken := os.Getenv("DIGITALOCEAN_TOKEN")
	if doToken == "" {
		slog.Error("DIGITALOCEAN_TOKEN is required")
		os.Exit(1)
	}

	client, err := digitalocean.NewClient(digitalocean.Config{
		Token:         doToken,
		CloudInitPath: "cloud-init/runner.yaml.tmpl",
	})
	if err != nil {
		slog.Error("failed to create DO client", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	maxAge := 60 * time.Minute
	deleted, err := client.CleanupOldDroplets(ctx, maxAge)
	if err != nil {
		slog.Error("cleanup failed", "error", err)
		os.Exit(1)
	}
	slog.Info("droplet cleanup complete", "deleted", deleted)

	appIDStr := os.Getenv("APP_ID")
	installIDStr := os.Getenv("APP_INSTALLATION_ID")
	keyPath := os.Getenv("APP_PRIVATE_KEY_FILE")
	if appIDStr == "" || installIDStr == "" || keyPath == "" {
		slog.Info("GitHub App credentials not set, skipping runner deregistration")
		return
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		slog.Warn("invalid APP_ID, skipping runner deregistration", "error", err)
		return
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		slog.Warn("invalid APP_INSTALLATION_ID, skipping runner deregistration", "error", err)
		return
	}
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		slog.Warn("failed to read private key, skipping runner deregistration", "error", err)
		return
	}

	githubApp := &gh.App{
		AppID:          appID,
		InstallationID: installID,
		PrivateKey:     privateKey,
	}

	repos, err := githubApp.ListInstallationRepos()
	if err != nil {
		slog.Error("failed to list installation repos", "error", err)
		return
	}

	totalRemoved := 0
	for _, repo := range repos {
		removed, err := githubApp.RemoveOfflineRepoRunners(repo[0], repo[1])
		if err != nil {
			slog.Error("failed to clean runners", "repo", repo[0]+"/"+repo[1], "error", err)
			continue
		}
		totalRemoved += removed
	}

	slog.Info("runner deregistration complete", "removed", totalRemoved)
}
