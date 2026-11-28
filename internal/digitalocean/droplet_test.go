// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/digitalocean/godo"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

func TestCloudInitContainsOnlyJITBootstrapMaterial(t *testing.T) {
	for _, name := range []string{"runner.yaml.tmpl", "runner-kadupul.yaml.tmpl", "runner-mantl.yaml.tmpl"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("../../cloud-init/" + name)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for _, forbidden := range []string{"DOToken", "DIGITALOCEAN_TOKEN", "VULTR_API_KEY", "LINODE_TOKEN", "aws ssm", "CallbackSecret", "/callback/destroy"} {
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
			if strings.Contains(rendered.String(), "runner-deadman-shutdown") {
				t.Fatal("rendered cloud-init included dead-man fuse when disabled")
			}

			var renderedDeadman bytes.Buffer
			err = tmpl.Execute(&renderedDeadman, compute.RunnerParams{
				RunnerJITConfig:        "encoded-jit-config",
				RunnerVersion:          "2.331.0",
				RunnerSHA256:           strings.Repeat("a", 64),
				ChefInstallerSHA256:    strings.Repeat("b", 64),
				DeadmanShutdownMinutes: 180,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"date -u -d '+180 minutes'", "OnCalendar=$deadline", "Persistent=true", "ExecStart=/sbin/shutdown --poweroff now", "systemctl enable --now runner-deadman-shutdown.timer"} {
				if !strings.Contains(renderedDeadman.String(), want) {
					t.Errorf("missing shutdown timer directive %q", want)
				}
			}
			if strings.Contains(renderedDeadman.String(), "set -eu#") || strings.Contains(renderedDeadman.String(), "set -eusystemctl") {
				t.Fatalf("dead-man template tags collapsed adjacent shell lines:\n%s", renderedDeadman.String())
			}
		})
	}

	t.Run("runner.yaml.tmpl prebaked and egress allowlist", func(t *testing.T) {
		data, err := os.ReadFile("../../cloud-init/runner.yaml.tmpl")
		if err != nil {
			t.Fatal(err)
		}
		tmpl, err := template.New("runner").Parse(string(data))
		if err != nil {
			t.Fatal(err)
		}
		out, err := compute.RenderCloudInit(tmpl, compute.RunnerParams{
			RunnerJITConfig:        "encoded-jit-config",
			PrebakedImage:          true,
			EgressAllowlist:        []string{"api.github.com", "140.82.112.0/20", "2001:db8::/32"},
			DeadmanShutdownMinutes: 180,
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "package_update: true") || strings.Contains(out, "omnitruck.chef.io") {
			t.Fatalf("prebaked cloud-init should skip package install and tarball download:\n%s", out)
		}
		for _, expected := range []string{"for filter in iptables ip6tables", `"$filter" -A RUNNER_HTTPS -j REJECT`, `"$filter" -I OUTPUT 1 -j RUNNER_HTTPS`, "'api.github.com'", "'2001:db8::/32'"} {
			if !strings.Contains(out, expected) {
				t.Errorf("missing egress rule %q", expected)
			}
		}
	})
}

func TestNewClientRequiresPrivateNetworkAndFirewall(t *testing.T) {
	base := Config{Token: "test-token", ControllerID: "test", CloudInitPath: "../../cloud-init/runner.yaml.tmpl"}
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

func TestNewClientRequiresToken(t *testing.T) {
	_, err := NewClient(Config{ControllerID: "test", VPCUUID: "vpc-id", FirewallID: "firewall-id"})
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("missing token error = %v", err)
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
	_, _, err := client.FindRunner(context.Background(), "org/repo:1")
	if !errors.Is(err, compute.ErrDuplicateInstances) {
		t.Fatalf("duplicate droplet error = %v", err)
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

func TestDeleteRunnerRejectsWrongJobTag(t *testing.T) {
	deleteCalls := 0
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			deleteCalls++
			return jsonResponse(http.StatusNoContent, ""), nil
		}
		return jsonResponse(http.StatusOK, `{"droplet":{"id":1,"name":"owned-other-job","tags":["runner-controller-test","`+runnerJobTag("org/repo:other")+`"]}}`), nil
	})}
	client := &Client{client: godo.NewClient(httpClient), controllerTag: "runner-controller-test"}
	err := client.DeleteRunner(context.Background(), "1", "org/repo:expected")
	if !errors.Is(err, compute.ErrOwnershipMismatch) || deleteCalls != 0 {
		t.Fatalf("wrong-job delete = %v, delete calls=%d", err, deleteCalls)
	}
}

