package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFixture reads testdata/<name>.json into a DiagnosticContext. Unknown
// fields are rejected so a fixture can never silently drift away from the
// struct it is meant to describe (a typo'd "livenessprobe" would otherwise
// make a rule quietly stop matching).
func loadFixture(t *testing.T, name string) *DiagnosticContext {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}

	// Fixtures carry a "_comment" key documenting the scenario; strip it
	// before the strict decode.
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", name, err)
	}
	delete(generic, "_comment")
	cleaned, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-encoding fixture %s: %v", name, err)
	}

	dec := json.NewDecoder(strings.NewReader(string(cleaned)))
	dec.DisallowUnknownFields()

	var ctx DiagnosticContext
	if err := dec.Decode(&ctx); err != nil {
		t.Fatalf("decoding fixture %s: %v", name, err)
	}
	return &ctx
}

// assertMatches checks that a rule fires on a fixture and produces a usable
// diagnosis in both languages.
func assertMatches(t *testing.T, r Rule, fixture string) Diagnosis {
	t.Helper()

	ctx := loadFixture(t, fixture)
	if !r.Match(ctx) {
		t.Fatalf("rule %s should match fixture %s, but it did not", r.Name, fixture)
	}

	d := r.Explain(ctx)
	if strings.TrimSpace(d.Cause) == "" {
		t.Errorf("rule %s produced an empty Cause on %s", r.Name, fixture)
	}
	if strings.TrimSpace(d.Suggestion) == "" {
		t.Errorf("rule %s produced an empty Suggestion on %s", r.Name, fixture)
	}
	if d.Confidence != ConfidenceHigh && d.Confidence != ConfidenceMedium {
		t.Errorf("rule %s produced Confidence %q, want %q or %q", r.Name, d.Confidence, ConfidenceHigh, ConfidenceMedium)
	}

	// The French output must be a real translation, not a fallback to English.
	fr := *loadFixture(t, fixture)
	fr.Lang = "fr"
	dfr := r.Explain(&fr)
	if dfr.Cause == d.Cause {
		t.Errorf("rule %s returned the same Cause for --lang fr and --lang en", r.Name)
	}
	if strings.TrimSpace(dfr.Suggestion) == "" {
		t.Errorf("rule %s produced an empty French Suggestion on %s", r.Name, fixture)
	}

	return d
}

// assertDoesNotMatch guards against false positives, which are worse than a
// missed diagnosis: a confident wrong answer sends the user down the wrong path.
func assertDoesNotMatch(t *testing.T, r Rule, fixture string) {
	t.Helper()

	ctx := loadFixture(t, fixture)
	if r.Match(ctx) {
		t.Fatalf("rule %s must NOT match fixture %s (false positive): %+v", r.Name, fixture, r.Explain(ctx))
	}
}

func TestCrashLoopProbeRule(t *testing.T) {
	d := assertMatches(t, crashLoopProbeRule, "crashloop_probe_match")
	if !strings.Contains(d.Cause, "liveness") {
		t.Errorf("cause should name the liveness probe, got: %s", d.Cause)
	}
	if !strings.Contains(d.Suggestion, "startupProbe") {
		t.Errorf("suggestion should offer a startupProbe, got: %s", d.Suggestion)
	}

	assertDoesNotMatch(t, crashLoopProbeRule, "crashloop_probe_nomatch")
	// An OOM kill also restarts a container that happens to have a probe; the
	// memory limit is the real cause and rule 2 owns it.
	assertDoesNotMatch(t, crashLoopProbeRule, "oomkilled_match")
	assertDoesNotMatch(t, crashLoopProbeRule, "healthy")
}

func TestOOMKilledRule(t *testing.T) {
	d := assertMatches(t, oomKilledRule, "oomkilled_match")
	if !strings.Contains(d.Cause, "128Mi") {
		t.Errorf("cause should quote the memory limit, got: %s", d.Cause)
	}
	if !strings.Contains(d.Suggestion, "256Mi") {
		t.Errorf("suggestion should propose a concrete larger limit, got: %s", d.Suggestion)
	}

	assertDoesNotMatch(t, oomKilledRule, "oomkilled_nomatch")
	assertDoesNotMatch(t, oomKilledRule, "healthy")
}

