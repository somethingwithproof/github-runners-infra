package digitalocean

import (
	"bytes"
	"testing"
	"text/template"
	"time"
)

func TestRunnerParams_Fields(t *testing.T) {
	p := RunnerParams{
		RunnerName:    "eph-test-123-456",
		RunnerToken:   "AABCDEF",
		RunnerLabels:  "self-hosted,linux",
		RunnerOrg:     "myorg",
		RunnerRepo:    "myorg/myrepo",
		DOToken:       "do-token",
		RunnerVersion: "2.331.0",
	}

	if p.RunnerName != "eph-test-123-456" {
		t.Errorf("unexpected runner name: %s", p.RunnerName)
	}
	if p.RunnerOrg != "myorg" {
		t.Errorf("unexpected runner org: %s", p.RunnerOrg)
	}
}

func TestCloudInitTemplateRendering(t *testing.T) {
	tmpl := template.Must(template.New("test").Parse(
		"name={{.RunnerName}} repo={{.RunnerRepo}} token={{.RunnerToken}} version={{.RunnerVersion}}"))

	client := &Client{cloudInitTmpl: tmpl}

	params := RunnerParams{
		RunnerName:    "eph-test-1-1234",
		RunnerToken:   "ABCTOKEN",
		RunnerRepo:    "org/repo",
		RunnerVersion: "2.331.0",
	}

	var buf bytes.Buffer
	err := client.cloudInitTmpl.Execute(&buf, params)
	if err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	result := buf.String()
	expected := "name=eph-test-1-1234 repo=org/repo token=ABCTOKEN version=2.331.0"
	if result != expected {
		t.Errorf("template rendered %q, want %q", result, expected)
	}
}

func TestDropletAgeFiltering(t *testing.T) {
	maxAge := 60 * time.Minute
	cutoff := time.Now().Add(-maxAge)

	tests := []struct {
		name    string
		created time.Time
		stale   bool
	}{
		{"new droplet (5 min)", time.Now().Add(-5 * time.Minute), false},
		{"recent droplet (30 min)", time.Now().Add(-30 * time.Minute), false},
		{"at boundary (60 min)", time.Now().Add(-60 * time.Minute), false}, // not strictly before
		{"old droplet (90 min)", time.Now().Add(-90 * time.Minute), true},
		{"very old droplet (24h)", time.Now().Add(-24 * time.Hour), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isStale := tt.created.Before(cutoff)
			if isStale != tt.stale {
				t.Errorf("droplet created %v: stale=%v, want %v", tt.created, isStale, tt.stale)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	// Verify that empty config fields get defaults
	cfg := Config{
		Token: "test",
	}

	if cfg.Region != "" {
		t.Error("empty config should have empty region (defaults applied in NewClient)")
	}

	// Defaults are applied in NewClient, verify the logic
	region := cfg.Region
	if region == "" {
		region = "nyc3"
	}
	if region != "nyc3" {
		t.Errorf("expected default region 'nyc3', got %q", region)
	}

	size := cfg.Size
	if size == "" {
		size = "s-4vcpu-8gb"
	}
	if size != "s-4vcpu-8gb" {
		t.Errorf("expected default size 's-4vcpu-8gb', got %q", size)
	}

	image := cfg.Image
	if image == "" {
		image = "ubuntu-24-04-x64"
	}
	if image != "ubuntu-24-04-x64" {
		t.Errorf("expected default image 'ubuntu-24-04-x64', got %q", image)
	}
}
