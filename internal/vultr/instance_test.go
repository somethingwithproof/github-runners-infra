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

func writeTemplate(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.yaml.tmpl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewClientValidation(t *testing.T) {
	tmplPath := writeTemplate(t, "#cloud-config\n{{.RunnerJITConfig}}\n")
	if _, err := NewClient(Config{ControllerID: "primary", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted missing API key")
	}
	if _, err := NewClient(Config{APIKey: "k", ControllerID: "bad controller", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted invalid controller ID")
	}
	if _, err := NewClient(Config{APIKey: "k", ControllerID: "primary", Plan: "bad/plan", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted invalid plan")
	}
	if _, err := NewClient(Config{APIKey: "k", ControllerID: "primary", BaseURL: "ftp://invalid", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted invalid base URL")
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

func TestCreateFindDeleteAndSizeOverride(t *testing.T) {
	var mu sync.Mutex
	var createdReq createInstanceRequest
	var instances []vultrInstance
	var deletedIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-vultr-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instances":
			tag := r.URL.Query().Get("tag")
			var matched []vultrInstance
			for _, inst := range instances {
				if hasTag(inst.Tags, tag) {
					matched = append(matched, inst)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"instances": matched, "meta": map[string]any{"total": len(matched)}})
		case r.Method == http.MethodPost && r.URL.Path == "/instances":
			if err := json.NewDecoder(r.Body).Decode(&createdReq); err != nil {
				t.Errorf("decode create body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			inst := vultrInstance{
				ID:          "inst-uuid-1",
				Label:       createdReq.Label,
				Plan:        createdReq.Plan,
				Region:      createdReq.Region,
				DateCreated: time.Now().UTC().Format(time.RFC3339),
				Tags:        createdReq.Tags,
			}
			instances = append(instances, inst)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"instance": inst})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/instances/"):
			id := strings.TrimPrefix(r.URL.Path, "/instances/")
			for _, inst := range instances {
				if inst.ID == id {
					_ = json.NewEncoder(w).Encode(map[string]any{"instance": inst})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/instances/"):
			id := strings.TrimPrefix(r.URL.Path, "/instances/")
			deletedIDs = append(deletedIDs, id)
			remaining := instances[:0]
			for _, inst := range instances {
				if inst.ID != id {
					remaining = append(remaining, inst)
				}
			}
			instances = remaining
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tmplPath := writeTemplate(t, "#cloud-config\njit={{.RunnerJITConfig}}\ndeadman={{.DeadmanShutdownMinutes}}\n")
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
	if createdReq.Plan != "vhf-8c-16gb" || createdReq.FirewallGroupID != "fw-deny-in" || len(createdReq.AttachVPC) != 1 || createdReq.AttachVPC[0] != "vpc-private" {
		t.Fatalf("unexpected create payload: %#v", createdReq)
	}
	decodedUD, err := base64.StdEncoding.DecodeString(createdReq.UserData)
	if err != nil || !strings.Contains(string(decodedUD), "jit=encoded-jit") || !strings.Contains(string(decodedUD), "deadman=180") {
		t.Fatalf("decoded user_data = %q, %v", string(decodedUD), err)
	}

	// Idempotent FindRunner and repeat CreateRunner return the existing instance.
	foundInst, found, err := client.FindRunner(context.Background(), params.JobKey)
	if err != nil || !found || foundInst.ID != "inst-uuid-1" {
		t.Fatalf("FindRunner() = %#v, %v, %v", foundInst, found, err)
	}
	absent, err := client.RunnerAbsent(context.Background(), "inst-uuid-1")
	if err != nil || absent {
		t.Fatalf("RunnerAbsent(existing) = %v, %v", absent, err)
	}

	// Ownership mismatch refuses deletion.
	if err := client.DeleteRunner(context.Background(), "inst-uuid-1", "org/repo:other"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("wrong-job delete error = %v", err)
	}

	// Matching ownership deletes cleanly and confirms absence.
	if err := client.DeleteRunner(context.Background(), "inst-uuid-1", params.JobKey); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	absent, err = client.RunnerAbsent(context.Background(), "inst-uuid-1")
	if err != nil || !absent {
		t.Fatalf("RunnerAbsent(deleted) = %v, %v", absent, err)
	}
	if len(deletedIDs) != 1 || deletedIDs[0] != "inst-uuid-1" {
		t.Fatalf("deletedIDs = %v", deletedIDs)
	}

	// Invalid override size is rejected before calling API.
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:bad", InstanceSize: "bad/plan",
	}); err == nil {
		t.Fatal("CreateRunner accepted invalid size override")
	}
}

func TestSweepOrphanedRunnersPaginationAndUndatedHandling(t *testing.T) {
	oldTime := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	freshTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	var deleted []string
	var cursors []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
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
		case http.MethodDelete:
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/instances/"))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	tmplPath := writeTemplate(t, "#cloud-config\n")
	client, err := NewClient(Config{
		APIKey: "test-key", ControllerID: "primary", CloudInitPath: tmplPath,
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"known-old": {}}, time.Now().Add(-time.Hour))
	if err == nil {
		t.Fatal("expected parse error for undated instance")
	}
	if count != 1 || len(deleted) != 1 || deleted[0] != "orphan-old" {
		t.Fatalf("sweep deleted=%d ids=%v, want [orphan-old]", count, deleted)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "cursor-2" {
		t.Fatalf("cursors = %v", cursors)
	}
}

func TestAmbiguousCreateRecoversDeterministicInstance(t *testing.T) {
	params := compute.RunnerParams{JobKey: "org/repo:77", ProvisionEpoch: 2}
	wantName := instanceName(params.JobKey, params.ProvisionEpoch)
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instances":
			listCalls++
			if listCalls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"instances": []vultrInstance{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"instances": []vultrInstance{{
				ID:    "recovered-1",
				Label: wantName,
				Tags:  []string{"runner-controller-primary", runnerJobTag(params.JobKey)},
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/instances":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream timeout"}`))
		}
	}))
	defer srv.Close()

	tmplPath := writeTemplate(t, "#cloud-config\n")
	client, err := NewClient(Config{
		APIKey: "test-key", ControllerID: "primary", CloudInitPath: tmplPath,
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
}

func TestFindRunnerRejectsForeignAndDuplicateInstances(t *testing.T) {
	t.Run("foreign controller", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"instances": []vultrInstance{{
				ID: "foreign-1", Tags: []string{"runner-controller-other"},
			}}})
		}))
		defer srv.Close()
		client, err := NewClient(Config{
			APIKey: "k", ControllerID: "primary", CloudInitPath: writeTemplate(t, "#cloud-config\n"),
			BaseURL: srv.URL, HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); !errors.Is(err, compute.ErrOwnershipMismatch) {
			t.Fatalf("foreign error = %v", err)
		}
	})

	t.Run("duplicates", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"instances": []vultrInstance{
				{ID: "dup-1", Tags: []string{"runner-controller-primary"}},
				{ID: "dup-2", Tags: []string{"runner-controller-primary"}},
			}})
		}))
		defer srv.Close()
		client, err := NewClient(Config{
			APIKey: "k", ControllerID: "primary", CloudInitPath: writeTemplate(t, "#cloud-config\n"),
			BaseURL: srv.URL, HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); !errors.Is(err, compute.ErrDuplicateInstances) {
			t.Fatalf("duplicate error = %v", err)
		}
	})
}
