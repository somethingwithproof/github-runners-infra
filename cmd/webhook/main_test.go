package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// buildProvisioner's failure paths call log.Fatalf (os.Exit), so they are
// exercised by re-executing the test binary as a subprocess and asserting it
// exits non-zero. TestMain runs the helper when BUILD_PROV_CASE is set; the
// parent tests set it and inspect the child's exit status.
func TestMain(m *testing.M) {
	if c := os.Getenv("BUILD_PROV_CASE"); c != "" {
		switch c {
		case "gcp_missing_env":
			os.Setenv("RUNNER_PROVIDER", "gcp")
			// GCP_PROJECT / GCP_ZONE deliberately unset: mustEnv fatals.
		case "unknown_provider":
			os.Setenv("RUNNER_PROVIDER", "wat")
			// Unknown value falls to the DO branch; DIGITALOCEAN_TOKEN unset fatals.
		}
		_, _, _ = buildProvisioner(context.Background())
		return
	}
	os.Exit(m.Run())
}

func TestBuildProvisionerGCPMissingEnvFatal(t *testing.T) {
	assertFatalExit(t, "gcp_missing_env")
}

func TestBuildProvisionerUnknownProviderFatal(t *testing.T) {
	assertFatalExit(t, "unknown_provider")
}

func assertFatalExit(t *testing.T, kase string) {
	t.Helper()
	// #nosec G204 -- re-exec of this test binary; the only variable is a fixed
	// case label, not external input. Standard Go pattern for log.Fatalf paths.
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(),
		"BUILD_PROV_CASE="+kase,
		// Clear creds so the fatal path is reached deterministically.
		"GCP_PROJECT=", "GCP_ZONE=", "DIGITALOCEAN_TOKEN=",
	)
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("case %q: expected exec.ExitError (non-zero exit), got %v", kase, err)
	}
	if ee.Success() {
		t.Fatalf("case %q: child exited 0, want fatal", kase)
	}
}
