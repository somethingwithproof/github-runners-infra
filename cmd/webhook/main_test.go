package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/thomasvincent/github-runners-infra/internal/github"
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

func TestServeUntilShutdownDrainsWorkers(t *testing.T) {
	processCtx, stopProcess := context.WithCancel(context.Background())
	started := make(chan context.Context, 1)
	shutdown := make(chan struct{})
	srv := &http.Server{}
	srv.RegisterOnShutdown(func() { close(shutdown) })

	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(
			processCtx,
			srv,
			func(workerCtx context.Context) { started <- workerCtx },
			func() {
				workerCtx := <-started
				if workerCtx.Err() == nil {
					t.Error("workers were awaited before cancellation")
				}
			},
			func() error {
				<-shutdown
				return http.ErrServerClosed
			},
		)
	}()

	stopProcess()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("graceful shutdown did not drain workers")
	}
}

func TestServeUntilShutdownPropagatesListenerFailureAndDrainsWorkers(t *testing.T) {
	want := errors.New("listener failed")
	workerCanceled := false
	var workerCtx context.Context
	err := serveUntilShutdown(
		context.Background(),
		&http.Server{},
		func(ctx context.Context) { workerCtx = ctx },
		func() { workerCanceled = workerCtx.Err() != nil },
		func() error { return want },
	)
	if !errors.Is(err, want) || !workerCanceled {
		t.Fatalf("listener failure = %v, workers canceled=%v", err, workerCanceled)
	}
}

func TestValidateGitHubTokenScopeRetriesThrottles(t *testing.T) {
	calls := 0
	err := validateGitHubTokenScopeWithPolicy(context.Background(), func(context.Context) error {
		calls++
		if calls == 1 {
			return &gh.RateLimitError{ResetAt: time.Now().Add(time.Millisecond), Status: http.StatusTooManyRequests}
		}
		return nil
	}, testTokenRetryPolicy(2))
	if err != nil || calls != 2 {
		t.Fatalf("rate-limited validation = calls %d, error %v", calls, err)
	}
}

func TestValidateGitHubTokenScopeBoundsPastResetRetries(t *testing.T) {
	for name, resetAt := range map[string]time.Time{
		"retry-after zero":      {},
		"past rate-limit reset": time.Now().Add(-time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			var delays []time.Duration
			policy := testTokenRetryPolicy(3)
			policy.minDelay = 5 * time.Second
			policy.wait = func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			}
			err := validateGitHubTokenScopeWithPolicy(context.Background(), func(context.Context) error {
				calls++
				return &gh.RateLimitError{ResetAt: resetAt, Status: http.StatusTooManyRequests}
			}, policy)
			if err == nil || calls != 3 || !reflect.DeepEqual(delays, []time.Duration{5 * time.Second, 5 * time.Second}) {
				t.Fatalf("bounded throttle = calls %d, delays %v, error %v", calls, delays, err)
			}
		})
	}
}

func testTokenRetryPolicy(maxAttempts int) tokenValidationRetryPolicy {
	return tokenValidationRetryPolicy{
		maxElapsed:  time.Hour,
		maxAttempts: maxAttempts,
		now:         time.Now,
		wait:        func(context.Context, time.Duration) error { return nil },
	}
}

func TestValidateGitHubTokenScopeStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := validateGitHubTokenScope(ctx, func(context.Context) error {
		return &gh.RateLimitError{ResetAt: time.Now().Add(time.Hour), Status: http.StatusForbidden}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown during token throttle = %v", err)
	}
}

