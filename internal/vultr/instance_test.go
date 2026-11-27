// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package vultr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type vultrMock struct {
	mu         sync.Mutex
	token      string
	instances  []vultrInstance
	createdReq createInstanceRequest
	deletedIDs []string
}

func (m *vultrMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+m.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == instancesPath:
		m.handleList(w, r)
	case r.Method == http.MethodPost && r.URL.Path == instancesPath:
		m.handleCreate(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, instancesBasePath):
		m.handleGet(w, strings.TrimPrefix(r.URL.Path, instancesBasePath))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, instancesBasePath):
		m.handleDelete(w, strings.TrimPrefix(r.URL.Path, instancesBasePath))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (m *vultrMock) handleList(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Query().Get("tag")
	var matched []vultrInstance
	for _, inst := range m.instances {
		if compute.HasTag(inst.Tags, tag) {
			matched = append(matched, inst)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"instances": matched, "meta": map[string]any{"total": len(matched)}})
}

func (m *vultrMock) handleCreate(w http.ResponseWriter, r *http.Request) {
	if err := json.NewDecoder(r.Body).Decode(&m.createdReq); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	inst := vultrInstance{
		ID:          "inst-uuid-1",
		Label:       m.createdReq.Label,
		Plan:        m.createdReq.Plan,
		Region:      m.createdReq.Region,
		DateCreated: time.Now().UTC().Format(time.RFC3339),
		Tags:        m.createdReq.Tags,
	}
	m.instances = append(m.instances, inst)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"instance": inst})
}

func (m *vultrMock) handleGet(w http.ResponseWriter, id string) {
	for _, inst := range m.instances {
		if inst.ID == id {
			_ = json.NewEncoder(w).Encode(map[string]any{"instance": inst})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

func (m *vultrMock) handleDelete(w http.ResponseWriter, id string) {
	m.deletedIDs = append(m.deletedIDs, id)
	remaining := m.instances[:0]
	for _, inst := range m.instances {
		if inst.ID != id {
			remaining = append(remaining, inst)
		}
	}
	m.instances = remaining
	w.WriteHeader(http.StatusNoContent)
}

func writeVultrTemplate(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vultr-runner.yaml.tmpl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVultrConfigValidation(t *testing.T) {
	tmplPath := writeVultrTemplate(t, "#cloud-config\n{{.RunnerJITConfig}}\n")
	invalidConfigs := []Config{
		{ControllerID: "primary", CloudInitPath: tmplPath},
		{APIKey: "k", ControllerID: "bad id", CloudInitPath: tmplPath},
		{APIKey: "k", ControllerID: "primary", Plan: "bad/plan", CloudInitPath: tmplPath},
		{APIKey: "k", ControllerID: "primary", OSID: -5, CloudInitPath: tmplPath},
		{APIKey: "k", ControllerID: "primary", BaseURL: "ftp://invalid", CloudInitPath: tmplPath},
	}
	for i, cfg := range invalidConfigs {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("case %d accepted invalid Vultr config", i)
		}
	}
	client, err := NewClient(Config{
		APIKey: "secret-key", ControllerID: "primary", CloudInitPath: tmplPath,
		FirewallGroupID: "fw-1", VPCID: "vpc-1", SSHKeyIDs: []string{"ssh-1"},
	})
	if err != nil {
		t.Fatalf("valid NewClient error = %v", err)
	}
	if client.Provider() != "vultr" || client.region != defaultRegion || client.plan != defaultPlan || client.osID != defaultOSID {
		t.Fatalf("unexpected defaults: region=%q plan=%q osID=%d", client.region, client.plan, client.osID)
	}
}

func TestVultrLifecycleAndSizeOverride(t *testing.T) {
	mock := &vultrMock{token: "test-vultr-key"}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	tmplPath := writeVultrTemplate(t, "#cloud-config\njit={{.RunnerJITConfig}}\ndeadman={{.DeadmanShutdownMinutes}}\n")
	client, err := NewClient(Config{
		APIKey: "test-vultr-key", Region: "ewr", Plan: "vc2-4c-8gb",
		FirewallGroupID: "fw-deny-in", VPCID: "vpc-private", SSHKeyIDs: []string{"ssh-key-1"},
		CloudInitPath: tmplPath, ControllerID: "primary", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	params := compute.RunnerParams{
		JobKey:                 "org/repo:42",
		ProvisionEpoch:         1,
		RunnerName:             "eph-repo-42",
		RunnerJITConfig:        "encoded-jit",
		InstanceSize:           "vhf-8c-16gb",
		DeadmanShutdownMinutes: 180,
	}
	created, err := client.CreateRunner(context.Background(), params)
	if err != nil || created.ID != "inst-uuid-1" {
		t.Fatalf("CreateRunner() = %#v, %v", created, err)
	}
	assertVultrCreatePayload(t, mock.createdReq)

	foundInst, found, err := client.FindRunner(context.Background(), params.JobKey)
	if err != nil || !found || foundInst.ID != "inst-uuid-1" {
		t.Fatalf("FindRunner() = %#v, %v, %v", foundInst, found, err)
	}
	if absent, err := client.RunnerAbsent(context.Background(), "inst-uuid-1"); err != nil || absent {
		t.Fatalf("RunnerAbsent(existing) = %v, %v", absent, err)
	}
	if err := client.DeleteRunner(context.Background(), "inst-uuid-1", "org/repo:other"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("wrong-job delete error = %v", err)
	}
	if err := client.DeleteRunner(context.Background(), "inst-uuid-1", params.JobKey); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	if absent, err := client.RunnerAbsent(context.Background(), "inst-uuid-1"); err != nil || !absent {
		t.Fatalf("RunnerAbsent(deleted) = %v, %v", absent, err)
	}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:bad", InstanceSize: "bad/plan"}); err == nil {
		t.Fatal("CreateRunner accepted invalid size override")
	}
}

func assertVultrCreatePayload(t *testing.T, req createInstanceRequest) {
	t.Helper()
	if req.Plan != "vhf-8c-16gb" || req.FirewallGroupID != "fw-deny-in" || len(req.AttachVPC) != 1 || req.AttachVPC[0] != "vpc-private" {
		t.Fatalf("unexpected create payload: %#v", req)
	}
	decoded, err := base64.StdEncoding.DecodeString(req.UserData)
	if err != nil || !strings.Contains(string(decoded), "jit=encoded-jit") || !strings.Contains(string(decoded), "deadman=180") {
		t.Fatalf("decoded user_data = %q, %v", string(decoded), err)
	}
}

func TestVultrCursorSweepAndUndatedTolerance(t *testing.T) {
	oldTime := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	freshTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	var deleted, cursors []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, instancesBasePath))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		if cursor == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"instances": []vultrInstance{
					{ID: "known-old", DateCreated: oldTime, Tags: []string{"runner-controller-primary"}},
					{ID: "undated", DateCreated: "not-a-timestamp", Tags: []string{"runner-controller-primary"}},
				},
				"meta": map[string]any{"links": map[string]string{"next": "cursor-2"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"instances": []vultrInstance{
				{ID: "fresh-unknown", DateCreated: freshTime, Tags: []string{"runner-controller-primary"}},
				{ID: "orphan-old", DateCreated: oldTime, Tags: []string{"runner-controller-primary"}},
			},
			"meta": map[string]any{"links": map[string]string{"next": ""}},
		})
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		APIKey: "test-key", ControllerID: "primary", CloudInitPath: writeVultrTemplate(t, "#cloud-config\n"),
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"known-old": {}}, time.Now().Add(-time.Hour))
	if err == nil || count != 1 || len(deleted) != 1 || deleted[0] != "orphan-old" || len(cursors) != 2 {
		t.Fatalf("sweep count=%d err=%v deleted=%v cursors=%v", count, err, deleted, cursors)
	}
}

