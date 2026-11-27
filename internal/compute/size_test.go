// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"strings"
	"testing"
	"text/template"
)

func TestValidInstanceSize(t *testing.T) {
	valid := []string{
		"s-4vcpu-8gb-amd",
		"c-8",
		"m7i.xlarge",
		"c7g.2xlarge",
		"Standard_D4s_v5",
		"n2-standard-4",
		"vc2-4c-8gb",
		"voc-c-4c-8gb-75s-amd",
		"g6-standard-4",
		"g6-dedicated-8",
		"VM.Standard.E4.Flex",
	}
	for _, size := range valid {
		if !ValidInstanceSize(size) {
			t.Fatalf("ValidInstanceSize(%q) = false", size)
		}
	}

	invalid := []string{
		"VM.Standard.E4.Flex:4:16", "m7i.xlarge:4:16",
		"",
		"-leading-hyphen",
		".leading-dot",
		"has space",
		"path/traversal",
		"bad..size",
		"size;rm -rf /",
		"size=evil",
		"size\nnewline",
		"VM.Standard.E4.Flex:bad:16",
		"VM.Standard.E4.Flex:4:",
		strings.Repeat("a", 65),
	}
	for _, size := range invalid {
		if ValidInstanceSize(size) {
			t.Fatalf("ValidInstanceSize(%q) = true", size)
		}
	}
}

func TestParseLabelSizeMap(t *testing.T) {
	if got, err := ParseLabelSizeMap("   "); err != nil || got != nil {
		t.Fatalf("empty map = %v, %v", got, err)
	}

	got, err := ParseLabelSizeMap("Large=c-8, gpu = g5.xlarge , xlarge=Standard_D8s_v5")
	if err != nil {
		t.Fatalf("ParseLabelSizeMap error = %v", err)
	}
	want := map[string]string{
		"large":  "c-8",
		"gpu":    "g5.xlarge",
		"xlarge": "Standard_D8s_v5",
	}
	if len(got) != len(want) {
		t.Fatalf("map length = %d, want %d", len(got), len(want))
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Fatalf("map[%q] = %q, want %q", key, got[key], wantVal)
		}
	}

	for _, bad := range []string{
		"large",
		"large=",
		"=c-8",
		"large=c-8,",
		"large=c-8,,small=c-2",
		"large=c-8,LARGE=c-16",
		"bad label=c-8",
		"large=bad/size",
		"large=bad..size",
		"large=c-8=extra",
	} {
		if _, err := ParseLabelSizeMap(bad); err == nil {
			t.Fatalf("ParseLabelSizeMap(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestResolveInstanceSize(t *testing.T) {
	sizeMap := map[string]string{
		"self-hosted": "c-4",
		"large":       "c-8",
		"xlarge":      "c-16",
	}
	if got := ResolveInstanceSize([]string{"self-hosted", "large"}, "self-hosted", sizeMap); got != "c-8" {
		t.Fatalf("non-required label precedence = %q, want c-8", got)
	}
	if got := ResolveInstanceSize([]string{"LARGE", "self-hosted"}, "self-hosted", sizeMap); got != "c-8" {
		t.Fatalf("case-insensitive precedence = %q, want c-8", got)
	}
	if got := ResolveInstanceSize([]string{"self-hosted"}, "self-hosted", sizeMap); got != "c-4" {
		t.Fatalf("required label fallback = %q, want c-4", got)
	}
	if got := ResolveInstanceSize([]string{"self-hosted", "linux"}, "self-hosted", map[string]string{"large": "c-8"}); got != "" {
		t.Fatalf("unmatched labels = %q, want empty", got)
	}
	if got := ResolveInstanceSize([]string{"self-hosted", "large"}, "self-hosted", nil); got != "" {
		t.Fatalf("nil map = %q, want empty", got)
	}
}

func TestRenderCloudInitRejectsOutOfRangeDeadman(t *testing.T) {
	tmpl := template.Must(template.New("runner").Parse("{{.DeadmanShutdownMinutes}}"))
	if _, err := RenderCloudInit(tmpl, RunnerParams{DeadmanShutdownMinutes: -1}); err == nil {
		t.Fatal("accepted negative dead-man minutes")
	}
	if _, err := RenderCloudInit(tmpl, RunnerParams{DeadmanShutdownMinutes: MaxDeadmanShutdownMinutes + 1}); err == nil {
		t.Fatal("accepted excessive dead-man minutes")
	}
	out, err := RenderCloudInit(tmpl, RunnerParams{DeadmanShutdownMinutes: DefaultDeadmanShutdownMinutes})
	if err != nil || out != "180" {
		t.Fatalf("RenderCloudInit = %q, %v", out, err)
	}
}
