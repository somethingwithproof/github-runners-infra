// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	// MaxRESTResponseBytes bounds JSON API responses read from cloud providers.
	MaxRESTResponseBytes = 4 * 1024 * 1024
	// ConfirmedAbsenceObservations is the number of clean empty polls required
	// before treating an ambiguous create failure as confirmed absent.
	ConfirmedAbsenceObservations = 3
)

// APIError captures a non-2xx HTTP status and parsed provider error message.
type APIError struct {
	Provider   string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s API status %d: %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s API status %d", e.Provider, e.StatusCode)
}

// IsHTTPNotFound reports whether err represents an HTTP 404 provider response.
func IsHTTPNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsAmbiguousCreateError reports whether a create error may have succeeded
// server-side (transport failure, 408 timeout, or 5xx response).
func IsAmbiguousCreateError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.StatusCode >= http.StatusInternalServerError || apiErr.StatusCode == http.StatusRequestTimeout
}

// DefaultHTTPClient returns a bounded HTTP client for cloud REST APIs.
func DefaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// NormalizeBaseURL validates an optional custom REST API base URL or falls back
// to defaultURL.
func NormalizeBaseURL(raw, defaultURL, provider string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultURL, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(trimmed, "#") {
		return "", fmt.Errorf("invalid %s API base URL: require HTTPS without credentials, query, or fragment", provider)
	}
	return strings.TrimRight(trimmed, "/"), nil
}

// RESTRequest describes a single authenticated JSON request against a cloud REST API.
type RESTRequest struct {
	Provider    string
	Client      *http.Client
	Method      string
	URL         string
	BearerToken string
	Headers     map[string]string
	Body        any
	Out         any
	ParseError  func([]byte) string
}

// DoJSON executes a bounded, context-aware authenticated JSON HTTP call.
func DoJSON(ctx context.Context, cfg RESTRequest) error {
	var bodyReader io.Reader
	if cfg.Body != nil {
		encoded, err := json.Marshal(cfg.Body)
		if err != nil {
			return fmt.Errorf("marshal %s request: %w", cfg.Provider, err)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bodyReader)
	if err != nil {
		return fmt.Errorf("build %s request: %w", cfg.Provider, err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	req.Header.Set("Accept", "application/json")
	if cfg.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cfg.Headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxRESTResponseBytes+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := ""
		if err == nil && len(raw) <= MaxRESTResponseBytes && cfg.ParseError != nil {
			msg = strings.TrimSpace(cfg.ParseError(raw))
		}
		return &APIError{Provider: cfg.Provider, StatusCode: resp.StatusCode, Message: msg}
	}

	if err != nil {
		return fmt.Errorf("read %s response: %w", cfg.Provider, err)
	}
	if len(raw) > MaxRESTResponseBytes {
		return fmt.Errorf("%s response exceeded %d bytes", cfg.Provider, MaxRESTResponseBytes)
	}

	if cfg.Out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, cfg.Out); err != nil {
		return fmt.Errorf("decode %s response: %w", cfg.Provider, err)
	}
	return nil
}

// ReconcileAmbiguousCreate polls lookup until the deterministically named
// resource is found, confirmed absent for ConfirmedAbsenceObservations clean
// polls, or ctx expires.
func ReconcileAmbiguousCreate[T any](
	ctx context.Context,
	provider, name string,
	pollInterval time.Duration,
	lookup func(context.Context) (*T, error),
) (*T, bool, error) {
	observations := 0
	consecutiveClean := 0
	for {
		recovered, err := lookup(ctx)
		observations++
		if recovered != nil {
			return recovered, false, nil
		}
		if err == nil {
			consecutiveClean++
			if consecutiveClean >= ConfirmedAbsenceObservations {
				return nil, true, nil
			}
		} else if ctx.Err() == nil {
			consecutiveClean = 0
			log.Printf("WARN: %s ambiguous-create recovery poll %d failed for %s: %v", provider, observations, name, err)
		}
		if ctx.Err() != nil {
			return nil, consecutiveClean >= ConfirmedAbsenceObservations, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, consecutiveClean >= ConfirmedAbsenceObservations, nil
		case <-timer.C:
		}
	}
}

// HasTag reports whether tags contains want.
func HasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// JobTag returns a deterministic, collision-resistant tag identifying jobKey.
func JobTag(jobKey string) string {
	jobHash := sha256.Sum256([]byte(jobKey))
	return "runner-job-" + hex.EncodeToString(jobHash[:8])
}

// DeterministicName returns a deterministic instance name scoped to provider,
// jobKey, and provisionEpoch.
func DeterministicName(provider, jobKey string, provisionEpoch int) string {
	resourceHash := sha256.Sum256([]byte(provider + ":" + jobKey + ":" + strconv.Itoa(provisionEpoch)))
	return "ghr-" + hex.EncodeToString(resourceHash[:16])
}

// RESTBaseConfig holds the validated common fields shared by token-based REST
// compute adapters.
type RESTBaseConfig struct {
	Provider       string
	TokenName      string
	Token          string
	ControllerID   string
	CloudInitPath  string
	Region         string
	DefaultRegion  string
	Size           string
	DefaultSize    string
	SizeLabel      string
	BaseURL        string
	DefaultBaseURL string
	HTTPClient     *http.Client
}

// RESTBase holds initialized common state for a REST compute adapter.
type RESTBase struct {
	HTTPClient    *http.Client
	BaseURL       string
	Token         string
	CloudInitTmpl *template.Template
	Region        string
	Size          string
	ControllerTag string
}

// InitRESTBase validates common REST adapter settings, parses the cloud-init
// template, and constructs a bounded default HTTP client when none is supplied.
func InitRESTBase(cfg RESTBaseConfig) (RESTBase, error) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return RESTBase{}, fmt.Errorf("%s %s is required", cfg.Provider, cfg.TokenName)
	}
	if !ValidControllerID(cfg.ControllerID) {
		return RESTBase{}, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	if strings.TrimSpace(cfg.CloudInitPath) == "" {
		return RESTBase{}, fmt.Errorf("%s cloud-init template path is required", cfg.Provider)
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return RESTBase{}, fmt.Errorf("parse cloud-init template: %w", err)
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = cfg.DefaultRegion
	}
	size := strings.TrimSpace(cfg.Size)
	if size == "" {
		size = cfg.DefaultSize
	}
	if !ValidInstanceSize(size) {
		return RESTBase{}, fmt.Errorf("invalid %s %q", cfg.SizeLabel, size)
	}
	baseURL, err := NormalizeBaseURL(cfg.BaseURL, cfg.DefaultBaseURL, cfg.Provider)
	if err != nil {
		return RESTBase{}, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = DefaultHTTPClient()
	}
	return RESTBase{
		HTTPClient:    httpClient,
		BaseURL:       baseURL,
		Token:         token,
		CloudInitTmpl: tmpl,
		Region:        region,
		Size:          size,
		ControllerTag: "runner-controller-" + cfg.ControllerID,
	}, nil
}

