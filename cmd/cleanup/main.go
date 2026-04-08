package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
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

	log.Printf("Cleanup complete: deleted %d stale runner droplets", deleted)
}
