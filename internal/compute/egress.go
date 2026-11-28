// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

var egressHostRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)

// ValidEgressEntry reports whether value is a safe IP, CIDR prefix, or DNS
// hostname suitable for runner egress firewall allowlisting.
func ValidEgressEntry(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 253 {
		return false
	}
	if _, err := netip.ParsePrefix(trimmed); err == nil {
		return true
	}
	if _, err := netip.ParseAddr(trimmed); err == nil {
		return true
	}
	return egressHostRegex.MatchString(trimmed)
}

// ParseEgressAllowlist parses a comma-separated list of allowed egress hosts
// or CIDRs, returning deduplicated canonical entries.
func ParseEgressAllowlist(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if !ValidEgressEntry(entry) {
			return nil, fmt.Errorf("invalid egress allowlist entry %q", part)
		}
		key := strings.ToLower(entry)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out, nil
}