// SelectSingleRunner enforces unique job instance ownership from a filtered slice.
func SelectSingleRunner[T any](provider, jobKey string, items []T, toRunner func(T) *RunnerInstance) (*RunnerInstance, bool, error) {
	if len(items) == 0 {
		return nil, false, nil
	}
	if len(items) > 1 {
		return nil, false, fmt.Errorf("%w: multiple %s instances exist for job %s", ErrDuplicateInstances, provider, jobKey)
	}
	return toRunner(items[0]), true, nil
}

// SelectSizeOverride validates an optional per-job instance size override or
// returns defaultSize when override is empty.
func SelectSizeOverride(defaultSize, override, sizeLabel string) (string, error) {
	if override == "" {
		return defaultSize, nil
	}
	if !ValidInstanceSize(override) {
		return "", fmt.Errorf("invalid %s %q", sizeLabel, override)
	}
	return override, nil
}

// RecoverCreateError resolves an ambiguous provider create error by polling
// lookup for the deterministically named instance before deciding whether the
// caller may retry or must treat the outcome as unknown.
func RecoverCreateError[T any](
	ctx context.Context,
	provider, desiredName string,
	timeout, poll time.Duration,
	createErr error,
	lookup func(context.Context) (*T, error),
	toRunner func(*T) *RunnerInstance,
) (*RunnerInstance, error) {
	if !IsAmbiguousCreateError(createErr) {
		return nil, fmt.Errorf("create %s instance: %w", provider, createErr)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	recovered, confirmedAbsent, recoveryErr := ReconcileAmbiguousCreate(
		recoveryCtx, provider, desiredName, poll, lookup,
	)
	if recoveryErr != nil {
		return nil, errors.Join(fmt.Errorf("create %s instance: %w", provider, createErr), fmt.Errorf("reconcile ambiguous create: %w", recoveryErr))
	}
	if recovered != nil {
		return toRunner(recovered), nil
	}
	if confirmedAbsent {
		return nil, fmt.Errorf("create %s instance after confirmed absence: %w", provider, createErr)
	}
	return nil, fmt.Errorf("%w: create %s instance: %v", ErrCreateOutcomeUnknown, provider, createErr)
}

// FilterOwned verifies that every non-excluded item carries controllerTag.
func FilterOwned[T any](
	provider, excludedID, controllerTag string,
	items []T,
	idOf func(T) string,
	tagsOf func(T) []string,
) ([]T, error) {
	verified := make([]T, 0, len(items))
	for _, item := range items {
		id := idOf(item)
		if excludedID != "" && id == excludedID {
			continue
		}
		if !HasTag(tagsOf(item), controllerTag) {
			return nil, fmt.Errorf("%w: job-tagged %s instance %s belongs to another controller", ErrOwnershipMismatch, provider, id)
		}
		verified = append(verified, item)
	}
	return verified, nil
}

// FindUniqueByName locates the single item matching name among items.
func FindUniqueByName[T any](provider, jobKey, name string, items []T, nameOf func(T) string) (*T, error) {
	var match *T
	for i := range items {
		if nameOf(items[i]) != name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple %s instances named %q exist for job %s", provider, name, jobKey)
		}
		match = &items[i]
	}
	return match, nil
}

// SweepOrphans deletes controller-owned items absent from known and older than
// cutoff while continuing past items with unparseable creation timestamps.
func SweepOrphans[T any](
	ctx context.Context,
	provider, controllerTag string,
	known map[string]struct{},
	cutoff time.Time,
	items []T,
	idOf func(T) string,
	tagsOf func(T) []string,
	createdOf func(T) (time.Time, error),
	deleteFn func(context.Context, string) error,
) (int, error) {
	deleted := 0
	var sweepErrs []error
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return deleted, errors.Join(append(sweepErrs, err)...)
		}
		if !HasTag(tagsOf(item), controllerTag) {
			continue
		}
		id := idOf(item)
		if _, ok := known[id]; ok {
			continue
		}
		created, err := createdOf(item)
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("parse creation time for controller %s instance %s: %w", provider, id, err))
			continue
		}
		if !created.Before(cutoff) {
			continue
		}
		if err := deleteFn(ctx, id); err != nil && !IsHTTPNotFound(err) {
			sweepErrs = append(sweepErrs, fmt.Errorf("delete orphaned controller %s instance %s: %w", provider, id, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		deleted++
	}
	return deleted, errors.Join(sweepErrs...)
}
