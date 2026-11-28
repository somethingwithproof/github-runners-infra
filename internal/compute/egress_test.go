// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"reflect"
	"testing"
)

func TestParseEgressAllowlist(t *testing.T) {
	if got, err := ParseEgressAllowlist("   "); err != nil || got != nil {
		t.Fatalf("empty allowlist = %v, %v", got, err)
	}

	got, err := ParseEgressAllowlist("api.github.com, 140.82.112.0/20, 1.1.1.1, API.GITHUB.COM")
	if err != nil {
		t.Fatalf("ParseEgressAllowlist error = %v", err)
	}
	want := []string{"api.github.com", "140.82.112.0/20", "1.1.1.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist = %v, want %v", got, want)
	}

	for _, bad := range []string{
		"localhost",
		"api.github.com;rm -rf /",
		"bad host.example.com",
		"https://api.github.com",
		"140.82.112.0/99",
		"-bad.example.com",
		"api.github.com,",
	} {
		if _, err := ParseEgressAllowlist(bad); err == nil {
			t.Fatalf("ParseEgressAllowlist(%q) unexpectedly succeeded", bad)
		}
	}
}
