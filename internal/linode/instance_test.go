// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package linode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type linodeMock struct {
	mu         sync.Mutex
	token      string
	items      []linodeInstance
	lastCreate createInstanceRequest
	removed    []string
}

func (m *linodeMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+m.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.URL.Path == instancesPath {
		if r.Method == http.MethodPost {
			m.create(w, r)
			return
		}
		m.list(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, instancesBasePath) {
		idStr := strings.TrimPrefix(r.URL.Path, instancesBasePath)
		if r.Method == http.MethodDelete {
			m.remove(w, idStr)
			return
		}
		m.get(w, idStr)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (m *linodeMock) list(w http.ResponseWriter, r *http.Request) {
	var filter map[string]string
	if hdr := r.Header.Get("X-Filter"); hdr != "" {
		if err := json.Unmarshal([]byte(hdr), &filter); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	var out []linodeInstance
	for _, item := range m.items {
		if filter["tags"] == "" || compute.HasTag(item.Tags, filter["tags"]) {
			out = append(out, item)
		}
	}
	_ = json.NewEncoder(w).Encode(listInstancesResponse{Data: out, Page: 1, Pages: 1, Results: len(out)})
}

func (m *linodeMock) create(w http.ResponseWriter, r *http.Request) {
	if err := json.NewDecoder(r.Body).Decode(&m.lastCreate); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	inst := linodeInstance{
		ID:      7001,
		Label:   m.lastCreate.Label,
		Type:    m.lastCreate.Type,
		Region:  m.lastCreate.Region,
		Image:   m.lastCreate.Image,
		Created: time.Now().UTC().Format("2006-01-02T15:04:05"),
		Tags:    m.lastCreate.Tags,
	}
	m.items = append(m.items, inst)
	_ = json.NewEncoder(w).Encode(inst)
}

func (m *linodeMock) get(w http.ResponseWriter, idStr string) {
	id, _ := strconv.Atoi(idStr)
	for _, item := range m.items {
		if item.ID == id {
			_ = json.NewEncoder(w).Encode(item)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"errors":[{"reason":"Not found"}]}`))
}

func (m *linodeMock) remove(w http.ResponseWriter, idStr string) {
	id, _ := strconv.Atoi(idStr)
	m.removed = append(m.removed, idStr)
	kept := m.items[:0]
	for _, item := range m.items {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	m.items = kept
	_, _ = w.Write([]byte(`{}`))
}

func writeLinodeTemplate(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "linode-runner.yaml.tmpl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLinodeConfigValidation(t *testing.T) {
	tmplPath := writeLinodeTemplate(t, "#cloud-config\n{{.RunnerJITConfig}}\n")
	badConfigs := []Config{
		{ControllerID: "primary", CloudInitPath: tmplPath},
		{Token: "t", ControllerID: "bad controller", CloudInitPath: tmplPath},
		{Token: "t", ControllerID: "primary", InstanceType: "bad/type", CloudInitPath: tmplPath},
		{Token: "t", ControllerID: "primary", FirewallID: -1, CloudInitPath: tmplPath},
		{Token: "t", ControllerID: "primary", VPCSubnetID: -2, CloudInitPath: tmplPath},
		{Token: "t", ControllerID: "primary", BaseURL: "ftp://invalid", CloudInitPath: tmplPath},
	}
	for idx, cfg := range badConfigs {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("case %d accepted invalid Linode config", idx)
		}
	}
	client, err := NewClient(Config{
		Token: "secret-token", ControllerID: "primary", CloudInitPath: tmplPath,
		FirewallID: 101, VPCSubnetID: 202,
	})
	if err != nil {
		t.Fatalf("valid NewClient error = %v", err)
	}
	if client.Provider() != "linode" || client.region != defaultRegion || client.instanceType != defaultInstanceType || client.image != defaultImage {
		t.Fatalf("unexpected defaults: region=%q type=%q image=%q", client.region, client.instanceType, client.image)
	}
}

func newMockLinodeClient(t *testing.T, mock *linodeMock) (*Client, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(mock)
	tmplPath := writeLinodeTemplate(t, "#cloud-config\njit={{.RunnerJITConfig}}\ndeadman={{.DeadmanShutdownMinutes}}\n")
	client, err := NewClient(Config{
		Token: mock.token, Region: "us-east", InstanceType: "g6-standard-4",
		FirewallID: 55, VPCSubnetID: 88, AuthorizedKeys: []string{"ssh-ed25519 AAAA"},
		CloudInitPath: tmplPath, ControllerID: "primary", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return client, srv.Close
}

func TestLinodeCreateAndHealth(t *testing.T) {
	mock := &linodeMock{token: "test-linode-token"}
	client, cleanup := newMockLinodeClient(t, mock)
	defer cleanup()

	ctx := context.Background()
	if err := client.Health(ctx); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if _, err := client.CreateRunner(ctx, compute.RunnerParams{JobKey: "org/repo:bad", InstanceSize: "bad/type"}); err == nil {
		t.Fatal("CreateRunner accepted invalid size override")
	}

	created, err := client.CreateRunner(ctx, compute.RunnerParams{
		JobKey: "org/repo:42", ProvisionEpoch: 1, RunnerName: "eph-repo-42",
		RunnerJITConfig: "encoded-jit", InstanceSize: "g6-dedicated-8", DeadmanShutdownMinutes: 180,
	})
	if err != nil || created.ID != "7001" {
		t.Fatalf("CreateRunner() = %#v, %v", created, err)
	}
	assertLinodeCreatePayload(t, mock.lastCreate)
}

func TestLinodeOwnershipDeleteAndAbsence(t *testing.T) {
	jobKey := "org/repo:42"
	mock := &linodeMock{
		token: "test-linode-token",
		items: []linodeInstance{{
			ID: 7001, Label: "ghr-existing",
			Tags: []string{"runner-controller-primary", compute.JobTag(jobKey)},
		}},
	}
	client, cleanup := newMockLinodeClient(t, mock)
	defer cleanup()

	ctx := context.Background()
	foundInst, found, err := client.FindRunner(ctx, jobKey)
	if err != nil || !found || foundInst.ID != "7001" {
		t.Fatalf("FindRunner() = %#v, %v, %v", foundInst, found, err)
	}
	if absent, err := client.RunnerAbsent(ctx, "7001"); err != nil || absent {
		t.Fatalf("RunnerAbsent(existing) = %v, %v", absent, err)
	}
	if err := client.DeleteRunner(ctx, "7001", "org/repo:other"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("wrong-job delete error = %v", err)
	}
	if err := client.DeleteRunner(ctx, "7001", jobKey); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	if absent, err := client.RunnerAbsent(ctx, "7001"); err != nil || !absent {
		t.Fatalf("RunnerAbsent(deleted) = %v, %v", absent, err)
	}
}

func assertLinodeCreatePayload(t *testing.T, req createInstanceRequest) {
	t.Helper()
	if req.Type != "g6-dedicated-8" || req.FirewallID != 55 || req.InterfaceGeneration != "legacy_config" || len(req.Interfaces) != 1 || req.Interfaces[0].SubnetID != 88 || req.Interfaces[0].IPv4 == nil || req.Interfaces[0].IPv4.NAT != "any" {
		t.Fatalf("unexpected create payload: %#v", req)
	}
	if len(req.RootPass) < 20 || req.Metadata == nil {
		t.Fatalf("missing password or metadata: passLen=%d meta=%v", len(req.RootPass), req.Metadata)
	}
	decoded, err := base64.StdEncoding.DecodeString(req.Metadata.UserData)
	if err != nil || !strings.Contains(string(decoded), "jit=encoded-jit") || !strings.Contains(string(decoded), "deadman=180") {
		t.Fatalf("decoded user_data = %q, %v", string(decoded), err)
	}
}

func TestLinodePageSweepAndTimestamps(t *testing.T) {
	oldLinodeTS := time.Now().Add(-3 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	oldRFC3339 := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	freshTS := time.Now().Add(-10 * time.Minute).UTC().Format("2006-01-02T15:04:05")
	var deleted, pages []string

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, instancesBasePath))
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, instancesBasePath) {
			var tags []string
			if strings.HasSuffix(r.URL.Path, "/14") {
				tags = []string{"runner-controller-primary"}
			}
			_ = json.NewEncoder(w).Encode(linodeInstance{Tags: tags})
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if page == "1" {
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{
					{ID: 10, Created: oldLinodeTS, Tags: []string{"runner-controller-primary"}},
					{ID: 11, Created: "bad-timestamp", Tags: []string{"runner-controller-primary"}},
					{ID: 12, Created: oldLinodeTS, Tags: []string{"runner-controller-primary"}},
				},
				Page: 1, Pages: 2, Results: 5,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(listInstancesResponse{
			Data: []linodeInstance{
				{ID: 13, Created: freshTS, Tags: []string{"runner-controller-primary"}},
				{ID: 14, Created: oldRFC3339, Tags: []string{"runner-controller-primary"}},
			},
			Page: 2, Pages: 2, Results: 5,
		})
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		Token: "test-token", ControllerID: "primary", CloudInitPath: writeLinodeTemplate(t, "#cloud-config\n"),
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"10": {}}, time.Now().Add(-time.Hour))
	if err == nil || !errors.Is(err, compute.ErrOwnershipMismatch) || count != 1 || strings.Join(deleted, ",") != "14" || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("sweep count=%d err=%v deleted=%v pages=%v", count, err, deleted, pages)
	}
}

func TestLinodeAmbiguousCreateAndOwnershipGuards(t *testing.T) {
	params := compute.RunnerParams{JobKey: "org/repo:88", ProvisionEpoch: 3}
	wantName := compute.DeterministicName("linode", params.JobKey, params.ProvisionEpoch)
	state := "empty"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			state = "created"
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"Service unavailable"}]}`))
			return
		}
		switch state {
		case "empty":
			_ = json.NewEncoder(w).Encode(listInstancesResponse{Page: 1, Pages: 1})
		case "created":
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{{ID: 999, Label: wantName, Tags: []string{"runner-controller-primary", compute.JobTag(params.JobKey)}}},
				Page: 1, Pages: 1, Results: 1,
			})
		case "foreign":
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{{ID: 1, Tags: []string{"runner-controller-other"}}},
				Page: 1, Pages: 1,
			})
		case "dup":
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{
					{ID: 1, Tags: []string{"runner-controller-primary"}},
					{ID: 2, Tags: []string{"runner-controller-primary"}},
				},
				Page: 1, Pages: 1,
			})
		}
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		Token: "test-token", ControllerID: "primary", CloudInitPath: writeLinodeTemplate(t, "#cloud-config\n"),
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.recoveryTimeout = 200 * time.Millisecond
	client.recoveryPoll = time.Millisecond

	inst, err := client.CreateRunner(context.Background(), params)
	if err != nil || inst.ID != "999" || inst.Name != wantName {
		t.Fatalf("recovered = %#v, %v", inst, err)
	}

	state = "foreign"
	if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("foreign FindRunner error = %v", err)
	}
	state = "dup"
	if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); !errors.Is(err, compute.ErrDuplicateInstances) {
		t.Fatalf("duplicate FindRunner error = %v", err)
	}
}