func TestVultrAmbiguousCreateAndOwnershipGuards(t *testing.T) {
	params := compute.RunnerParams{JobKey: "org/repo:77", ProvisionEpoch: 2}
	wantName := compute.DeterministicName("vultr", params.JobKey, params.ProvisionEpoch)
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream timeout"}`))
			return
		}
		listCalls++
		if listCalls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"instances": []vultrInstance{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": []vultrInstance{{
			ID: "recovered-1", Label: wantName, Tags: []string{"runner-controller-primary", compute.JobTag(params.JobKey)},
		}}})
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		APIKey: "test-key", ControllerID: "primary", CloudInitPath: writeVultrTemplate(t, "#cloud-config\n"),
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.recoveryTimeout = 200 * time.Millisecond
	client.recoveryPoll = time.Millisecond

	inst, err := client.CreateRunner(context.Background(), params)
	if err != nil || inst.ID != "recovered-1" || inst.Name != wantName {
		t.Fatalf("recovered = %#v, %v", inst, err)
	}

	for _, tc := range []struct {
		name      string
		instances []vultrInstance
		wantErr   error
	}{
		{"foreign", []vultrInstance{{ID: "f-1", Tags: []string{"runner-controller-other"}}}, compute.ErrOwnershipMismatch},
		{"duplicate", []vultrInstance{
			{ID: "d-1", Tags: []string{"runner-controller-primary"}},
			{ID: "d-2", Tags: []string{"runner-controller-primary"}},
		}, compute.ErrDuplicateInstances},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guardSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"instances": tc.instances})
			}))
			defer guardSrv.Close()
			c, err := NewClient(Config{
				APIKey: "k", ControllerID: "primary", CloudInitPath: writeVultrTemplate(t, "#cloud-config\n"),
				BaseURL: guardSrv.URL, HTTPClient: guardSrv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.FindRunner(context.Background(), "org/repo:1"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("FindRunner error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestVultrRejectsSpotConfigAndRequest(t *testing.T) {
	if _, err := NewClient(Config{
		APIKey: "k", ControllerID: "primary", CloudInitPath: writeVultrTemplate(t, "#cloud-config\n"), Spot: true,
	}); !compute.IsSpotUnsupported(err) {
		t.Fatalf("NewClient(Spot=true) error = %v, want ErrSpotUnsupported", err)
	}
	client, err := NewClient(Config{
		APIKey: "k", ControllerID: "primary", CloudInitPath: writeVultrTemplate(t, "#cloud-config\n"),
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
