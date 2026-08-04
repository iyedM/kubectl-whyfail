package output

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/iyedM/kubectl-why-fail/internal/collector"
	"github.com/iyedM/kubectl-why-fail/internal/rules"
)

func init() {
	// Deterministic output in tests: no escape codes to assert around.
	SetColorEnabled(false)
}

func sampleContext() *collector.DiagnosticContext {
	return &collector.DiagnosticContext{
		Pod: collector.PodInfo{
			Name:      "checkout-api-7d9f8b6c-x2kpl",
			Namespace: "production",
			Phase:     "Running",
			NodeName:  "node-a",
		},
		Containers: []collector.ContainerInfo{
			{
				Name:         "api",
				RestartCount: 7,
				State:        collector.ContainerState{Type: "Waiting", Reason: "CrashLoopBackOff"},
			},
		},
	}
}

func render(t *testing.T, r Result, lang string) string {
	t.Helper()
	var buf bytes.Buffer
	NewPrinter(&buf, lang).Print(r)
	return buf.String()
}

func TestPrintRuleResult(t *testing.T) {
	out := render(t, Result{
		Source:   SourceRule,
		RuleName: "crashloop_probe",
		Diagnosis: rules.Diagnosis{
			Cause:      "The liveness probe kills the container before it finishes booting.",
			Suggestion: "Add a startupProbe.",
			Confidence: rules.ConfidenceHigh,
		},
		Context: sampleContext(),
	}, "en")

	for _, want := range []string{
		"checkout-api-7d9f8b6c-x2kpl",
		"production",
		"MATCHED RULE",
		"crashloop_probe",
		"high",
		"WHY IT FAILS",
		"liveness probe kills",
		"HOW TO FIX IT",
		"startupProbe",
		"CrashLoopBackOff",
		"restarts: 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "AI GUESS") {
		t.Error("a rule match must not be labelled as an AI guess")
	}
}

// TestBadgesAreDistinct is the point of this package: a deterministic answer
// and a model's guess must never be mistaken for one another.
func TestBadgesAreDistinct(t *testing.T) {
	d := rules.Diagnosis{Cause: "Something broke.", Suggestion: "Fix it.", Confidence: rules.ConfidenceHigh}

	ruleOut := render(t, Result{Source: SourceRule, RuleName: "oomkilled", Diagnosis: d, Context: sampleContext()}, "en")
	llmOut := render(t, Result{Source: SourceLLM, Diagnosis: rules.Diagnosis{
		Cause: "Something broke.", Suggestion: "Fix it.", Confidence: rules.ConfidenceMedium,
	}, Context: sampleContext()}, "en")

	if !strings.Contains(ruleOut, "MATCHED RULE") {
		t.Error("rule result should carry the rule badge")
	}
	if strings.Contains(ruleOut, "AI GUESS") || strings.Contains(ruleOut, "not verified") {
		t.Error("rule result should carry no AI framing at all")
	}

	if !strings.Contains(llmOut, "AI GUESS") {
		t.Error("LLM result should carry the AI badge")
	}
	if strings.Contains(llmOut, "MATCHED RULE") {
		t.Error("LLM result must not claim a rule matched")
	}
	if !strings.Contains(llmOut, "not verified") {
		t.Error("LLM result should warn that the answer is unverified")
	}
	if !strings.Contains(llmOut, "medium") {
		t.Error("LLM result should show medium confidence")
	}
}

func TestPrintFrench(t *testing.T) {
	out := render(t, Result{
		Source:   SourceRule,
		RuleName: "oomkilled",
		Diagnosis: rules.Diagnosis{
			Cause:      "Le conteneur a été tué par l'OOM killer.",
			Suggestion: "Augmentez la limite mémoire.",
			Confidence: rules.ConfidenceHigh,
		},
		Context: sampleContext(),
	}, "fr")

	for _, want := range []string{"POURQUOI ÇA ÉCHOUE", "COMMENT CORRIGER", "RÈGLE MATCHÉE", "redémarrages : 7"} {
		if !strings.Contains(out, want) {
			t.Errorf("French output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WHY IT FAILS") {
		t.Error("French output should not contain English headings")
	}
}

func TestPrintOmitsEmptySuggestion(t *testing.T) {
	out := render(t, Result{
		Source:    SourceLLM,
		Diagnosis: rules.Diagnosis{Cause: "Unclear.", Confidence: rules.ConfidenceMedium},
		Context:   sampleContext(),
	}, "en")

	if strings.Contains(out, "HOW TO FIX IT") {
		t.Error("the fix heading should be omitted when there is no suggestion")
	}
}

func TestPrintNoDiagnosisGuidesTheUser(t *testing.T) {
	var buf bytes.Buffer
	NewPrinter(&buf, "en").PrintNoDiagnosis(sampleContext(), errors.New("no OPENROUTER_API_KEY set"))
	out := buf.String()

	for _, want := range []string{"No rule matched", "OPENROUTER_API_KEY", "issues/new"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestPrintHealthy(t *testing.T) {
	var buf bytes.Buffer
	NewPrinter(&buf, "en").PrintHealthy(sampleContext())
	out := buf.String()

	if !strings.Contains(out, "LOOKS HEALTHY") {
		t.Errorf("output is missing the healthy badge:\n%s", out)
	}
	if !strings.Contains(out, "nothing to report") {
		t.Errorf("output should say there is nothing to report:\n%s", out)
	}
}

func TestMultiLineCauseStaysIndented(t *testing.T) {
	out := render(t, Result{
		Source:   SourceRule,
		RuleName: "imagepull",
		Diagnosis: rules.Diagnosis{
			Cause:      "line one\nline two\nline three",
			Suggestion: "do a\ndo b",
			Confidence: rules.ConfidenceHigh,
		},
		Context: sampleContext(),
	}, "en")

	for _, line := range []string{"  line one", "  line two", "  line three", "  do a", "  do b"} {
		if !strings.Contains(out, line) {
			t.Errorf("expected indented line %q in:\n%s", line, out)
		}
	}
}

func TestPrinterToleratesMissingContext(t *testing.T) {
	// A diagnosis without a context must not panic the CLI.
	var buf bytes.Buffer
	NewPrinter(&buf, "en").Print(Result{
		Source:    SourceLLM,
		Diagnosis: rules.Diagnosis{Cause: "c", Suggestion: "s", Confidence: rules.ConfidenceMedium},
	})
	if buf.Len() == 0 {
		t.Error("expected some output even without a context")
	}
}
