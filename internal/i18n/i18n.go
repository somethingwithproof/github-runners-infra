// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package i18n provides zero-dependency localization across 20 Tier 1 locales.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

//go:embed locales/*.json
var localeFS embed.FS

var supported = []string{
	"en", "de", "es", "fr", "it", "pt-BR", "nl", "ru", "uk",
	"ja", "ko", "zh-CN", "zh-TW", "ar", "hi", "pl", "tr", "sv", "vi", "id",
}

// Catalog holds resolved message strings for a single locale with English fallback.
type Catalog struct {
	messages map[string]string
	fallback map[string]string
	locale   string
}

var (
	cacheMu  sync.RWMutex
	catalogs = make(map[string]map[string]string)

	defaultMu      sync.RWMutex
	defaultCatalog *Catalog
)

// SupportedLocales returns a copy of the 20 supported canonical locale identifiers.
func SupportedLocales() []string {
	out := make([]string, len(supported))
	copy(out, supported)
	return out
}

// IsRTL reports whether locale uses right-to-left script direction.
func IsRTL(locale string) bool {
	return ResolveLocale(locale) == "ar"
}

// ResolveLocale maps a POSIX or BCP-47 locale tag to a supported canonical locale.
// Unrecognized, empty, C, and POSIX values resolve to "en".
func ResolveLocale(tag string) string {
	clean := strings.TrimSpace(tag)
	if clean == "" {
		return "en"
	}
	if idx := strings.IndexAny(clean, ".@"); idx >= 0 {
		clean = clean[:idx]
	}
	clean = strings.ReplaceAll(clean, "_", "-")
	lower := strings.ToLower(clean)

	if lower == "" || lower == "c" || lower == "posix" {
		return "en"
	}

	switch lower {
	case "zh-tw", "zh-hk", "zh-mo", "zh-hant", "zh-hant-tw", "zh-hant-hk", "zh-hant-mo":
		return "zh-TW"
	case "zh", "zh-cn", "zh-sg", "zh-hans", "zh-hans-cn", "zh-hans-sg":
		return "zh-CN"
	case "pt", "pt-br":
		return "pt-BR"
	}

	for _, loc := range supported {
		if strings.EqualFold(loc, lower) {
			return loc
		}
	}

	base := lower
	if idx := strings.IndexByte(lower, '-'); idx >= 0 {
		base = lower[:idx]
	}
	if base == "zh" {
		if strings.Contains(lower, "-hans") {
			return "zh-CN"
		}
		if strings.Contains(lower, "-hant") || strings.Contains(lower, "-tw") || strings.Contains(lower, "-hk") || strings.Contains(lower, "-mo") {
			return "zh-TW"
		}
		return "zh-CN"
	}
	if base == "pt" {
		return "pt-BR"
	}
	for _, loc := range supported {
		if strings.EqualFold(loc, base) {
			return loc
		}
	}
	return "en"
}

// DetectLocale inspects POSIX locale variables in standard precedence order:
// LC_ALL, then LC_MESSAGES, then LANG.
func DetectLocale() string {
	for _, envKey := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
			return ResolveLocale(val)
		}
	}
	return "en"
}

func loadMessages(locale string) map[string]string {
	cacheMu.RLock()
	if m, ok := catalogs[locale]; ok {
		cacheMu.RUnlock()
		return m
	}
	cacheMu.RUnlock()

	raw, err := localeFS.ReadFile("locales/" + locale + ".json")
	if err != nil {
		return nil
	}
	var parsed map[string]string
	if unmarshalErr := json.Unmarshal(raw, &parsed); unmarshalErr != nil {
		return nil
	}

	cacheMu.Lock()
	catalogs[locale] = parsed
	cacheMu.Unlock()
	return parsed
}

// New creates a Catalog for the resolved locale with English fallback.
func New(locale string) *Catalog {
	resolved := ResolveLocale(locale)
	fallback := loadMessages("en")
	msgs := fallback
	if resolved != "en" {
		if loaded := loadMessages(resolved); loaded != nil {
			msgs = loaded
		}
	}
	return &Catalog{
		messages: msgs,
		fallback: fallback,
		locale:   resolved,
	}
}

// Locale returns the canonical locale code of the catalog.
func (c *Catalog) Locale() string {
	return c.locale
}

// IsRTL reports whether this catalog uses right-to-left script direction.
func (c *Catalog) IsRTL() bool {
	return c.locale == "ar"
}

// T looks up key and formats it with args when args are provided.
func (c *Catalog) T(key string, args ...any) string {
	msg, ok := c.messages[key]
	if !ok {
		msg, ok = c.fallback[key]
		if !ok {
			msg = key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// Init configures the process-wide default catalog from an explicit flag override
// or POSIX environment variables when override is empty.
func Init(override string) *Catalog {
	locale := DetectLocale()
	if strings.TrimSpace(override) != "" {
		locale = ResolveLocale(override)
	}
	cat := New(locale)
	defaultMu.Lock()
	defaultCatalog = cat
	defaultMu.Unlock()
	return cat
}

// Default returns the active default catalog, initializing from environment if needed.
func Default() *Catalog {
	defaultMu.RLock()
	cat := defaultCatalog
	defaultMu.RUnlock()
	if cat != nil {
		return cat
	}
	return Init("")
}

// T translates key using the active default catalog.
func T(key string, args ...any) string {
	return Default().T(key, args...)
}

type wrappedError struct {
	err error
	msg string
}

func (e *wrappedError) Error() string { return e.msg }
func (e *wrappedError) Unwrap() error { return e.err }

// Wrap formats a localized error message while preserving errors.Is/errors.As unwrapping.
func Wrap(err error, key string, args ...any) error {
	if err == nil {
		return nil
	}
	return &wrappedError{err: err, msg: Default().T(key, args...)}
}
