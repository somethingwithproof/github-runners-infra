// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package i18n

import (
	"encoding/json"
	"regexp"
	"sort"
	"testing"
)

var placeholderPattern = regexp.MustCompile(`%[0-9.]*[a-zA-Z]`)

func extractPlaceholders(s string) []string {
	matches := placeholderPattern.FindAllString(s, -1)
	sort.Strings(matches)
	return matches
}

func TestAllLocalesLoadWithParity(t *testing.T) {
	locales := SupportedLocales()
	if len(locales) != 48 {
		t.Fatalf("expected 48 supported locales, got %d", len(locales))
	}

	enRaw, err := localeFS.ReadFile("locales/en.json")
	if err != nil {
		t.Fatalf("read en.json: %v", err)
	}
	var enMap map[string]string
	if err := json.Unmarshal(enRaw, &enMap); err != nil {
		t.Fatalf("parse en.json: %v", err)
	}
	if len(enMap) == 0 {
		t.Fatal("en.json is empty")
	}

	for _, loc := range locales {
		t.Run(loc, func(t *testing.T) {
			raw, readErr := localeFS.ReadFile("locales/" + loc + ".json")
			if readErr != nil {
				t.Fatalf("read %s.json: %v", loc, readErr)
			}
			var locMap map[string]string
			if parseErr := json.Unmarshal(raw, &locMap); parseErr != nil {
				t.Fatalf("parse %s.json: %v", loc, parseErr)
			}
			if len(locMap) != len(enMap) {
				t.Fatalf("%s key count = %d, want %d", loc, len(locMap), len(enMap))
			}
			for key, enVal := range enMap {
				locVal, ok := locMap[key]
				if !ok {
					t.Errorf("%s missing key %q", loc, key)
					continue
				}
				if locVal == "" {
					t.Errorf("%s empty value for key %q", loc, key)
				}
				wantPH := extractPlaceholders(enVal)
				gotPH := extractPlaceholders(locVal)
				if len(wantPH) != len(gotPH) {
					t.Errorf("%s key %q placeholder mismatch: got %v, want %v", loc, key, gotPH, wantPH)
					continue
				}
				for i := range wantPH {
					if wantPH[i] != gotPH[i] {
						t.Errorf("%s key %q placeholder mismatch: got %v, want %v", loc, key, gotPH, wantPH)
						break
					}
				}
			}
			cat := New(loc)
			if cat.Locale() != loc {
				t.Errorf("New(%q).Locale() = %q", loc, cat.Locale())
			}
		})
	}
}

func TestResolveLocaleAndRTL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", "en"},
		{"C", "en"},
		{"POSIX", "en"},
		{"C.UTF-8", "en"},
		{"en_US.UTF-8", "en"},
		{"de_DE.UTF-8", "de"},
		{"pt_BR.UTF-8", "pt-BR"},
		{"zh_CN.UTF-8", "zh-CN"},
		{"zh_TW.UTF-8", "zh-TW"},
		{"zh-Hant", "zh-TW"},
		{"ar_SA.UTF-8", "ar"},
		{"ar_EG.UTF-8", "ar-EG"},
		{"ar_MA.UTF-8", "ar-MA"},
		{"fr_MA.UTF-8", "fr-MA"},
		{"sw_KE.UTF-8", "sw-KE"},
		{"zu", "zu-ZA"},
		{"fa_IR.UTF-8", "fa"},
		{"he_IL.UTF-8", "he"},
		{"ur_PK.UTF-8", "ur"},
		{"tl_PH.UTF-8", "fil"},
		{"hi_IN.UTF-8", "hi"},
		{"sv_SE.UTF-8", "sv"},
		{"bs", "bs"},
		{"bs-BA", "bs"},
		{"bs_BA.UTF-8", "bs"},
		{"hr", "hr"},
		{"hr-HR", "hr"},
		{"hr_HR.UTF-8", "hr"},
		{"sr", "sr"},
		{"sr-RS", "sr"},
		{"sr_RS.UTF-8", "sr"},
		{"sr-Cyrl", "sr"},
		{"sr-Latn", "sr"},
		{"sr-Cyrl-RS", "sr"},
		{"sr-Latn-RS", "sr"},
		{"unknown", "en"},
	}
	for _, tc := range cases {
		if got := ResolveLocale(tc.input); got != tc.want {
			t.Errorf("ResolveLocale(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	rtlSet := map[string]bool{
		"ar": true, "ar-EG": true, "ar-MA": true, "fa": true, "he": true, "ur": true,
	}
	for _, loc := range SupportedLocales() {
		wantRTL := rtlSet[loc]
		if IsRTL(loc) != wantRTL {
			t.Errorf("IsRTL(%q) = %v, want %v", loc, IsRTL(loc), wantRTL)
		}
	}
}

func TestDetectLocalePrecedence(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "tr_TR.UTF-8")
	if got := DetectLocale(); got != "tr" {
		t.Fatalf("LANG only = %q, want tr", got)
	}

	t.Setenv("LC_MESSAGES", "vi_VN.UTF-8")
	if got := DetectLocale(); got != "vi" {
		t.Fatalf("LC_MESSAGES override = %q, want vi", got)
	}

	t.Setenv("LC_ALL", "C")
	if got := DetectLocale(); got != "en" {
		t.Fatalf("LC_ALL=C override = %q, want en", got)
	}
}

func TestEnglishBackwardCompatibility(t *testing.T) {
	cat := New("en")
	if got := cat.T("webhook.http.method_not_allowed"); got != "method not allowed" {
		t.Fatalf("method_not_allowed = %q", got)
	}
	if got := cat.T("router.http.unauthorized"); got != "installation not authorized" {
		t.Fatalf("unauthorized = %q", got)
	}
	if got := cat.T("nonexistent.key"); got != "nonexistent.key" {
		t.Fatalf("unknown key = %q", got)
	}
}