func TestImagePullRule(t *testing.T) {
	d := assertMatches(t, imagePullRule, "imagepull_match")
	if !strings.Contains(d.Cause, "ghcr.io/acme/billing-worker:v2.3.1") {
		t.Errorf("cause should quote the image, got: %s", d.Cause)
	}
	if !strings.Contains(strings.ToLower(d.Cause), "does not exist") {
		t.Errorf("a 'not found' registry error should be reported as a missing tag/repo, got: %s", d.Cause)
	}

	// The credentials variant must be told apart from the missing-tag variant.
	authCtx := loadFixture(t, "imagepull_arch_nomatch")
	if !imagePullRule.Match(authCtx) {
		t.Fatal("imagepull rule should own the authorization failure fixture")
	}
	if got := imagePullRule.Explain(authCtx); !strings.Contains(strings.ToLower(got.Cause), "unauthenticated") {
		t.Errorf("an authorization failure should be reported as a credentials problem, got: %s", got.Cause)
	}

	assertDoesNotMatch(t, imagePullRule, "imagepull_nomatch")
	// An architecture mismatch also surfaces as a failed pull: rule 10 owns it.
	assertDoesNotMatch(t, imagePullRule, "imagepull_arch_match")
	assertDoesNotMatch(t, imagePullRule, "healthy")
}

func TestPendingResourcesRule(t *testing.T) {
	d := assertMatches(t, pendingResourcesRule, "pending_resources_match")
	if !strings.Contains(d.Cause, "cpu and memory") {
		t.Errorf("cause should list both scarce resources, got: %s", d.Cause)
	}
	if !strings.Contains(d.Cause, "48Gi") {
		t.Errorf("cause should quote what the pod requests, got: %s", d.Cause)
	}

	assertDoesNotMatch(t, pendingResourcesRule, "pending_resources_nomatch")
	assertDoesNotMatch(t, pendingResourcesRule, "pending_pvc_match")
	assertDoesNotMatch(t, pendingResourcesRule, "healthy")
}

func TestPendingPVCRule(t *testing.T) {
	d := assertMatches(t, pendingPVCRule, "pending_pvc_match")
	if !strings.Contains(d.Cause, "data-postgres-0") {
		t.Errorf("cause should name the claim, got: %s", d.Cause)
	}
	if !strings.Contains(d.Cause, "fast-ssd") {
		t.Errorf("cause should surface the provisioner error about the missing StorageClass, got: %s", d.Cause)
	}

	assertDoesNotMatch(t, pendingPVCRule, "pending_pvc_nomatch")
	assertDoesNotMatch(t, pendingPVCRule, "healthy")
}

func TestConfigErrorRule(t *testing.T) {
	d := assertMatches(t, configErrorRule, "configerror_match")
	if !strings.Contains(d.Cause, "notifier-config") {
		t.Errorf("cause should name the missing ConfigMap, got: %s", d.Cause)
	}
	if !strings.Contains(d.Cause, "SMTP_HOST") {
		t.Errorf("cause should tie the ConfigMap back to the env var that needs it, got: %s", d.Cause)
	}

	assertDoesNotMatch(t, configErrorRule, "configerror_nomatch")
	assertDoesNotMatch(t, configErrorRule, "healthy")
}

func TestEvictedRule(t *testing.T) {
	d := assertMatches(t, evictedRule, "evicted_match")
	if !strings.Contains(d.Cause, "ephemeral-storage") {
		t.Errorf("cause should name the exhausted resource, got: %s", d.Cause)
	}
	if !strings.Contains(d.Cause, "BestEffort") {
		t.Errorf("cause should explain why THIS pod was chosen (QoS class), got: %s", d.Cause)
	}

	assertDoesNotMatch(t, evictedRule, "evicted_nomatch")
	assertDoesNotMatch(t, evictedRule, "healthy")
}

func TestCrashLoopCommandRule(t *testing.T) {
	d := assertMatches(t, crashLoopCommandRule, "crashloop_command_match")
	if !strings.Contains(d.Cause, "127") {
		t.Errorf("cause should explain exit code 127, got: %s", d.Cause)
	}
	if !strings.Contains(d.Cause, "/app/migrate.sh") {
		t.Errorf("cause should quote the failing command, got: %s", d.Cause)
	}

	assertDoesNotMatch(t, crashLoopCommandRule, "crashloop_command_nomatch")
	// "exec format error" is the same symptom with a different cause (rule 10).
	assertDoesNotMatch(t, crashLoopCommandRule, "imagepull_arch_match")
	assertDoesNotMatch(t, crashLoopCommandRule, "healthy")
}

func TestReadinessNeverReadyRule(t *testing.T) {
	d := assertMatches(t, readinessNeverReadyRule, "readiness_never_ready_match")
	if !strings.Contains(d.Cause, "8080") || !strings.Contains(d.Cause, "3000") {
		t.Errorf("cause should contrast the probed port with the declared port, got: %s", d.Cause)
	}
	if !strings.Contains(d.Suggestion, "0.0.0.0") {
		t.Errorf("suggestion should mention the loopback-bind trap, got: %s", d.Suggestion)
	}

	assertDoesNotMatch(t, readinessNeverReadyRule, "readiness_never_ready_nomatch")
	assertDoesNotMatch(t, readinessNeverReadyRule, "crashloop_probe_match")
	assertDoesNotMatch(t, readinessNeverReadyRule, "healthy")
}

