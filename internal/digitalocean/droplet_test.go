package digitalocean

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"text/template"

	"github.com/digitalocean/godo"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

func TestCloudInitContainsOnlyJITBootstrapMaterial(t *testing.T) {
	data, err := os.ReadFile("../../cloud-init/runner.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"DOToken", "DIGITALOCEAN_TOKEN", "aws ssm", "CallbackSecret", "/callback/destroy"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("cloud-init contains forbidden control-plane material %q", forbidden)
		}
	}
	for _, required := range []string{"ssh_pwauth: false", "disable_root: true", "set -eu"} {
		if !strings.Contains(text, required) {
			t.Fatalf("cloud-init is missing hardening directive %q", required)
		}
	}
	if strings.Contains(text, "pipefail") {
		t.Fatal("cloud-init uses non-POSIX pipefail under cloud-init's /bin/sh")
	}

	tmpl, err := template.New("runner").Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	err = tmpl.Execute(&rendered, compute.RunnerParams{
		RunnerJITConfig:     "encoded-jit-config",
		RunnerVersion:       "2.331.0",
		RunnerSHA256:        strings.Repeat("a", 64),
		ChefInstallerSHA256: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), "./run.sh --jitconfig") {
		t.Fatal("rendered cloud-init does not start a JIT runner")
	}
}

func TestNewClientRequiresPrivateNetworkAndFirewall(t *testing.T) {
	base := Config{ControllerID: "test"}
	if _, err := NewClient(base); err == nil {
		t.Fatal("accepted configuration without a VPC and firewall")
	}
	base.VPCUUID = "vpc-id"
	if _, err := NewClient(base); err == nil {
		t.Fatal("accepted configuration without a firewall")
	}
	base.FirewallID = "firewall-id"
	if _, err := NewClient(base); err != nil {
		t.Fatalf("rejected private configuration: %v", err)
	}
}

func TestValidateRunnerFirewallFailsClosed(t *testing.T) {
	controllerTag := "runner-controller-test"
	if err := validateRunnerFirewall(&godo.Firewall{
		ID:           "firewall-id",
		InboundRules: []godo.InboundRule{{Protocol: "tcp"}},
		Tags:         []string{controllerTag},
	}, controllerTag); err == nil {
		t.Fatal("accepted a firewall with an inbound rule")
	}
	if err := validateRunnerFirewall(&godo.Firewall{
		ID:   "firewall-id",
		Tags: []string{"runner-controller-other"},
	}, controllerTag); err == nil {
		t.Fatal("accepted a firewall that does not target the controller tag")
	}
	if err := validateRunnerFirewall(&godo.Firewall{
		ID:   "firewall-id",
		Tags: []string{controllerTag},
	}, controllerTag); err != nil {
		t.Fatalf("rejected deny-inbound controller firewall: %v", err)
	}
}

func TestFindRunnerRejectsDuplicateJobDroplets(t *testing.T) {
	client := testDropletClient(t, `{"droplets":[
		{"id":1,"name":"one","tags":["runner-controller-test"]},
		{"id":2,"name":"two","tags":["runner-controller-test"]}
	]}`)
	if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); err == nil {
		t.Fatal("accepted duplicate job-tagged droplets")
	}
}

func TestFindRunnerRejectsForeignControllerDroplet(t *testing.T) {
	client := testDropletClient(t, `{"droplets":[
		{"id":1,"name":"foreign","tags":["runner-controller-other"]}
	]}`)
	_, _, err := client.FindRunner(context.Background(), "org/repo:1")
	if !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("foreign droplet error = %v", err)
	}
}

func testDropletClient(t *testing.T, response string) *Client {
	t.Helper()
	httpClient := &http.Client{Transport: doRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	})}
	api := godo.NewClient(httpClient)
	return &Client{client: api, controllerTag: "runner-controller-test"}
}

type doRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn doRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestRunnerJobTagIsStableAndScoped(t *testing.T) {
	first := runnerJobTag("org/repo:1")
	if first != runnerJobTag("org/repo:1") {
		t.Fatal("job tag is not deterministic")
	}
	if first == runnerJobTag("org/repo:2") {
		t.Fatal("different jobs received the same tag")
	}
	if !strings.HasPrefix(first, "runner-job-") {
		t.Fatalf("unexpected tag %q", first)
	}
}