func TestValidateGitHubTokenScopeReturnsNonThrottleError(t *testing.T) {
	want := errors.New("invalid token scope")
	err := validateGitHubTokenScope(context.Background(), func(context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("non-throttle validation error = %v", err)
	}
}

type errorCloser struct{ err error }

func (c errorCloser) Close() error { return c.err }

func TestCloseGCPClientLogsFailure(t *testing.T) {
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(original) })
	closeGCPClient(errorCloser{err: errors.New("close failed")})
	if !strings.Contains(output.String(), "close GCP Compute client") || !strings.Contains(output.String(), "close failed") {
		t.Fatalf("GCP close log = %q", output.String())
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
	t.Run("invalid spot boolean", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("RUNNER_SPOT", "sometimes")
		if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), "RUNNER_SPOT") {
			t.Fatalf("invalid RUNNER_SPOT error = %v", err)
		}
	})
	t.Run("unsupported provider", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "unknown")
		if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil {
			t.Fatal("newComputeClient accepted unsupported provider")
		}
	})
	t.Run("invalid AWS external IP boolean", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "aws")
		t.Setenv("AWS_REGION", "us-west-2")
		t.Setenv("AWS_AMI_ID", "ami-test")
		t.Setenv("AWS_INSTANCE_TYPE", "m7i.large")
		t.Setenv("AWS_SUBNET_ID", "subnet-test")
		t.Setenv("AWS_SECURITY_GROUP_IDS", "sg-test")
		t.Setenv("AWS_EXTERNAL_IP", "sometimes")
		if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), "AWS_EXTERNAL_IP") {
			t.Fatalf("invalid AWS_EXTERNAL_IP error = %v", err)
		}
	})
	t.Run("invalid GCP external IP boolean", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "gcp")
		t.Setenv("GCP_PROJECT_ID", "project-test")
		t.Setenv("GCP_ZONE", "us-central1-a")
		t.Setenv("GCP_MACHINE_TYPE", "n2-standard-4")
		t.Setenv("GCP_SOURCE_IMAGE", "projects/test/global/images/pinned")
		t.Setenv("GCP_SUBNETWORK", "projects/test/regions/us-central1/subnetworks/runners")
		t.Setenv("GCP_RUNNER_SERVICE_ACCOUNT_EMAIL", "runner@test.iam.gserviceaccount.com")
		t.Setenv("GCP_EXTERNAL_IP", "sometimes")
		if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), "GCP_EXTERNAL_IP") {
			t.Fatalf("invalid GCP_EXTERNAL_IP error = %v", err)
		}
	})
	t.Run("DigitalOcean spot", func(t *testing.T) {
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("COMPUTE_PROVIDER", "digitalocean")
		t.Setenv("RUNNER_SPOT", "true")
		if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil {
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
		if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), "read Azure SSH public key") {
			t.Fatalf("unreadable Azure SSH key error = %v", err)
		}
	})
}

func TestNewComputeClientRequiresProviderInputs(t *testing.T) {
	for _, test := range []struct {
		provider, missing string
		required          map[string]string
	}{
		{provider: "digitalocean", missing: "DIGITALOCEAN_TOKEN", required: map[string]string{
			"DIGITALOCEAN_TOKEN": "token", "DO_VPC_UUID": "vpc", "DO_FIREWALL_ID": "firewall",
		}},
		{provider: "aws", missing: "AWS_REGION", required: map[string]string{
			"AWS_REGION": "us-west-2", "AWS_AMI_ID": "ami-test", "AWS_INSTANCE_TYPE": "m7i.large",
			"AWS_SUBNET_ID": "subnet-test", "AWS_SECURITY_GROUP_IDS": "sg-test",
		}},
		{provider: "gcp", missing: "GCP_PROJECT_ID", required: map[string]string{
			"GCP_PROJECT_ID": "project", "GCP_ZONE": "us-central1-a", "GCP_MACHINE_TYPE": "n2-standard-4",
			"GCP_SOURCE_IMAGE":                 "projects/test/global/images/pinned",
			"GCP_SUBNETWORK":                   "projects/test/regions/us-central1/subnetworks/runners",
			"GCP_RUNNER_SERVICE_ACCOUNT_EMAIL": "runner@test.iam.gserviceaccount.com",
		}},
		{provider: "azure", missing: "AZURE_SSH_PUBLIC_KEY_FILE", required: map[string]string{
			"AZURE_SSH_PUBLIC_KEY_FILE": "/does/not/matter", "AZURE_SUBSCRIPTION_ID": "subscription",
			"AZURE_RESOURCE_GROUP": "resource-group", "AZURE_LOCATION": "westus",
			"AZURE_VM_SIZE": "Standard_D2s_v5", "AZURE_IMAGE": "Canonical:ubuntu-24_04-lts:server:latest",
			"AZURE_SUBNET_ID": "/subscriptions/subnet",
		}},
	} {
		t.Run(test.provider, func(t *testing.T) {
			t.Setenv("CONTROLLER_ID", "controller-1")
			t.Setenv("COMPUTE_PROVIDER", test.provider)
			for key, value := range test.required {
				t.Setenv(key, value)
			}
			t.Setenv(test.missing, "")
			if _, _, err := newComputeClient(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), test.missing) {
				t.Fatalf("missing %s error = %v", test.missing, err)
			}
		})
	}
}

