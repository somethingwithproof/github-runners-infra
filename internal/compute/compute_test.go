// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"text/template"
)

func TestValidControllerID(t *testing.T) {
	for _, value := range []string{"controller-1", "A", "runner_controller"} {
		if !ValidControllerID(value) {
			t.Fatalf("ValidControllerID(%q) = false", value)
		}
	}
	for _, value := range []string{"", "-controller", "_controller", "controller space", strings.Repeat("a", 65)} {
		if ValidControllerID(value) {
			t.Fatalf("ValidControllerID(%q) = true", value)
		}
	}
}

func TestResolveSpotAndPreference(t *testing.T) {
	if ResolveSpot(true, nil) != true || ResolveSpot(false, nil) != false {
		t.Fatal("ResolveSpot failed to apply provider default")
	}
	if ResolveSpot(true, BoolPtr(false)) != false || ResolveSpot(false, BoolPtr(true)) != true {
		t.Fatal("ResolveSpot failed to apply per-request override")
	}

	if got := ResolveSpotPreference([]string{"self-hosted", "large"}); got != nil {
		t.Fatalf("ResolveSpotPreference(no spot labels) = %v, want nil", *got)
	}
	if got := ResolveSpotPreference([]string{"self-hosted", "Spot"}); got == nil || !*got {
		t.Fatalf("ResolveSpotPreference(spot) = %v, want true", got)
	}
	if got := ResolveSpotPreference([]string{"self-hosted", "ON-DEMAND"}); got == nil || *got {
		t.Fatalf("ResolveSpotPreference(on-demand) = %v, want false", got)
	}
	// Deterministic precedence: on-demand wins when both labels appear.
	if got := ResolveSpotPreference([]string{"self-hosted", "spot", "on-demand"}); got == nil || *got {
		t.Fatalf("ResolveSpotPreference(spot+on-demand) = %v, want false", got)
	}
	if got := ResolveSpotPreference([]string{"on-demand", "spot", "self-hosted"}); got == nil || *got {
		t.Fatalf("ResolveSpotPreference(on-demand+spot) = %v, want false", got)
	}

	capErr := fmt.Errorf("%w: pool empty", ErrCapacityUnavailable)
	if !IsCapacityError(capErr) || IsCapacityError(errors.New("other")) {
		t.Fatal("IsCapacityError mismatch")
	}
	unsupErr := RejectSpotUnsupported("Vultr", true)
	if !IsSpotUnsupported(unsupErr) || RejectSpotUnsupported("Vultr", false) != nil {
		t.Fatal("RejectSpotUnsupported / IsSpotUnsupported mismatch")
	}
}

func TestRenderCloudInitSpotPreemptionWatcher(t *testing.T) {
	tmpl, err := template.ParseFiles("../../cloud-init/runner.yaml.tmpl")
	if err != nil {
		t.Fatalf("parse cloud-init template: %v", err)
	}
	base := RunnerParams{
		RunnerJITConfig:        "jit-config",
		RunnerVersion:          "2.336.0",
		RunnerSHA256:           strings.Repeat("a", 64),
		ChefInstallerSHA256:    strings.Repeat("b", 64),
		DeadmanShutdownMinutes: 60,
	}

	onDemandOut, err := RenderCloudInit(tmpl, base.WithSpot(false))
	if err != nil {
		t.Fatalf("RenderCloudInit(on-demand) error = %v", err)
	}
	if strings.Contains(onDemandOut, "spot_preemption_detected") {
		t.Fatal("on-demand cloud-init unexpectedly rendered spot preemption watcher")
	}

	spotOut, err := RenderCloudInit(tmpl, base.WithSpot(true))
	if err != nil {
		t.Fatalf("RenderCloudInit(spot) error = %v", err)
	}
	for _, want := range []string{
		"spot_preemption_detected",
		"latest/meta-data/spot/instance-action",
		"computeMetadata/v1/instance/preempted",
		"metadata/scheduledevents?api-version=2020-07-01",
		"opc/v2/instance/preemptionAction",
		"Authorization: Bearer Oracle",
		"pkill -TERM -f Runner.Listener",
		`kill "$PREEMPT_WATCH_PID"`,
	} {
		if !strings.Contains(spotOut, want) {
			t.Fatalf("spot cloud-init missing %q", want)
		}
	}
}
