// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package oci

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

func generateTestRSAKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test RSA key: %v", err)
	}
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return key, string(pem.EncodeToMemory(block))
}

func writeOCITemplate(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.yaml.tmpl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var sigParamRegex = regexp.MustCompile(`([a-zA-Z]+)="([^"]*)"`)

func verifyOCISignature(pub *rsa.PublicKey, r *http.Request, body []byte, wantKeyID string) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Signature ") {
		return fmt.Errorf("missing OCI Signature Authorization header: %q", auth)
	}
	params := make(map[string]string)
	for _, match := range sigParamRegex.FindAllStringSubmatch(auth, -1) {
		params[match[1]] = match[2]
	}
	if params["version"] != "1" || params["algorithm"] != "rsa-sha256" || params["keyId"] != wantKeyID {
		return fmt.Errorf("unexpected signature params: %#v", params)
	}
	headerNames := strings.Fields(params["headers"])
	wantHeaders := []string{"(request-target)", "date", "host"}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		wantHeaders = append(wantHeaders, "x-content-sha256", "content-type", "content-length")
		digest := sha256.Sum256(body)
		wantSHA := base64.StdEncoding.EncodeToString(digest[:])
		if got := r.Header.Get("x-content-sha256"); got != wantSHA {
			return fmt.Errorf("x-content-sha256 = %q, want %q", got, wantSHA)
		}
	}
	if strings.Join(headerNames, " ") != strings.Join(wantHeaders, " ") {
		return fmt.Errorf("signed headers = %v, want %v", headerNames, wantHeaders)
	}

	var lines []string
	for _, name := range headerNames {
		switch name {
		case "(request-target)":
			lines = append(lines, "(request-target): "+strings.ToLower(r.Method)+" "+r.URL.RequestURI())
		case "host":
			lines = append(lines, "host: "+r.Host)
		case "content-length":
			lines = append(lines, "content-length: "+strconv.Itoa(len(body)))
		default:
			lines = append(lines, name+": "+r.Header.Get(name))
		}
	}
	signingString := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(signingString))
	sigBytes, err := base64.StdEncoding.DecodeString(params["signature"])
	if err != nil {
		return fmt.Errorf("decode signature: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sigBytes); err != nil {
		return fmt.Errorf("RSA-SHA256 signature verification failed for %s %s: %v\nsigning string:\n%s", r.Method, r.URL.RequestURI(), err, signingString)
	}
	return nil
}

func baseTestConfig(t *testing.T, srv *httptest.Server, pemKey string) Config {
	t.Helper()
	return Config{
		TenancyOCID:        "ocid1.tenancy.oc1..tenancy",
		UserOCID:           "ocid1.user.oc1..user",
		Fingerprint:        "aa:bb:cc:dd:ee:ff",
		PrivateKeyPEM:      pemKey,
		Region:             "us-ashburn-1",
		CompartmentOCID:    "ocid1.compartment.oc1..comp",
		SubnetOCID:         "ocid1.subnet.oc1..sub",
		AvailabilityDomain: "Uocm:US-ASHBURN-AD-1",
		ImageOCID:          "ocid1.image.oc1..img",
		Shape:              "VM.Standard.E4.Flex",
		OCPUs:              4,
		MemoryGB:           16,
		CloudInitPath:      writeOCITemplate(t, "#cloud-config\njit={{.RunnerJITConfig}}\nspot={{.SpotActive}}"),
		ControllerID:       "primary",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
	}
}

