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
		t.Fatal("accepted missing token")
	}
	if _, err := NewClient(Config{Token: "t", ControllerID: "bad controller", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted invalid controller ID")
	}
	if _, err := NewClient(Config{Token: "t", ControllerID: "primary", InstanceType: "bad/type", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted invalid instance type")
	}
	if _, err := NewClient(Config{Token: "t", ControllerID: "primary", FirewallID: -1, CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted negative firewall ID")
	}
	if _, err := NewClient(Config{Token: "t", ControllerID: "primary", BaseURL: "ftp://invalid", CloudInitPath: tmplPath}); err == nil {
		t.Fatal("accepted invalid base URL")
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

func TestCreateFindDeleteAndSizeOverride(t *testing.T) {
	var mu sync.Mutex
	var createdReq createInstanceRequest
	var instances []linodeInstance
	var deletedIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-linode-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/linode/instances":
			var filter map[string]string
			if hdr := r.Header.Get("X-Filter"); hdr != "" {
				if err := json.Unmarshal([]byte(hdr), &filter); err != nil {
					t.Errorf("invalid X-Filter header %q: %v", hdr, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			var matched []linodeInstance
			for _, inst := range instances {
				if filter["tags"] == "" || hasTag(inst.Tags, filter["tags"]) {
					matched = append(matched, inst)
				}
			}
			_ = json.NewEncoder(w).Encode(listInstancesResponse{Data: matched, Page: 1, Pages: 1, Results: len(matched)})
		case r.Method == http.MethodPost && r.URL.Path == "/linode/instances":
			if err := json.NewDecoder(r.Body).Decode(&createdReq); err != nil {
				t.Errorf("decode create body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			inst := linodeInstance{
				ID:      7001,
				Label:   createdReq.Label,
				Type:    createdReq.Type,
				Region:  createdReq.Region,
				Image:   createdReq.Image,
				Created: time.Now().UTC().Format("2006-01-02T15:04:05"),
				Tags:    createdReq.Tags,
			}
			instances = append(instances, inst)
			_ = json.NewEncoder(w).Encode(inst)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/linode/instances/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/linode/instances/")
			id, _ := strconv.Atoi(idStr)
			for _, inst := range instances {
				if inst.ID == id {
					_ = json.NewEncoder(w).Encode(inst)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"Not found"}]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/linode/instances/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/linode/instances/")
			id, _ := strconv.Atoi(idStr)
			deletedIDs = append(deletedIDs, idStr)
			remaining := instances[:0]
			for _, inst := range instances {
				if inst.ID != id {
					remaining = append(remaining, inst)
				}
			}
			instances = remaining
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tmplPath := writeTemplate(t, "#cloud-config\njit={{.RunnerJITConfig}}\ndeadman={{.DeadmanShutdownMinutes}}\n")
	client, err := NewClient(Config{
		Token: "test-linode-token", Region: "us-east", InstanceType: "g6-standard-4",
		FirewallID: 55, VPCSubnetID: 88, AuthorizedKeys: []string{"ssh-ed25519 AAAA"},
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
		InstanceSize:           "g6-dedicated-8",
		DeadmanShutdownMinutes: 180,
	}
	created, err := client.CreateRunner(context.Background(), params)
	if err != nil || created.ID != "7001" {
		t.Fatalf("CreateRunner() = %#v, %v", created, err)
	}
	if createdReq.Type != "g6-dedicated-8" || createdReq.FirewallID != 55 || len(createdReq.Interfaces) != 1 || createdReq.Interfaces[0].SubnetID != 88 {
		t.Fatalf("unexpected create payload: %#v", createdReq)
	}
	if len(createdReq.RootPass) < 20 {
		t.Fatalf("root_pass too short: %d", len(createdReq.RootPass))
	}
	if createdReq.Metadata == nil {
		t.Fatal("missing Linode metadata")
	}
	decodedUD, err := base64.StdEncoding.DecodeString(createdReq.Metadata.UserData)
	if err != nil || !strings.Contains(string(decodedUD), "jit=encoded-jit") || !strings.Contains(string(decodedUD), "deadman=180") {
		t.Fatalf("decoded user_data = %q, %v", string(decodedUD), err)
	}

	foundInst, found, err := client.FindRunner(context.Background(), params.JobKey)
	if err != nil || !found || foundInst.ID != "7001" {
		t.Fatalf("FindRunner() = %#v, %v, %v", foundInst, found, err)
	}
	absent, err := client.RunnerAbsent(context.Background(), "7001")
	if err != nil || absent {
		t.Fatalf("RunnerAbsent(existing) = %v, %v", absent, err)
	}

	if err := client.DeleteRunner(context.Background(), "7001", "org/repo:other"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("wrong-job delete error = %v", err)
	}

	if err := client.DeleteRunner(context.Background(), "7001", params.JobKey); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	absent, err = client.RunnerAbsent(context.Background(), "7001")
	if err != nil || !absent {
		t.Fatalf("RunnerAbsent(deleted) = %v, %v", absent, err)
	}
	if len(deletedIDs) != 1 || deletedIDs[0] != "7001" {
		t.Fatalf("deletedIDs = %v", deletedIDs)
	}

	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:bad", InstanceSize: "bad/type",
	}); err == nil {
		t.Fatal("CreateRunner accepted invalid size override")
	}
}

func TestSweepOrphanedRunnersPaginationAndLinodeTimestamps(t *testing.T) {
	oldLinodeTS := time.Now().Add(-3 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	oldRFC3339 := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	freshTS := time.Now().Add(-10 * time.Minute).UTC().Format("2006-01-02T15:04:05")
	var deleted []string
	var pages []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
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
		case http.MethodDelete:
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/linode/instances/"))
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		Token: "test-token", ControllerID: "primary", CloudInitPath: writeTemplate(t, "#cloud-config\n"),
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"10": {}}, time.Now().Add(-time.Hour))
	if err == nil {
		t.Fatal("expected parse error for undated instance")
	}
	if count != 2 || strings.Join(deleted, ",") != "12,14" {
		t.Fatalf("sweep deleted=%d ids=%v, want [12 14]", count, deleted)
	}
	if strings.Join(pages, ",") != "1,2" {
		t.Fatalf("pages = %v", pages)
	}
}

func TestAmbiguousCreateRecoversDeterministicInstance(t *testing.T) {
	params := compute.RunnerParams{JobKey: "org/repo:88", ProvisionEpoch: 3}
	wantName := instanceName(params.JobKey, params.ProvisionEpoch)
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/linode/instances":
			listCalls++
			if listCalls == 1 {
				_ = json.NewEncoder(w).Encode(listInstancesResponse{Page: 1, Pages: 1})
				return
			}
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{{
					ID:    999,
					Label: wantName,
					Tags:  []string{"runner-controller-primary", runnerJobTag(params.JobKey)},
				}},
				Page: 1, Pages: 1, Results: 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/linode/instances":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"Service unavailable"}]}`))
		}
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		Token: "test-token", ControllerID: "primary", CloudInitPath: writeTemplate(t, "#cloud-config\n"),
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
}

func TestFindRunnerRejectsForeignAndDuplicateInstances(t *testing.T) {
	t.Run("foreign controller", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{{ID: 1, Tags: []string{"runner-controller-other"}}},
				Page: 1, Pages: 1,
			})
		}))
		defer srv.Close()
		client, err := NewClient(Config{
			Token: "k", ControllerID: "primary", CloudInitPath: writeTemplate(t, "#cloud-config\n"),
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
			_ = json.NewEncoder(w).Encode(listInstancesResponse{
				Data: []linodeInstance{
					{ID: 1, Tags: []string{"runner-controller-primary"}},
					{ID: 2, Tags: []string{"runner-controller-primary"}},
				},
				Page: 1, Pages: 1,
			})
		}))
		defer srv.Close()
		client, err := NewClient(Config{
			Token: "k", ControllerID: "primary", CloudInitPath: writeTemplate(t, "#cloud-config\n"),
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
