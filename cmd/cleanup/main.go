package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
)

func main() {
	doToken := os.Getenv("DIGITALOCEAN_TOKEN")
	if doToken == "" {
		log.Fatal("DIGITALOCEAN_TOKEN is required")
	}

	client, err := digitalocean.NewClient(digitalocean.Config{
		Token:         doToken,
		CloudInitPath: "cloud-init/runner.yaml.tmpl",
	})
	if err != nil {
		log.Fatalf("Failed to create DO client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	maxAge := 60 * time.Minute
	deleted, err := client.CleanupOldDroplets(ctx, maxAge)
	if err != nil {
		log.Fatalf("Cleanup failed: %v", err)
	}
	log.Printf("Cleanup: deleted %d stale runner droplets", deleted)

	// Deregister offline ghost runners from GitHub (if credentials are available)
	appIDStr := os.Getenv("APP_ID")
	installIDStr := os.Getenv("APP_INSTALLATION_ID")
	keyPath := os.Getenv("APP_PRIVATE_KEY_FILE")
	if appIDStr == "" || installIDStr == "" || keyPath == "" {
		log.Printf("GitHub App credentials not set, skipping runner deregistration")
		return
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		log.Printf("Invalid APP_ID, skipping runner deregistration: %v", err)
		return
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		log.Printf("Invalid APP_INSTALLATION_ID, skipping runner deregistration: %v", err)
		return
	}
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		log.Printf("Failed to read private key, skipping runner deregistration: %v", err)
		return
	}

	githubApp := &gh.App{
		AppID:          appID,
		InstallationID: installID,
		PrivateKey:     privateKey,
	}

	repos, err := githubApp.ListInstallationRepos()
	if err != nil {
		log.Printf("Failed to list installation repos: %v", err)
		return
	}

	totalRemoved := 0
	for _, repo := range repos {
		removed, err := githubApp.RemoveOfflineRepoRunners(repo[0], repo[1])
		if err != nil {
			log.Printf("Failed to clean runners for %s/%s: %v", repo[0], repo[1], err)
			continue
		}
		totalRemoved += removed
	}

	log.Printf("Cleanup: deregistered %d offline ghost runners from GitHub", totalRemoved)
}
