package llm

import (
	"strings"
	"testing"
)

func TestReasoningEffortValidity(t *testing.T) {
	for _, effort := range []ReasoningEffort{
		ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax,
	} {
		if !effort.Standard() || !effort.Valid() {
			t.Errorf("%q: standard = %t, valid = %t, want both", effort, effort.Standard(), effort.Valid())
		}
	}
	// Provider-specific levels pass through; the provider decides whether it accepts them.
	for _, effort := range []ReasoningEffort{"minimal", "none", "default", "turbo", "Level-2", "v1.5_fast", ReasoningEffort(strings.Repeat("a", 64))} {
		if effort.Standard() || !effort.Valid() {
			t.Errorf("%q: standard = %t, valid = %t, want provider-specific", effort, effort.Standard(), effort.Valid())
		}
	}
	for _, effort := range []ReasoningEffort{"", " ", "high ", "max effort", "high\n", "a/b", `"high"`, "ünicode", ReasoningEffort(strings.Repeat("a", 65))} {
		if effort.Valid() {
			t.Errorf("%q: valid, want invalid", effort)
		}
	}
}