func TestFindRunnerFollowsPagination(t *testing.T) {
	var pages []string
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		pages = append(pages, request.URL.Query().Get("page"))
		body := `{"droplets":[],"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets?page=2"}}}`
		if request.URL.Query().Get("page") == "2" {
			body = `{"droplets":[{"id":2,"name":"owned","tags":["runner-controller-test"]}]}`
		}
		return jsonResponse(http.StatusOK, body), nil
	})}
	client := &Client{client: godo.NewClient(httpClient), controllerTag: "runner-controller-test"}
	instance, found, err := client.FindRunner(context.Background(), "org/repo:1")
	if err != nil || !found || instance.ID != "2" {
		t.Fatalf("FindRunner() = %#v, %v, %v", instance, found, err)
	}
	if strings.Join(pages, ",") != "1,2" {
		t.Fatalf("requested pages = %v", pages)
	}
}

func TestCreateRunnerRecoversAmbiguousCreateByDeterministicName(t *testing.T) {
	params := compute.RunnerParams{JobKey: "org/repo:7", ProvisionEpoch: 2, RunnerName: "display-name"}
	wantName := dropletName(params.JobKey, params.ProvisionEpoch)
	listCalls, createCalls := 0, 0
	createCtx, cancelCreate := context.WithCancel(context.Background())
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v2/droplets":
			listCalls++
			if listCalls > 1 && request.Context().Err() != nil {
				t.Fatal("ambiguous-create recovery reused the cancelled create context")
			}
			if listCalls == 1 {
				return jsonResponse(http.StatusOK, `{"droplets":[]}`), nil
			}
			return jsonResponse(http.StatusOK, `{"droplets":[{"id":77,"name":"`+wantName+`","tags":["runner-controller-test"]}]}`), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v2/firewalls":
			return jsonResponse(http.StatusOK, `{"firewalls":[{"id":"firewall-id","tags":["runner-controller-test"],"inbound_rules":[]}]}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/v2/droplets":
			createCalls++
			var body struct {
				Name string   `json:"name"`
				Tags []string `json:"tags"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Name != wantName {
				t.Fatalf("create name = %q, want %q", body.Name, wantName)
			}
			if len(body.Tags) != 2 || body.Tags[0] != "runner-controller-test" || body.Tags[1] != runnerJobTag(params.JobKey) {
				t.Fatalf("create tags = %v", body.Tags)
			}
			cancelCreate()
			return nil, errors.New("ambiguous provider failure")
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}
	client := &Client{
		client: godo.NewClient(httpClient), cloudInitTmpl: template.Must(template.New("cloud-init").Parse("bootstrap")),
		region: "nyc3", size: "s-1vcpu-1gb", image: "ubuntu", controllerTag: "runner-controller-test",
		vpcUUID: "vpc-id", firewallID: "firewall-id",
		recoveryTimeout: 50 * time.Millisecond, recoveryPoll: time.Millisecond,
	}
	for attempt, ctx := range []context.Context{createCtx, context.Background()} {
		instance, err := client.CreateRunner(ctx, params)
		if err != nil || instance.ID != "77" || instance.Name != wantName {
			t.Fatalf("attempt %d CreateRunner() = %#v, %v", attempt+1, instance, err)
		}
	}
	if createCalls != 1 {
		t.Fatalf("provider create calls = %d, want 1", createCalls)
	}
}