func TestOCISignatureFlexPreemptibleLifecycleAndPagination(t *testing.T) {
	privKey, pemKey := generateTestRSAKey(t)
	wantKeyID := "ocid1.tenancy.oc1..tenancy/ocid1.user.oc1..user/aa:bb:cc:dd:ee:ff"

	var mu sync.Mutex
	var lastLaunch launchInstanceRequest
	var lastRetryToken string
	var deletedPaths []string
	instances := make(map[string]ociInstance)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "body read failed", 400)
			return
		}
		if err := verifyOCISignature(&privKey.PublicKey, r, body, wantKeyID); err != nil {
			t.Error(err)
			http.Error(w, "invalid signature", 400)
			return
		}

		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == instancesPath:
			var list []ociInstance
			for _, inst := range instances {
				list = append(list, inst)
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodPost && r.URL.Path == instancesPath:
			var req launchInstanceRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Error(err)
				http.Error(w, "invalid JSON", 400)
				return
			}
			lastLaunch = req
			lastRetryToken = r.Header.Get("opc-retry-token")
			inst := ociInstance{
				ID:             "ocid1.instance.oc1.iad.runner1",
				DisplayName:    req.DisplayName,
				LifecycleState: "RUNNING",
				TimeCreated:    time.Now().UTC().Format(time.RFC3339),
				FreeformTags:   req.FreeformTags,
			}
			instances[inst.ID] = inst
			_ = json.NewEncoder(w).Encode(inst)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, instancesBasePath):
			id := strings.TrimPrefix(r.URL.Path, instancesBasePath)
			inst, ok := instances[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"NotAuthorizedOrNotFound","message":"not found"}`))
				return
			}
			w.Header().Set("ETag", "current")
			_ = json.NewEncoder(w).Encode(inst)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, instancesBasePath):
			id := strings.TrimPrefix(r.URL.Path, instancesBasePath)
			if r.Header.Get("If-Match") != "current" {
				http.Error(w, "stale", 412)
				return
			}
			deletedPaths = append(deletedPaths, r.URL.RequestURI())
			delete(instances, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			http.Error(w, "unexpected request", 400)
		}
	}))
	defer srv.Close()

	cfg := baseTestConfig(t, srv, pemKey)
	cfg.Spot = true
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	params := compute.RunnerParams{
		JobKey:          "org/repo:42",
		ProvisionEpoch:  1,
		RunnerJITConfig: "jit-secret",
		InstanceSize:    "VM.Standard.A1.Flex:4:24",
	}
	created, err := client.CreateRunner(context.Background(), params)
	if err != nil || created.ID != "ocid1.instance.oc1.iad.runner1" {
		t.Fatalf("CreateRunner() = %#v, %v", created, err)
	}

	mu.Lock()
	launch, token := lastLaunch, lastRetryToken
	mu.Unlock()
	if launch.Shape != "VM.Standard.A1.Flex" || launch.ShapeConfig == nil ||
		launch.ShapeConfig.OCPUs != 4 || launch.ShapeConfig.MemoryInGBs != 24 {
		t.Fatalf("unexpected flex shape config: %#v", launch)
	}
	if launch.PreemptibleInstanceConfig == nil ||
		launch.PreemptibleInstanceConfig.PreemptionAction.Type != "TERMINATE" ||
		launch.PreemptibleInstanceConfig.PreemptionAction.PreserveBootVolume {
		t.Fatalf("unexpected preemptible config: %#v", launch.PreemptibleInstanceConfig)
	}
	if launch.CreateVnicDetails.AssignPublicIP {
		t.Fatal("private-by-default OCI VNIC unexpectedly requested public IP")
	}
	if token == "" {
		t.Fatal("missing opc-retry-token")
	}
	decodedUD, err := base64.StdEncoding.DecodeString(launch.Metadata["user_data"])
	if err != nil || !strings.Contains(string(decodedUD), "spot=true") {
		t.Fatalf("decoded user_data = %q, %v", decodedUD, err)
	}

	// FindRunner returns the created instance; RunnerAbsent reports false while RUNNING.
	found, ok, err := client.FindRunner(context.Background(), params.JobKey)
	if err != nil || !ok || found.ID != created.ID {
		t.Fatalf("FindRunner = %#v, %v, %v", found, ok, err)
	}
	if absent, err := client.RunnerAbsent(context.Background(), created.ID); err != nil || absent {
		t.Fatalf("RunnerAbsent(RUNNING) = %v, %v", absent, err)
	}

	// Terminated state is treated as absent cleanly.
	mu.Lock()
	termInst := instances[created.ID]
	termInst.LifecycleState = "TERMINATED"
	instances[created.ID] = termInst
	mu.Unlock()
	if _, ok, err := client.FindRunner(context.Background(), params.JobKey); err != nil || ok {
		t.Fatalf("FindRunner(TERMINATED) = ok=%v err=%v, want absent", ok, err)
	}
	if absent, err := client.RunnerAbsent(context.Background(), created.ID); err != nil || !absent {
		t.Fatalf("RunnerAbsent(TERMINATED) = %v, %v, want true", absent, err)
	}

	// Restore RUNNING and verify DeleteRunner terminates with preserveBootVolume=false.
	mu.Lock()
	termInst.LifecycleState = "RUNNING"
	instances[created.ID] = termInst
	mu.Unlock()
	if err := client.DeleteRunner(context.Background(), created.ID, params.JobKey); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	mu.Lock()
	paths := slices.Clone(deletedPaths)
	mu.Unlock()
	if len(paths) != 1 || !strings.Contains(paths[0], "preserveBootVolume=false") {
		t.Fatalf("paths = %v", paths)
	}
	if absent, err := client.RunnerAbsent(context.Background(), created.ID); err == nil || absent {
		t.Fatalf("RunnerAbsent(404) = %v, %v, want indeterminate", absent, err)
	}
}

func TestOCICapacityClassificationAndOnDemandFallback(t *testing.T) {
	_, pemKey := generateTestRSAKey(t)

	t.Run("classifies Out of host capacity and LimitExceeded", func(t *testing.T) {
		for _, respBody := range []string{
			`{"code":"InternalError","message":"Out of host capacity."}`,
			`{"code":"LimitExceeded","message":"Service limit reached"}`,
		} {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(respBody))
			}))
			defer srv.Close()

			cfg := baseTestConfig(t, srv, pemKey)
			cfg.Spot = true
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:cap"})
			if !compute.IsCapacityError(err) {
				t.Fatalf("response %s -> error = %v, want ErrCapacityUnavailable", respBody, err)
			}
		}
	})

	t.Run("spot fallback to on-demand uses distinct retry token and drops preemptible config", func(t *testing.T) {
		var mu sync.Mutex
		var attempts []launchInstanceRequest
		var retryTokens []string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			var req launchInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				http.Error(w, "invalid JSON", 400)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, req)
			retryTokens = append(retryTokens, r.Header.Get("opc-retry-token"))
			if len(attempts) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":"InternalError","message":"Out of host capacity."}`))
				return
			}
			_ = json.NewEncoder(w).Encode(ociInstance{ID: "ocid1.instance.oc1..ondemand", DisplayName: req.DisplayName})
		}))
		defer srv.Close()

		cfg := baseTestConfig(t, srv, pemKey)
		cfg.Spot = true
		cfg.SpotFallbackOnDemand = true
		client, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:fb"})
		if err != nil || inst.ID != "ocid1.instance.oc1..ondemand" {
			t.Fatalf("fallback CreateRunner = %#v, %v", inst, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(attempts) != 2 || attempts[0].PreemptibleInstanceConfig == nil || attempts[1].PreemptibleInstanceConfig != nil {
			t.Fatalf("unexpected attempts: %#v", attempts)
		}
		if retryTokens[0] == retryTokens[1] {
			t.Fatal("fallback attempt reused spot opc-retry-token")
		}
	})
}

