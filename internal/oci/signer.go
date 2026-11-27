// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package oci

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type requestSigner struct {
	keyID      string
	privateKey *rsa.PrivateKey
	now        func() time.Time
}

func newRequestSigner(tenancyOCID, userOCID, fingerprint string, privateKey *rsa.PrivateKey) *requestSigner {
	return &requestSigner{
		keyID:      tenancyOCID + "/" + userOCID + "/" + fingerprint,
		privateKey: privateKey,
		now:        time.Now,
	}
}

func loadPrivateKey(path, rawPEM string) (*rsa.PrivateKey, error) {
	trimmedPath := strings.TrimSpace(path)
	trimmedPEM := strings.TrimSpace(rawPEM)
	if trimmedPath != "" && trimmedPEM != "" {
		return nil, fmt.Errorf("set OCI private key path or inline PEM, not both")
	}
	if trimmedPath == "" && trimmedPEM == "" {
		return nil, fmt.Errorf("OCI private key path or PEM is required")
	}
	var pemBytes []byte
	if trimmedPath != "" {
		var err error
		pemBytes, err = os.ReadFile(trimmedPath)
		if err != nil {
			return nil, fmt.Errorf("read OCI private key file: %w", err)
		}
	} else {
		pemBytes = []byte(trimmedPEM)
	}
	return parseRSAPrivateKey(pemBytes)
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("decode OCI private key PEM: no PEM block found")
	}
	if strings.Contains(strings.ToUpper(block.Type), "ENCRYPTED") || block.Headers["Proc-Type"] != "" {
		return nil, fmt.Errorf("encrypted OCI private keys are not supported")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse OCI private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("OCI private key must be an RSA key")
	}
	return rsaKey, nil
}

// Sign applies OCI HTTP Signature version="1" (RSA-SHA256) authentication to req.
// For GET/DELETE/HEAD requests it signs (request-target), date, and host; for
// POST/PUT/PATCH requests it also signs x-content-sha256, content-type, and
// content-length.
func (s *requestSigner) Sign(req *http.Request, body []byte) error {
	dateVal := req.Header.Get("Date")
	if dateVal == "" {
		dateVal = s.now().UTC().Format(http.TimeFormat)
		req.Header.Set("Date", dateVal)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Host = host

	headers := []string{"(request-target)", "date", "host"}
	lines := []string{
		fmt.Sprintf("(request-target): %s %s", strings.ToLower(req.Method), req.URL.RequestURI()),
		"date: " + dateVal,
		"host: " + host,
	}

	if req.Method == http.MethodPost || req.Method == http.MethodPut || req.Method == http.MethodPatch {
		digest := sha256.Sum256(body)
		contentSHA := base64.StdEncoding.EncodeToString(digest[:])
		contentType := req.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
			req.Header.Set("Content-Type", contentType)
		}
		contentLength := strconv.Itoa(len(body))
		req.Header.Set("x-content-sha256", contentSHA)
		req.Header.Set("Content-Length", contentLength)
		req.ContentLength = int64(len(body))

		headers = append(headers, "x-content-sha256", "content-type", "content-length")
		lines = append(lines,
			"x-content-sha256: "+contentSHA,
			"content-type: "+contentType,
			"content-length: "+contentLength,
		)
	}

	signingString := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(signingString))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.privateKey, crypto.SHA256, sum[:])
	if err != nil {
		return fmt.Errorf("sign OCI request digest: %w", err)
	}

	authHeader := fmt.Sprintf(
		`Signature version="1",keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		s.keyID,
		strings.Join(headers, " "),
		base64.StdEncoding.EncodeToString(sig),
	)
	req.Header.Set("Authorization", authHeader)
	return nil
}
