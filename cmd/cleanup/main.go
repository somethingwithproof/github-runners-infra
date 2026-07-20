package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	"github.com/thomasvincent/github-runners-infra/internal/envutil"
	"github.com/thomasvincent/github-runners-infra/internal/gcp"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/provider"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	prov, err := buildProvisioner(ctx)
	if err != nil {
		log.Fatalf("Failed to create runner provisioner: %v", err)
	}

	maxAge := 60 * time.Minute
	deleted, err := prov.CleanupOld(ctx, maxAge)
	if err != nil {
		log.Fatalf("Cleanup failed: %v", err)
	}
	log.Printf("Cleanup: deleted %d stale runner instances", deleted)

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

// buildProvisioner selects the runner backend from RUNNER_PROVIDER (default
// "digitalocean"). NewClient parses the startup template even though cleanup
// never provisions, so resolve the path the same way the webhook does instead
// of hardcoding a relative path that only works from the repo root.
func buildProvisioner(ctx context.Context) (provider.Provisioner, error) {
	switch strings.ToLower(envutil.OrDefault("RUNNER_PROVIDER", "digitalocean")) {
	case "gcp":
		return gcp.NewClient(ctx, gcp.Config{
			Project:           envutil.MustEnv("GCP_PROJECT"),
			Zone:              envutil.MustEnv("GCP_ZONE"),
			MachineType:       envutil.OrDefault("GCP_MACHINE_TYPE", "e2-custom-4-8192"),
			RunnerSA:          os.Getenv("GCP_RUNNER_SA"),
			Network:           os.Getenv("GCP_NETWORK"),
			Subnet:            os.Getenv("GCP_SUBNET"),
			SourceImage:       os.Getenv("GCP_IMAGE"),
			StartupScriptPath: envutil.OrDefault("GCP_STARTUP_SCRIPT", "cloud-init/runner-gcp.sh.tmpl"),
		})

	default:
		doToken := os.Getenv("DIGITALOCEAN_TOKEN")
		if doToken == "" {
			log.Fatal("DIGITALOCEAN_TOKEN is required")
		}
		return digitalocean.NewClient(digitalocean.Config{
			Token:         doToken,
			CloudInitPath: envutil.OrDefault("CLOUD_INIT_PATH", "cloud-init/runner.yaml.tmpl"),
		})
	}
}