func TestImagePullArchRule(t *testing.T) {
	d := assertMatches(t, imagePullArchRule, "imagepull_arch_match")
	if !strings.Contains(d.Cause, "linux/amd64") {
		t.Errorf("cause should name the platform the node needs, got: %s", d.Cause)
	}
	if !strings.Contains(d.Suggestion, "buildx") {
		t.Errorf("suggestion should point at a multi-arch build, got: %s", d.Suggestion)
	}

	assertDoesNotMatch(t, imagePullArchRule, "imagepull_arch_nomatch")
	assertDoesNotMatch(t, imagePullArchRule, "imagepull_match")
	assertDoesNotMatch(t, imagePullArchRule, "healthy")
}

// TestRunAllPriority is the guard on the registry order: it asserts that each
// scenario is claimed by the rule that owns it, and not by an earlier, broader
// rule. It fails the moment a new rule shadows an existing one.
func TestRunAllPriority(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{"crashloop_probe_match", "crashloop_probe"},
		{"oomkilled_match", "oomkilled"},
		{"imagepull_match", "imagepull"},
		{"pending_resources_match", "pending_resources"},
		{"pending_pvc_match", "pending_pvc"},
		{"configerror_match", "configerror"},
		{"evicted_match", "evicted"},
		{"crashloop_command_match", "crashloop_command"},
		{"readiness_never_ready_match", "readiness_never_ready"},
		{"imagepull_arch_match", "imagepull_arch"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			ctx := loadFixture(t, tc.fixture)
			m, ok := Evaluate(ctx)
			if !ok {
				t.Fatalf("no rule matched %s, expected %s", tc.fixture, tc.want)
			}
			if m.Rule != tc.want {
				t.Fatalf("%s was claimed by rule %q, expected %q — check the registry order in rule.go", tc.fixture, m.Rule, tc.want)
			}

			// RunAll must agree with Evaluate.
			d, ok := RunAll(ctx)
			if !ok || d == nil {
				t.Fatal("RunAll disagreed with Evaluate")
			}
			if d.Cause != m.Diagnosis.Cause {
				t.Error("RunAll and Evaluate returned different diagnoses")
			}
		})
	}
}

// TestRegistryCoversV1Scope pins the v1 scope: exactly the ten rules from
// CLAUDE.md, in that order.
func TestRegistryCoversV1Scope(t *testing.T) {
	want := []string{
		"crashloop_probe",
		"oomkilled",
		"imagepull",
		"pending_resources",
		"pending_pvc",
		"configerror",
		"evicted",
		"crashloop_command",
		"readiness_never_ready",
		"imagepull_arch",
	}

	got := All()
	if len(got) != len(want) {
		t.Fatalf("registry has %d rules, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("rule %d is %q, want %q", i, got[i].Name, name)
		}
		if got[i].Match == nil || got[i].Explain == nil {
			t.Errorf("rule %q has a nil Match or Explain", got[i].Name)
		}
	}
}

// TestNoRuleMatchesHealthyPod is the anti-false-positive backstop: a healthy
// pod must fall all the way through so the CLI can say "nothing wrong here"
// rather than inventing a problem.
func TestNoRuleMatchesHealthyPod(t *testing.T) {
	ctx := loadFixture(t, "healthy")
	for _, r := range All() {
		if r.Match(ctx) {
			t.Errorf("rule %s matched a healthy pod: %+v", r.Name, r.Explain(ctx))
		}
	}
	if _, ok := RunAll(ctx); ok {
		t.Error("RunAll matched a healthy pod")
	}
}

// TestRunAllHandlesNil documents that a nil context is a miss, not a panic.
func TestRunAllHandlesNil(t *testing.T) {
	if _, ok := RunAll(nil); ok {
		t.Error("RunAll(nil) should not match")
	}
	if _, ok := Evaluate(nil); ok {
		t.Error("Evaluate(nil) should not match")
	}
}

// TestRulesAreSideEffectFree makes sure Match/Explain do not mutate the
// context, so evaluation order can never change a later rule's answer.
func TestRulesAreSideEffectFree(t *testing.T) {
	for _, fixture := range []string{"crashloop_probe_match", "oomkilled_match", "imagepull_match", "evicted_match"} {
		before, err := json.Marshal(loadFixture(t, fixture))
		if err != nil {
			t.Fatal(err)
		}

		ctx := loadFixture(t, fixture)
		for _, r := range All() {
			if r.Match(ctx) {
				r.Explain(ctx)
			}
		}

		after, err := json.Marshal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Errorf("evaluating rules mutated the context for fixture %s", fixture)
		}
	}
}
