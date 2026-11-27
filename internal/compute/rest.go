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
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("invalid %s API base URL %q", provider, raw)
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
	if err != nil {
		return fmt.Errorf("read %s response: %w", cfg.Provider, err)
	}
	if len(raw) > MaxRESTResponseBytes {
		return fmt.Errorf("%s response exceeded %d bytes", cfg.Provider, MaxRESTResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := ""
		if cfg.ParseError != nil {
			msg = strings.TrimSpace(cfg.ParseError(raw))
		}
		return &APIError{Provider: cfg.Provider, StatusCode: resp.StatusCode, Message: msg}
	}

	if cfg.Out == nil || len(bytes.TrimSpace(raw)) == 0 {
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