func TestOCISweepPaginationOwnershipAndAmbiguousRecovery(t *testing.T) {
	_, pemKey := generateTestRSAKey(t)

	t.Run("paginated orphan sweep via opc-next-page", func(t *testing.T) {
		old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
		fresh := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
		var mu sync.Mutex
		var deleted []string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, instancesBasePath) {
				id := strings.TrimPrefix(r.URL.Path, instancesBasePath)
				w.Header().Set("ETag", id)
				_ = json.NewEncoder(w).Encode(ociInstance{ID: id, LifecycleState: "RUNNING", FreeformTags: map[string]string{controllerTagKey: "primary"}})
				return
			}
			if r.Method == http.MethodDelete {
				if r.Header.Get("If-Match") != strings.TrimPrefix(r.URL.Path, instancesBasePath) {
					http.Error(w, "stale", 412)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				deleted = append(deleted, strings.TrimPrefix(r.URL.Path, instancesBasePath))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			page := r.URL.Query().Get("page")
			switch page {
			case "":
				w.Header().Set("opc-next-page", "page-2")
				_ = json.NewEncoder(w).Encode([]ociInstance{
					{ID: "ocid1.inst.known", LifecycleState: "RUNNING", TimeCreated: old, FreeformTags: map[string]string{controllerTagKey: "primary"}},
					{ID: "ocid1.inst.foreign", LifecycleState: "RUNNING", TimeCreated: old, FreeformTags: map[string]string{controllerTagKey: "other"}},
					{ID: "ocid1.inst.orphan1", LifecycleState: "RUNNING", TimeCreated: old, FreeformTags: map[string]string{controllerTagKey: "primary"}},
				})
			case "page-2":
				_ = json.NewEncoder(w).Encode([]ociInstance{
					{ID: "ocid1.inst.fresh", LifecycleState: "RUNNING", TimeCreated: fresh, FreeformTags: map[string]string{controllerTagKey: "primary"}},
					{ID: "ocid1.inst.terminated", LifecycleState: "TERMINATED", TimeCreated: old, FreeformTags: map[string]string{controllerTagKey: "primary"}},
					{ID: "ocid1.inst.orphan2", LifecycleState: "RUNNING", TimeCreated: old, FreeformTags: map[string]string{controllerTagKey: "primary"}},
				})
			default:
				t.Errorf("unexpected page %q", page)
				http.Error(w, "unexpected page", 400)
			}
		}))
		defer srv.Close()

		client, err := NewClient(baseTestConfig(t, srv, pemKey))
		if err != nil {
			t.Fatal(err)
		}
		count, err := client.SweepOrphanedRunners(context.Background(), map[string]struct{}{"ocid1.inst.known": {}}, time.Now().Add(-time.Hour))
		mu.Lock()
		defer mu.Unlock()
		slices.Sort(deleted)
		if err != nil || count != 2 || !slices.Equal(deleted, []string{"ocid1.inst.orphan1", "ocid1.inst.orphan2"}) {
			t.Fatalf("sweep = %d, %v, deleted=%v", count, err, deleted)
		}
	})

	t.Run("ownership mismatch and duplicate guards", func(t *testing.T) {
		jobKey := "org/repo:guard"
		var duplicate atomic.Bool
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, instancesBasePath) {
				_ = json.NewEncoder(w).Encode(ociInstance{
					ID: "ocid1.inst.foreign", LifecycleState: "RUNNING",
					FreeformTags: map[string]string{controllerTagKey: "other", jobTagKey: compute.JobTag(jobKey)},
				})
				return
			}
			if !duplicate.Load() {
				_ = json.NewEncoder(w).Encode([]ociInstance{{
					ID: "ocid1.inst.foreign", LifecycleState: "RUNNING",
					FreeformTags: map[string]string{controllerTagKey: "other", jobTagKey: compute.JobTag(jobKey)},
				}})
				return
			}
			_ = json.NewEncoder(w).Encode([]ociInstance{
				{ID: "ocid1.inst.d1", LifecycleState: "RUNNING", FreeformTags: map[string]string{controllerTagKey: "primary", jobTagKey: compute.JobTag(jobKey)}},
				{ID: "ocid1.inst.d2", LifecycleState: "RUNNING", FreeformTags: map[string]string{controllerTagKey: "primary", jobTagKey: compute.JobTag(jobKey)}},
			})
		}))
		defer srv.Close()

		client, err := NewClient(baseTestConfig(t, srv, pemKey))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.FindRunner(context.Background(), jobKey); !errors.Is(err, compute.ErrOwnershipMismatch) {
			t.Fatalf("foreign FindRunner error = %v", err)
		}
		if err := client.DeleteRunner(context.Background(), "ocid1.inst.foreign", jobKey); !errors.Is(err, compute.ErrOwnershipMismatch) {
			t.Fatalf("foreign DeleteRunner error = %v", err)
		}
		duplicate.Store(true)
		if _, _, err := client.FindRunner(context.Background(), jobKey); !errors.Is(err, compute.ErrDuplicateInstances) {
			t.Fatalf("duplicate FindRunner error = %v", err)
		}
	})

	t.Run("ambiguous create recovery", func(t *testing.T) {
		params := compute.RunnerParams{JobKey: "org/repo:ambig", ProvisionEpoch: 2}
		wantName := compute.DeterministicName("oci", params.JobKey, params.ProvisionEpoch)
		var listCalls atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"code":"BadGateway","message":"transient"}`))
				return
			}
			if listCalls.Add(1) == 1 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]ociInstance{{
				ID: "ocid1.inst.recovered", DisplayName: wantName, LifecycleState: "RUNNING",
				FreeformTags: map[string]string{controllerTagKey: "primary", jobTagKey: compute.JobTag(params.JobKey)},
			}})
		}))
		defer srv.Close()

		client, err := NewClient(baseTestConfig(t, srv, pemKey))
		if err != nil {
			t.Fatal(err)
		}
		client.recoveryTimeout = 200 * time.Millisecond
		client.recoveryPoll = time.Millisecond
		inst, err := client.CreateRunner(context.Background(), params)
		if err != nil || inst.ID != "ocid1.inst.recovered" || inst.Name != wantName {
			t.Fatalf("recovered = %#v, %v", inst, err)
		}
	})
}

func TestOCIValidationAndPKCS8Key(t *testing.T) {
	privKey, _ := generateTestRSAKey(t)
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8PEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes}))
	keyFile := filepath.Join(t.TempDir(), "oci.pem")
	if err := os.WriteFile(keyFile, []byte(pkcs8PEM), 0o600); err != nil {
		t.Fatal(err)
	}

	shape, cfg, err := ParseShapeSpec("VM.Standard2.4", 4, 16)
	if err != nil || shape != "VM.Standard2.4" || cfg != nil {
		t.Fatalf("fixed shape = %q, %#v, %v", shape, cfg, err)
	}
	shape, cfg, err = ParseShapeSpec("VM.Standard.E4.Flex:8:32", 4, 16)
	if err != nil || shape != "VM.Standard.E4.Flex" || cfg == nil || cfg.OCPUs != 8 || cfg.MemoryInGBs != 32 {
		t.Fatalf("inline flex shape = %q, %#v, %v", shape, cfg, err)
	}
	if _, _, err := ParseShapeSpec("VM.Standard.E4.Flex:0:16", 4, 16); err == nil {
		t.Fatal("accepted zero OCPUs")
	}

	loaded, err := loadPrivateKey(keyFile, "")
	if err != nil || loaded == nil {
		t.Fatalf("loadPrivateKey(PKCS8) = %v", err)
	}
	if _, err := loadPrivateKey(keyFile, pkcs8PEM); err == nil {
		t.Fatal("accepted both private key path and PEM")
	}
	if _, err := loadPrivateKey("", ""); err == nil {
		t.Fatal("accepted missing private key")
	}
}

func TestOCIUnsupportedSpotShape(t *testing.T) {
	_, key := generateTestRSAKey(t)
	var posts atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "[]")
			return
		}
		var launch launchInstanceRequest
		if err := json.NewDecoder(r.Body).Decode(&launch); err != nil || launch.PreemptibleInstanceConfig != nil || launch.Shape != "VM.Standard.E2.1.Micro" {
			t.Error("expected an on-demand Micro request")
			http.Error(w, "unexpected launch", 400)
			return
		}
		posts.Add(1)
		_ = json.NewEncoder(w).Encode(ociInstance{ID: "micro"})
	}))
	defer srv.Close()
	cfg := baseTestConfig(t, srv, key)
	cfg.Shape = "VM.Standard.E2.1.Micro"
	cfg.Spot = true
	for _, fallback := range []bool{false, true} {
		cfg.SpotFallbackOnDemand = fallback
		c, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CreateRunner(t.Context(), compute.RunnerParams{JobKey: "micro"})
		if !fallback && (!errors.Is(err, compute.ErrSpotUnsupported) || posts.Load() != 0) {
			t.Fatalf("spot-only launch: posts=%d err=%v", posts.Load(), err)
		}
		if fallback && (err != nil || posts.Load() != 1) {
			t.Fatalf("on-demand fallback: posts=%d err=%v", posts.Load(), err)
		}
	}
}

func TestOCIDeletionGuards(t *testing.T) {
	_, key := generateTestRSAKey(t)
	for _, tc := range []struct {
		name                    string
		getStatus, deleteStatus int
		tag, etag               string
		wantDeletes             int
	}{
		{"hidden", 404, 0, "", "", 0}, {"retagged", 200, 0, "other", "changed", 0},
		{"no etag", 200, 0, "primary", "", 0}, {"changed during termination", 200, 412, "primary", "observed", 1},
		{"termination denied", 200, 404, "primary", "observed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deletes atomic.Int32
			inst := ociInstance{ID: "runner", TimeCreated: time.Now().Add(-time.Hour).Format(time.RFC3339), LifecycleState: "RUNNING", FreeformTags: map[string]string{controllerTagKey: "primary", jobTagKey: compute.JobTag("job")}}
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == instancesPath:
					_ = json.NewEncoder(w).Encode([]ociInstance{inst})
				case r.Method == http.MethodGet && tc.getStatus == 200:
					current := inst
					current.FreeformTags = map[string]string{controllerTagKey: tc.tag, jobTagKey: compute.JobTag("job")}
					w.Header().Set("ETag", tc.etag)
					_ = json.NewEncoder(w).Encode(current)
				case r.Method == http.MethodDelete:
					deletes.Add(1)
					if tc.deleteStatus == 0 || r.Header.Get("If-Match") != tc.etag {
						t.Error("unexpected termination")
						http.Error(w, "unexpected delete", 400)
						return
					}
					w.WriteHeader(tc.deleteStatus)
					_, _ = io.WriteString(w, `{"code":"NotAuthorizedOrNotFound"}`)
				default:
					w.WriteHeader(tc.getStatus)
					_, _ = io.WriteString(w, `{"code":"NotAuthorizedOrNotFound"}`)
				}
			}))
			defer srv.Close()
			c, err := NewClient(baseTestConfig(t, srv, key))
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range []func() error{func() error { return c.DeleteRunner(t.Context(), "runner", "job") }, func() error {
				count, err := c.SweepOrphanedRunners(t.Context(), nil, time.Now())
				if count != 0 {
					t.Errorf("sweep count=%d", count)
				}
				return err
			}} {
				err := run()
				if err == nil {
					t.Fatal("reported successful deletion")
				}
				if tc.getStatus == 404 || tc.deleteStatus >= 400 {
					var apiErr *compute.APIError
					if !errors.As(err, &apiErr) || compute.IsHTTPNotFound(err) {
						t.Fatalf("lost indeterminate API error: %v", err)
					}
				}
				if n := deletes.Swap(0); int(n) != tc.wantDeletes {
					t.Fatalf("delete requests=%d, want %d", n, tc.wantDeletes)
				}
			}
		})
	}
}

func TestOCIValidatedPricingAndEndpoint(t *testing.T) {
	_, key := generateTestRSAKey(t)
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	cfg := baseTestConfig(t, srv, key)
	cfg.OCPUs = 16
	cfg.MemoryGB = 64
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{"": "VM.Standard.E4.Flex:16:64", "VM.Standard.E5.Flex": "VM.Standard.E5.Flex:16:64", "VM.Standard.E5.Flex:2:8": "VM.Standard.E5.Flex:2:8", "VM.Standard2.4": "VM.Standard2.4"} {
		if got := compute.PricingSize(c, input, ""); got != want {
			t.Errorf("pricing size(%q)=%q, want %q", input, got, want)
		}
	}
	if _, _, err := ParseShapeSpec("VM.Standard2.4:4:16", 4, 16); err == nil {
		t.Error("accepted dimensions on fixed shape")
	}
	cfg.BaseURL = "http://oci.example.invalid"
	if _, err := NewClient(cfg); err == nil {
		t.Error("accepted plaintext endpoint")
	}
}
