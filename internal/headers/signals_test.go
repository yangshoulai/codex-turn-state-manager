package headers

import (
	"net/http"
	"testing"
	"time"
)

func TestParseSignals(t *testing.T) {
	h := http.Header{}
	h.Set(SignalPlanType, "pro")
	h.Set(SignalPrimaryUsedPercent, "42.6")
	h.Set(SignalPrimaryWindowMinutes, "300")
	h.Set(SignalPrimaryResetAt, "1767225600")
	h.Set(SignalSecondaryUsedPercent, "7")
	h.Set(SignalSecondaryResetAt, "2026-10-01T12:00:00Z")
	h.Set(SignalActiveLimit, "primary")
	h.Set(SignalCreditsBalance, "12.5")

	got := ParseSignals(h)
	if got.PlanType != "pro" {
		t.Errorf("PlanType = %q, want pro", got.PlanType)
	}
	if got.PrimaryUsedPercent == nil || *got.PrimaryUsedPercent != 43 {
		t.Errorf("PrimaryUsedPercent = %v, want 43 (rounded)", got.PrimaryUsedPercent)
	}
	if got.PrimaryWindowMinutes == nil || *got.PrimaryWindowMinutes != 300 {
		t.Errorf("PrimaryWindowMinutes = %v, want 300", got.PrimaryWindowMinutes)
	}
	if got.PrimaryResetAt == nil || !got.PrimaryResetAt.Equal(time.Unix(1767225600, 0).UTC()) {
		t.Errorf("PrimaryResetAt = %v, want the epoch value", got.PrimaryResetAt)
	}
	if got.SecondaryResetAt == nil || got.SecondaryResetAt.UTC().Format(time.RFC3339) != "2026-10-01T12:00:00Z" {
		t.Errorf("SecondaryResetAt = %v, want the RFC3339 value", got.SecondaryResetAt)
	}
	if got.ActiveLimit != "primary" || got.CreditsBalance != "12.5" {
		t.Errorf("ActiveLimit/CreditsBalance = %q/%q", got.ActiveLimit, got.CreditsBalance)
	}
	if got.Empty() {
		t.Error("a populated header set should not report as empty")
	}
}

// TestParseSignalsToleratesAnything pins the contract these headers have: they
// are undocumented, so a missing, empty or unparseable value must leave the
// field unset rather than fail the response that carried it.
func TestParseSignalsToleratesAnything(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
	}{
		{"nil header", nil},
		{"empty", http.Header{}},
		{"unparseable percent", http.Header{SignalPrimaryUsedPercent: []string{"lots"}}},
		{"unparseable window", http.Header{SignalPrimaryWindowMinutes: []string{"a while"}}},
		{"unparseable reset", http.Header{SignalPrimaryResetAt: []string{"soon"}}},
		{"blank values", http.Header{SignalPlanType: []string{"  "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSignals(tc.h)
			if !got.Empty() {
				t.Errorf("Signals = %+v, want zero", got)
			}
		})
	}
}

func TestParseSignalsClampsPercent(t *testing.T) {
	h := http.Header{}
	h.Set(SignalPrimaryUsedPercent, "140")
	if got := ParseSignals(h).PrimaryUsedPercent; got == nil || *got != 100 {
		t.Errorf("over-range percent = %v, want 100", got)
	}

	h.Set(SignalPrimaryUsedPercent, "-5")
	if got := ParseSignals(h).PrimaryUsedPercent; got == nil || *got != 0 {
		t.Errorf("negative percent = %v, want 0", got)
	}
}