func TestAmbiguousCreateRetriesAfterConfirmedAbsence(t *testing.T) {
	listCalls := 0
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v2/droplets":
			listCalls++
			return jsonResponse(http.StatusOK, `{"droplets":[]}`), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v2/firewalls":
			return jsonResponse(http.StatusOK, `{"firewalls":[{"id":"firewall-id","tags":["runner-controller-test"],"inbound_rules":[]}]}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/v2/droplets":
			return jsonResponse(http.StatusServiceUnavailable, `{"id":"unavailable","message":"temporary"}`), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}
	client := &Client{
		client: godo.NewClient(httpClient), cloudInitTmpl: template.Must(template.New("cloud-init").Parse("bootstrap")),
		region: "nyc3", size: "s-1vcpu-1gb", image: "ubuntu", controllerTag: "runner-controller-test",
		vpcUUID: "vpc-id", firewallID: "firewall-id",
		recoveryTimeout: 2 * time.Second, recoveryPoll: time.Millisecond,
	}
	_, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:absent", ProvisionEpoch: 1})
	if err == nil || errors.Is(err, compute.ErrCreateOutcomeUnknown) {
		t.Fatalf("confirmed-absent create error = %v", err)
	}
	if listCalls < 4 {
		t.Fatalf("clean absence observations = %d, want initial lookup plus at least three recovery polls", listCalls)
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

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
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

func TestDropletNameIsStableAndEpochScoped(t *testing.T) {
	first := dropletName("org/repo:1", 1)
	if first != dropletName("org/repo:1", 1) {
		t.Fatal("droplet name is not deterministic")
	}
	if first == dropletName("org/repo:1", 2) || first == dropletName("org/repo:2", 1) {
		t.Fatal("droplet name is not scoped to the job and provision epoch")
	}
	if !strings.HasPrefix(first, "ghr-") || len(first) > 64 {
		t.Fatalf("unexpected droplet name %q", first)
	}
}

func TestAmbiguousCreateErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		ambiguous bool
	}{
		{name: "transport", err: errors.New("connection reset"), ambiguous: true},
		{name: "request timeout", err: doStatusError(http.StatusRequestTimeout), ambiguous: true},
		{name: "server error", err: doStatusError(http.StatusInternalServerError), ambiguous: true},
		{name: "service unavailable", err: doStatusError(http.StatusServiceUnavailable), ambiguous: true},
		{name: "conflict", err: doStatusError(http.StatusConflict), ambiguous: false},
		{name: "too early", err: doStatusError(http.StatusTooEarly), ambiguous: false},
		{name: "rate limited", err: doStatusError(http.StatusTooManyRequests), ambiguous: false},
		{name: "canceled", err: context.Canceled, ambiguous: false},
		{name: "deadline", err: context.DeadlineExceeded, ambiguous: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isAmbiguousCreateError(test.err); got != test.ambiguous {
				t.Fatalf("isAmbiguousCreateError() = %v, want %v", got, test.ambiguous)
			}
		})
	}
}

func TestRunnerFirewallSetRejectsConflictingIngress(t *testing.T) {
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/v2/firewalls" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return jsonResponse(http.StatusOK, `{"firewalls":[
			{"id":"firewall-id","tags":["runner-controller-test"],"inbound_rules":[]},
			{"id":"conflicting","tags":["runner-controller-test"],"inbound_rules":[{"protocol":"tcp","ports":"22","sources":{"addresses":["0.0.0.0/0"]}}]}
		]}`), nil
	})}
	client := &Client{client: godo.NewClient(httpClient), controllerTag: "runner-controller-test", firewallID: "firewall-id"}
	if err := client.validateRunnerFirewalls(context.Background(), []string{"runner-controller-test", "runner-job-test"}); err == nil {
		t.Fatal("accepted an additional inbound firewall targeting a runner tag")
	}
}

func doStatusError(status int) error {
	return &godo.ErrorResponse{Response: &http.Response{StatusCode: status}}
}

func TestSweepOrphanedRunnersPreservesKnownAndDeletesOldUnknown(t *testing.T) {
	deletedIDs := make([]string, 0)
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			deletedIDs = append(deletedIDs, request.URL.Path)
			return jsonResponse(http.StatusNoContent, ""), nil
		}
		return jsonResponse(http.StatusOK, `{"droplets":[
			{"id":1,"name":"known","created_at":"2020-01-01T00:00:00Z","tags":["runner-controller-test"]},
			{"id":2,"name":"orphan","created_at":"2020-01-01T00:00:00Z","tags":["runner-controller-test"]}
		]}`), nil
	})}
	client := &Client{client: godo.NewClient(httpClient), controllerTag: "runner-controller-test"}
	deleted, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"1": {}}, time.Now().Add(-time.Hour))
	if err != nil || deleted != 1 || len(deletedIDs) != 1 || deletedIDs[0] != "/v2/droplets/2" {
		t.Fatalf("sweep = %d, %v, deleted=%v", deleted, err, deletedIDs)
	}
}

// createRunnerWithProject runs CreateRunner for droplet 91 with project
// assignment enabled. assign answers the project-resources POST and receives
// the requested resource URNs.
func createRunnerWithProject(t *testing.T, assign func(resources []string) *http.Response) (*compute.RunnerInstance, error) {
	t.Helper()
	params := compute.RunnerParams{JobKey: "org/repo:9", ProvisionEpoch: 1, RunnerName: "display-name"}
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v2/droplets":
			return jsonResponse(http.StatusOK, `{"droplets":[]}`), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v2/firewalls":
			return jsonResponse(http.StatusOK, `{"firewalls":[{"id":"firewall-id","tags":["runner-controller-test"],"inbound_rules":[]}]}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/v2/droplets":
			return jsonResponse(http.StatusAccepted, `{"droplet":{"id":91,"name":"runner"}}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/v2/projects/project-uuid/resources":
			var body struct {
				Resources []string `json:"resources"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			return assign(body.Resources), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}
	client := &Client{
		client: godo.NewClient(httpClient), cloudInitTmpl: template.Must(template.New("cloud-init").Parse("bootstrap")),
		region: "nyc3", size: "s-1vcpu-1gb", image: "ubuntu", controllerTag: "runner-controller-test",
		vpcUUID: "vpc-id", firewallID: "firewall-id", projectID: "project-uuid",
	}
	return client.CreateRunner(context.Background(), params)
}

func TestCreateRunnerAssignsConfiguredProject(t *testing.T) {
	var assigned []string
	_, err := createRunnerWithProject(t, func(resources []string) *http.Response {
		assigned = resources
		return jsonResponse(http.StatusOK, `{"resources":[]}`)
	})
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}
	if len(assigned) != 1 || assigned[0] != "do:droplet:91" {
		t.Fatalf("assigned resources = %v, want [do:droplet:91]", assigned)
	}
}

