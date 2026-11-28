// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func openTestStateStore(t *testing.T) state.Store {
	t.Helper()
	store, err := state.OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type captureCompute struct {
	*fakeCompute
	lastParams compute.RunnerParams
}

func (c *captureCompute) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	c.lastParams = params
	return c.fakeCompute.CreateRunner(ctx, params)
}

func TestHandlerPassesResolvedSizeAndDeadmanToCompute(t *testing.T) {
	store := openTestStateStore(t)
	ghClient := &fakeGitHub{}
	baseCompute := &fakeCompute{}
	capturing := &captureCompute{fakeCompute: baseCompute}

	handler, err := NewHandler(Config{
		WebhookSecret:          []byte(testWebhookSecret),
		GitHubClient:           ghClient,
		ComputeClient:          capturing,
		Store:                  store,
		RequiredLabel:          "self-hosted",
		AllowedLabels:          []string{"self-hosted", "large", "chef"},
		LabelSizeMap:           map[string]string{"large": "c-16"},
		AllowedRepositories:    []string{"trusted/private-repo"},
		RunnerVersion:          "2.336.0",
		RunnerSHA256:           strings.Repeat("a", 64),
		ChefInstallerSHA256:    strings.Repeat("b", 64),
		DeadmanShutdownMinutes: 120,
		InstallationID:         42,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	ctx := context.Background()
	key := "trusted/private-repo:500"
	if _, err := store.Create(ctx, state.Record{
		Key:        key,
		DeliveryID: "delivery-size-1",
		JobID:      500,
		Owner:      "trusted",
		Repository: "private-repo",
		Labels:     []string{"self-hosted", "large"},
		Provider:   "test",
	}); err != nil {
		t.Fatal(err)
	}
	record, kind, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || record == nil || kind != state.WorkProvision {
		t.Fatalf("ClaimNext() = %#v %q %v", record, kind, err)
	}
	handler.provision(ctx, *record)
	if capturing.lastParams.InstanceSize != "c-16" {
		t.Fatalf("InstanceSize = %q, want c-16", capturing.lastParams.InstanceSize)
	}
	if capturing.lastParams.DeadmanShutdownMinutes != 120 {
		t.Fatalf("DeadmanShutdownMinutes = %d, want 120", capturing.lastParams.DeadmanShutdownMinutes)
	}
}

func TestNewHandlerLabelSizeMapAndDeadmanValidation(t *testing.T) {
	baseCfg := func() Config {
		return Config{
			WebhookSecret:       []byte(testWebhookSecret),
			GitHubClient:        &fakeGitHub{},
			ComputeClient:       &fakeCompute{},
			Store:               openTestStateStore(t),
			RequiredLabel:       "self-hosted",
			AllowedLabels:       []string{"self-hosted", "large"},
			AllowedRepositories: []string{"trusted/private-repo"},
			RunnerVersion:       "2.336.0",
			RunnerSHA256:        strings.Repeat("a", 64),
			ChefInstallerSHA256: strings.Repeat("b", 64),
			InstallationID:      42,
		}
	}

	t.Run("default deadman is 180 minutes", func(t *testing.T) {
		h, err := NewHandler(baseCfg())
		if err != nil {
			t.Fatal(err)
		}
		if h.deadmanShutdownMinutes != compute.DefaultDeadmanShutdownMinutes {
			t.Fatalf("deadman = %d, want %d", h.deadmanShutdownMinutes, compute.DefaultDeadmanShutdownMinutes)
		}
	})

	t.Run("disable deadman sets zero", func(t *testing.T) {
		cfg := baseCfg()
		cfg.DisableDeadmanShutdown = true
		h, err := NewHandler(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if h.deadmanShutdownMinutes != 0 {
			t.Fatalf("deadman = %d, want 0", h.deadmanShutdownMinutes)
		}
	})

	t.Run("rejects negative deadman", func(t *testing.T) {
		cfg := baseCfg()
		cfg.DeadmanShutdownMinutes = -10
		if _, err := NewHandler(cfg); err == nil {
			t.Fatal("accepted negative dead-man minutes")
		}
	})

	t.Run("rejects label outside ALLOWED_LABELS", func(t *testing.T) {
		cfg := baseCfg()
		cfg.LabelSizeMap = map[string]string{"gpu": "g5.xlarge"}
		if _, err := NewHandler(cfg); err == nil {
			t.Fatal("accepted unallowed label in LabelSizeMap")
		}
	})

	t.Run("rejects invalid size value", func(t *testing.T) {
		cfg := baseCfg()
		cfg.LabelSizeMap = map[string]string{"large": "../evil"}
		if _, err := NewHandler(cfg); err == nil {
			t.Fatal("accepted invalid size value in LabelSizeMap")
		}
	})
}
