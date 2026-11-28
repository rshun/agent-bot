package antigravity

import "testing"

func TestParseModels(t *testing.T) {
	raw := "\x1b[32m* gemini-4-hydrogen-medium (current)\x1b[0m\n" +
		"  gemini-4-neon-high             Gemini 4 Neon (High)\n" +
		"  gemini-pro-orchestrator        Orchestrator (Carbon) (default)\n"

	list := ParseModels(raw)
	if list.Current != "gemini-4-hydrogen-medium" {
		t.Fatalf("Current = %q", list.Current)
	}
	if list.Default != "gemini-pro-orchestrator" {
		t.Fatalf("Default = %q", list.Default)
	}
	if len(list.Names) != 3 || list.Names[1] != "gemini-4-neon-high" {
		t.Fatalf("Names = %#v", list.Names)
	}

	noisy := "\x1b[?25l⠋ Fetching models...\rFetching\r\n" +
		"Loading...\n" +
		"Available models:\n" +
		"---\n" +
		"  gemini-4-neon-high       Gemini 4 Neon (High)\n" +
		"  claude-opus-5-5-high     Claude Opus 5.5 (High)\n" +
		"  gpt-oss-120b-medium      GPT-OSS 120B (Medium)\n" +
		"  Fetching\n" +
		"\x1b[?25h"
	cleanList := ParseModels(noisy)
	want := []string{"gemini-4-neon-high", "claude-opus-5-5-high", "gpt-oss-120b-medium"}
	if len(cleanList.Names) != len(want) {
		t.Fatalf("noisy Names = %#v, want %#v", cleanList.Names, want)
	}
	for i, w := range want {
		if cleanList.Names[i] != w {
			t.Fatalf("noisy Names[%d] = %q, want %q", i, cleanList.Names[i], w)
		}
	}
}

func TestParseUsageOutput(t *testing.T) {
	raw := "Account: user@example.com\n" +
		"Plan: Google AI Pro\n" +
		"Gemini Pro (5h): 82% remaining (resets 2026-11-28 01:00:00)\n" +
		"Claude / GPT (weekly): 25% used\n"

	a, err := ParseUsageOutput(raw)
	if err != nil {
		t.Fatalf("ParseUsageOutput: %v", err)
	}
	if a.AccountEmail != "user@example.com" || a.PlanType != "Google AI Pro" {
		t.Fatalf("unexpected metadata: %#v", a)
	}
	if len(a.Buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d: %#v", len(a.Buckets), a.Buckets)
	}
	if a.Buckets[0].Primary.RemainingPercent != 82 || a.Buckets[0].Primary.WindowDurationMins != 300 {
		t.Fatalf("unexpected bucket 0: %#v", a.Buckets[0].Primary)
	}
	if a.Buckets[1].Primary.UsedPercent != 25 || a.Buckets[1].Primary.RemainingPercent != 75 || a.Buckets[1].Primary.WindowDurationMins != 7*24*60 {
		t.Fatalf("unexpected bucket 1: %#v", a.Buckets[1].Primary)
	}

	if _, err := ParseUsageOutput("Quota unavailable when authenticated via GEMINI_API_KEY."); err == nil {
		t.Fatal("expected error for GEMINI_API_KEY mode")
	}
}
