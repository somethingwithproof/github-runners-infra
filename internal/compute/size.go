// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	labelNameRegex    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)
	instanceSizeRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
)

// ValidLabel reports whether value is a safe GitHub runner label identifier.
func ValidLabel(value string) bool {
	return labelNameRegex.MatchString(value)
}

// ValidInstanceSize reports whether value is a safe provider instance size,
// machine type, plan, or SKU identifier.
func ValidInstanceSize(value string) bool {
	return instanceSizeRegex.MatchString(value) && !strings.Contains(value, "..")
}

// ValidSizeSpec also accepts dimensions on an OCI Flex shape.
func ValidSizeSpec(value string) bool {
	name, dimensions, inline := strings.Cut(value, ":")
	if !ValidInstanceSize(name) {
		return false
	}
	return !inline || strings.HasSuffix(strings.ToLower(name), ".flex") && flexDimensions.MatchString(dimensions)
}

var flexDimensions = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?:[0-9]+(\.[0-9]+)?$`)

// PricingSize resolves the same provider defaults used at launch.
func PricingSize(provider interface{ Provider() string }, override, fallback string) string {
	if override == "" {
		override = fallback
	}
	if p, ok := provider.(interface{ PricingSize(string) string }); ok {
		return p.PricingSize(override)
	}
	return override
}

// ParseLabelSizeMap parses comma-separated "label=size" entries into a
// lowercase-keyed map. Empty input returns a nil map.
func ParseLabelSizeMap(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	entries := strings.Split(trimmed, ",")
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		part := strings.TrimSpace(entry)
		if part == "" {
			return nil, fmt.Errorf("empty entry in label size map %q", raw)
		}
		rawLabel, rawSize, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("label size map entry %q must use label=size format", part)
		}
		label := strings.TrimSpace(rawLabel)
		size := strings.TrimSpace(rawSize)
		if !ValidLabel(label) {
			return nil, fmt.Errorf("invalid label %q in label size map", rawLabel)
		}
		if !ValidSizeSpec(size) {
			return nil, fmt.Errorf("invalid instance size %q for label %q", rawSize, label)
		}
		key := strings.ToLower(label)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate label %q in label size map", label)
		}
		result[key] = size
	}
	return result, nil
}

// ResolveInstanceSize selects a deterministic instance size override for a
// job's labels. Non-required labels take precedence over requiredLabel so a
// generic routing label (such as "self-hosted") never masks a size label.
func ResolveInstanceSize(labels []string, requiredLabel string, sizeMap map[string]string) string {
	if len(sizeMap) == 0 {
		return ""
	}
	required := strings.ToLower(strings.TrimSpace(requiredLabel))
	fallback := ""
	for _, raw := range labels {
		key := strings.ToLower(strings.TrimSpace(raw))
		size, ok := sizeMap[key]
		if !ok {
			continue
		}
		if required != "" && key == required {
			if fallback == "" {
				fallback = size
			}
			continue
		}
		return size
	}
	return fallback
}