func TestLinodeRejectsSpotConfigAndRequest(t *testing.T) {
	if _, err := NewClient(Config{
		Token: "tok", ControllerID: "primary", CloudInitPath: writeLinodeTemplate(t, "#cloud-config\n"), Spot: true,
	}); !compute.IsSpotUnsupported(err) {
		t.Fatalf("NewClient(Spot=true) error = %v, want ErrSpotUnsupported", err)
	}
	client, err := NewClient(Config{
		Token: "tok", ControllerID: "primary", CloudInitPath: writeLinodeTemplate(t, "#cloud-config\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:spot", Spot: compute.BoolPtr(true),
	}); !compute.IsSpotUnsupported(err) {
		t.Fatalf("CreateRunner(Spot=true) error = %v, want ErrSpotUnsupported", err)
	}
}

func TestFirewallWithoutSubnetUsesLegacyInterfaces(t *testing.T) {
	c := Client{firewallID: 55}
	req := c.buildCreateRequest("runner", "g6-standard-4", "org/repo:1", "fixture", "")
	if req.FirewallID != 55 || req.InterfaceGeneration != "legacy_config" || len(req.Interfaces) != 0 {
		t.Fatal("firewall-only request must select legacy interfaces")
	}
	c.firewallID = 0
	req = c.buildCreateRequest("runner", "g6-standard-4", "org/repo:1", "fixture", "")
	if req.InterfaceGeneration != "" {
		t.Fatal("unexpected interface override without firewall or VPC")
	}
}