func TestNewComputeClientDigitalOceanHappyPath(t *testing.T) {
	templatePath := writeRunnerTemplate(t)
	t.Setenv("COMPUTE_PROVIDER", "digitalocean")
	t.Setenv("CONTROLLER_ID", "controller-1")
	t.Setenv("CLOUD_INIT_PATH", templatePath)
	t.Setenv("RUNNER_SPOT", "false")
	t.Setenv("DIGITALOCEAN_TOKEN", "test-token")
	t.Setenv("DO_VPC_UUID", "vpc-id")
	t.Setenv("DO_FIREWALL_ID", "firewall-id")
	client, closeClient, err := newComputeClient(context.Background(), context.Background())
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

func TestNewComputeClientCloudProviderHappyPaths(t *testing.T) {
	t.Run("aws", func(t *testing.T) {
		t.Setenv("COMPUTE_PROVIDER", "aws")
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("CLOUD_INIT_PATH", writeRunnerTemplate(t))
		t.Setenv("AWS_REGION", "us-west-2")
		t.Setenv("AWS_AMI_ID", "ami-0123456789abcdef0")
		t.Setenv("AWS_INSTANCE_TYPE", "m7i.large")
		t.Setenv("AWS_SUBNET_ID", "subnet-test")
		t.Setenv("AWS_SECURITY_GROUP_IDS", "sg-test")
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
		t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
		assertComputeProvider(t, "aws")
	})
	t.Run("gcp", func(t *testing.T) {
		credentials := filepath.Join(t.TempDir(), "gcp.json")
		if err := os.WriteFile(credentials, []byte(`{"type":"authorized_user","client_id":"test","client_secret":"test","refresh_token":"test"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)
		t.Setenv("COMPUTE_PROVIDER", "gcp")
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("CLOUD_INIT_PATH", writeRunnerTemplate(t))
		t.Setenv("GCP_PROJECT_ID", "project-test")
		t.Setenv("GCP_ZONE", "us-central1-a")
		t.Setenv("GCP_MACHINE_TYPE", "n2-standard-4")
		t.Setenv("GCP_SOURCE_IMAGE", "projects/ubuntu-os-cloud/global/images/ubuntu-pinned")
		t.Setenv("GCP_SUBNETWORK", "projects/project-test/regions/us-central1/subnetworks/runners")
		t.Setenv("GCP_RUNNER_SERVICE_ACCOUNT_EMAIL", "runner@project-test.iam.gserviceaccount.com")
		assertComputeProvider(t, "gcp")
	})
	t.Run("azure", func(t *testing.T) {
		sshKey := filepath.Join(t.TempDir(), "runner.pub")
		if err := os.WriteFile(sshKey, []byte("ssh-ed25519 test"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AZURE_TENANT_ID", "00000000-0000-0000-0000-000000000001")
		t.Setenv("AZURE_CLIENT_ID", "00000000-0000-0000-0000-000000000002")
		t.Setenv("AZURE_CLIENT_SECRET", "test-secret")
		t.Setenv("COMPUTE_PROVIDER", "azure")
		t.Setenv("CONTROLLER_ID", "controller-1")
		t.Setenv("CLOUD_INIT_PATH", writeRunnerTemplate(t))
		t.Setenv("AZURE_SSH_PUBLIC_KEY_FILE", sshKey)
		t.Setenv("AZURE_SUBSCRIPTION_ID", "00000000-0000-0000-0000-000000000003")
		t.Setenv("AZURE_RESOURCE_GROUP", "runner-test")
		t.Setenv("AZURE_LOCATION", "westus2")
		t.Setenv("AZURE_VM_SIZE", "Standard_D4s_v5")
		t.Setenv("AZURE_IMAGE", "Canonical:ubuntu-24_04-lts:server:24.04.202601010")
		t.Setenv("AZURE_SUBNET_ID", "/subscriptions/test/subnets/runners")
		assertComputeProvider(t, "azure")
	})
}

func assertComputeProvider(t *testing.T, want string) {
	t.Helper()
	client, closeClient, err := newComputeClient(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("newComputeClient(%s) error = %v", want, err)
	}
	defer closeClient()
	if client == nil || client.Provider() != want {
		t.Fatalf("newComputeClient(%s) = %#v", want, client)
	}
}

func writeRunnerTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.yaml.tmpl")
	if err := os.WriteFile(path, []byte("#cloud-config\n{{.RunnerJITConfig}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRuntimeConfigEndToEnd(t *testing.T) {
	setValidRuntimeEnvironment(t)
	t.Setenv("REQUIRED_LABEL", "ephemeral")
	t.Setenv("LISTEN_ADDR", "127.0.0.1:9090")
	cfg, err := loadRuntimeConfig()
	if err != nil {
		t.Fatalf("loadRuntimeConfig error = %v", err)
	}
	if cfg.appID != 1 || cfg.installationID != 2 || len(cfg.allowedRepositories) != 1 || cfg.maxAttempts != 5 ||
		cfg.requiredLabel != "ephemeral" || cfg.listenAddr != "127.0.0.1:9090" {
		t.Fatalf("runtime config = %#v", cfg)
	}
}

func TestLoadRuntimeConfigPreservesWebhookSecretBytes(t *testing.T) {
	setValidRuntimeEnvironment(t)
	t.Setenv("WEBHOOK_SECRET", "  secret bytes are not identifiers  ")
	cfg, err := loadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.webhookSecret != "  secret bytes are not identifiers  " {
		t.Fatalf("webhook secret was modified: %q", cfg.webhookSecret)
	}
}

func TestLoadRuntimeConfigRejectsMissingAndMalformedValues(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		setValidRuntimeEnvironment(t)
		t.Setenv("APP_PRIVATE_KEY_FILE", "")
		if _, err := loadRuntimeConfig(); err == nil {
			t.Fatal("loadRuntimeConfig accepted a missing private key path")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		setValidRuntimeEnvironment(t)
		t.Setenv("MAX_ATTEMPTS", "not-an-integer")
		if _, err := loadRuntimeConfig(); err == nil {
			t.Fatal("loadRuntimeConfig accepted malformed MAX_ATTEMPTS")
		}
	})
	for _, test := range []struct {
		key, value string
	}{
		{key: "MAX_ATTEMPTS", value: "  invalid  "},
		{key: "MAX_RUNNER_AGE", value: "  invalid  "},
	} {
		t.Run("trimmed invalid "+test.key, func(t *testing.T) {
			setValidRuntimeEnvironment(t)
			t.Setenv(test.key, test.value)
			if _, err := loadRuntimeConfig(); err == nil {
				t.Fatalf("loadRuntimeConfig accepted %s=%q", test.key, test.value)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsInvalidOptionalBoundaries(t *testing.T) {
	for _, key := range []string{
		"RUNNER_GROUP_ID", "WORKER_COUNT", "MAX_LIVE_RUNNERS", "MAX_ATTEMPTS",
		"MAX_RUNNER_AGE", "RUNNER_REGISTRATION_TIMEOUT", "CANCELLED_RUNNER_TTL", "DELETED_RECORD_RETENTION",
		"LIVENESS_SETTLE_WINDOW", "LIVENESS_CONFIRMATIONS",
	} {
		t.Run(key, func(t *testing.T) {
			setValidRuntimeEnvironment(t)
			t.Setenv(key, "0")
			if _, err := loadRuntimeConfig(); err == nil {
				t.Fatalf("loadRuntimeConfig accepted %s=0", key)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsWhitespaceOnlyAllowlists(t *testing.T) {
	for _, key := range []string{"ALLOWED_REPOSITORIES", "ALLOWED_LABELS"} {
		t.Run(key, func(t *testing.T) {
			setValidRuntimeEnvironment(t)
			t.Setenv(key, " ,  , ")
			if _, err := loadRuntimeConfig(); err == nil {
				t.Fatalf("loadRuntimeConfig accepted whitespace-only %s", key)
			}
		})
	}
}

func setValidRuntimeEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"APP_ID": "1", "APP_INSTALLATION_ID": "2", "APP_PRIVATE_KEY_FILE": "/tmp/key.pem",
		"ALLOWED_REPOSITORIES": "trusted/private-repo", "ALLOWED_LABELS": "self-hosted,chef",
		"WEBHOOK_SECRET": "unit-test-webhook-secret-not-a-credential", "RUNNER_VERSION": "2.331.0",
		"RUNNER_SHA256": strings.Repeat("a", 64), "CHEF_INSTALLER_SHA256": strings.Repeat("b", 64),
	} {
		t.Setenv(key, value)
	}
}