func TestCreateRunnerSucceedsWhenProjectAssignmentFails(t *testing.T) {
	instance, err := createRunnerWithProject(t, func([]string) *http.Response {
		return jsonResponse(http.StatusForbidden, `{"id":"forbidden","message":"no access"}`)
	})
	if err != nil || instance.ID != "91" {
		t.Fatalf("CreateRunner() = %#v, %v; want droplet 91 despite assignment failure", instance, err)
	}
}

func TestCreateRunnerLogsRejectedProjectAssignment(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	instance, err := createRunnerWithProject(t, func([]string) *http.Response {
		return jsonResponse(http.StatusOK, `{"resources":[{"urn":"do:droplet:91","status":"service_down"}]}`)
	})
	if err != nil || instance.ID != "91" {
		t.Fatalf("CreateRunner() = %#v, %v; want droplet 91 despite rejected assignment", instance, err)
	}
	if !strings.Contains(logs.String(), "do:droplet:91 status service_down") {
		t.Fatalf("log = %q, want the rejected resource status", logs.String())
	}
}

func TestCreateRunnerAppliesSizeOverrideAndRejectsInvalidSize(t *testing.T) {
	var requestedSize string
	httpClient := &http.Client{Transport: doRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v2/droplets":
			return jsonResponse(http.StatusOK, `{"droplets":[]}`), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v2/firewalls":
			return jsonResponse(http.StatusOK, `{"firewalls":[{"id":"firewall-id","tags":["runner-controller-test"],"inbound_rules":[]}]}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/v2/droplets":
			var body struct {
				Size string `json:"size"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			requestedSize = body.Size
			return jsonResponse(http.StatusAccepted, `{"droplet":{"id":92,"name":"runner"}}`), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}
	client := &Client{
		client: godo.NewClient(httpClient), cloudInitTmpl: template.Must(template.New("cloud-init").Parse("bootstrap")),
		region: "nyc3", size: "c-4", image: "ubuntu", controllerTag: "runner-controller-test",
		vpcUUID: "vpc-id", firewallID: "firewall-id",
	}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:10", ProvisionEpoch: 1, InstanceSize: "c-16",
	}); err != nil {
		t.Fatalf("CreateRunner override error = %v", err)
	}
	if requestedSize != "c-16" {
		t.Fatalf("requested size = %q, want c-16", requestedSize)
	}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:11", ProvisionEpoch: 1, InstanceSize: "bad/size",
	}); err == nil {
		t.Fatal("CreateRunner accepted invalid droplet size override")
	}
}

func TestDigitalOceanRejectsSpotConfigAndRequest(t *testing.T) {
	if _, err := NewClient(Config{
		Token: "tok", CloudInitPath: "unused", ControllerID: "primary",
		VPCUUID: "vpc", FirewallID: "fw", Spot: true,
	}); !compute.IsSpotUnsupported(err) {
		t.Fatalf("NewClient(Spot=true) error = %v, want ErrSpotUnsupported", err)
	}
	client := &Client{controllerTag: "runner-controller-test"}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:spot", Spot: compute.BoolPtr(true),
	}); !compute.IsSpotUnsupported(err) {
		t.Fatalf("CreateRunner(Spot=true) error = %v, want ErrSpotUnsupported", err)
	}
}
