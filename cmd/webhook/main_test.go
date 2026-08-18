package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestUnexpectedListenerErrorControlsProcessFailure(t *testing.T) {
	if err := unexpectedListenerError(nil); err != nil {
		t.Fatalf("nil listener result = %v", err)
	}
	if err := unexpectedListenerError(http.ErrServerClosed); err != nil {
		t.Fatalf("normal server close = %v", err)
	}
	want := errors.New("bind failed")
	if err := unexpectedListenerError(want); !errors.Is(err, want) {
		t.Fatalf("listener failure = %v", err)
	}
}

func TestPositiveIntegerConfiguration(t *testing.T) {
	for _, value := range []string{"invalid", "0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEST_INT", value)
			if _, err := requiredPositiveInt64("TEST_INT"); err == nil {
				t.Fatal("requiredPositiveInt64 accepted invalid value")
			}
			if _, err := envPositiveInt64("TEST_INT", 1); err == nil {
				t.Fatal("envPositiveInt64 accepted invalid value")
			}
			if _, err := envPositiveInt("TEST_INT", 1); err == nil {
				t.Fatal("envPositiveInt accepted invalid value")
			}
		})
	}
	t.Setenv("TEST_INT", "42")
	if value, err := envPositiveInt("TEST_INT", 1); err != nil || value != 42 {
		t.Fatalf("envPositiveInt = %d, %v", value, err)
	}
}

func TestBooleanAndDurationConfiguration(t *testing.T) {
	t.Setenv("TEST_BOOL", "not-a-boolean")
	if _, err := envBoolValue("TEST_BOOL", false); err == nil {
		t.Fatal("envBoolValue accepted invalid value")
	}
	for _, value := range []string{"invalid", "0s", "-1s"} {
		t.Setenv("TEST_DURATION", value)
		if _, err := envDuration("TEST_DURATION", time.Second); err == nil {
			t.Fatalf("envDuration accepted %q", value)
		}
	}
	t.Setenv("TEST_DURATION", "5m")
	if value, err := envDuration("TEST_DURATION", time.Second); err != nil || value != 5*time.Minute {
		t.Fatalf("envDuration = %s, %v", value, err)
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" alpha, ,beta, ")
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCSV = %#v, want %#v", got, want)
	}
	if got := splitCSV("  , "); len(got) != 0 {
		t.Fatalf("empty splitCSV = %#v", got)
	}
}

func TestNewComputeClientRejectsInvalidProviderConfiguration(t *testing.T) {
	t.Run("unsupported provider", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "unknown")
		if _, _, err := newComputeClient(context.Background()); err == nil {
			t.Fatal("newComputeClient accepted unsupported provider")
		}
	})
	t.Run("DigitalOcean spot", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "digitalocean")
		t.Setenv("RUNNER_SPOT", "true")
		if _, _, err := newComputeClient(context.Background()); err == nil {
			t.Fatal("newComputeClient accepted DigitalOcean spot")
		}
	})
	t.Run("Azure unreadable SSH key", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "azure")
		t.Setenv("AZURE_SSH_PUBLIC_KEY_FILE", "/does/not/exist")
		t.Setenv("AZURE_SUBSCRIPTION_ID", "subscription")
		t.Setenv("AZURE_RESOURCE_GROUP", "resource-group")
		t.Setenv("AZURE_LOCATION", "westus")
		t.Setenv("AZURE_VM_SIZE", "Standard_D2s_v5")
		t.Setenv("AZURE_IMAGE", "Canonical:ubuntu-24_04-lts:server:24.04.202601010")
		t.Setenv("AZURE_SUBNET_ID", "/subscriptions/subnet")
		if _, _, err := newComputeClient(context.Background()); err == nil {
			t.Fatal("newComputeClient accepted unreadable Azure SSH key")
		}
	})
}

func TestNewComputeClientDigitalOceanHappyPath(t *testing.T) {
	templatePath := filepath.Join(t.TempDir(), "runner.yaml.tmpl")
	if err := os.WriteFile(templatePath, []byte("#cloud-config\n{{.RunnerJITConfig}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPUTE_PROVIDER", "digitalocean")
	t.Setenv("CONTROLLER_ID", "controller-1")
	t.Setenv("CLOUD_INIT_PATH", templatePath)
	t.Setenv("RUNNER_SPOT", "false")
	t.Setenv("DIGITALOCEAN_TOKEN", "test-token")
	t.Setenv("DO_VPC_UUID", "vpc-id")
	t.Setenv("DO_FIREWALL_ID", "firewall-id")
	client, closeClient, err := newComputeClient(context.Background())
	if err != nil {
		t.Fatalf("newComputeClient error = %v", err)
	}
	if client == nil || closeClient == nil {
		t.Fatal("newComputeClient returned a nil client or close function")
	}
	if client.Provider() != "digitalocean" {
		t.Fatalf("provider = %q", client.Provider())
	}
	closeClient()
}
