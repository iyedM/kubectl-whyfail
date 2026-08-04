// Package rules holds the deterministic diagnosis engine.
//
// A rule is a pure function of the collected DiagnosticContext: no network, no
// cluster access, no clock beyond what the context carries. That is what makes
// every rule fully testable from a JSON fixture in testdata/.
package rules

import "github.com/iyedM/kubectl-why-fail/internal/collector"

// DiagnosticContext is the snapshot produced by the collector. It is aliased
// here so rules read naturally and so collector.Collect's result can be handed
// to RunAll unchanged.
type DiagnosticContext = collector.DiagnosticContext

// Rule is one deterministic explanation of a pod failure.
//
// Match decides whether the rule applies; Explain turns the context into a
// human sentence. Explain is only ever called when Match returned true, so it
// may assume the shape Match checked for.
type Rule struct {
	Name    string
	Match   func(ctx *DiagnosticContext) bool
	Explain func(ctx *DiagnosticContext) Diagnosis
}

// Diagnosis is the answer given to the user.
type Diagnosis struct {
	Cause      string
	Suggestion string
	Confidence string // "high" ou "medium"
}

// Confidence levels.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
)

// registry lists every v1 rule in the priority order fixed by CLAUDE.md. The
// first match wins, so the order is part of the contract: do not shuffle it.
//
// Where a broad rule could shadow a narrower one further down the list (the
// generic image-pull rule vs. the architecture-mismatch rule, for instance),
// the broad rule carries an explicit exclusion rather than being demoted.
var registry = []Rule{
	crashLoopProbeRule,
	oomKilledRule,
	imagePullRule,
	pendingResourcesRule,
	pendingPVCRule,
	configErrorRule,
	evictedRule,
	crashLoopCommandRule,
	readinessNeverReadyRule,
	imagePullArchRule,
}

// All returns the registered rules in priority order.
func All() []Rule {
	out := make([]Rule, len(registry))
	copy(out, registry)
	return out
}

// Match is a rule hit: the diagnosis plus the rule that produced it.
type Match struct {
	Rule      string
	Diagnosis Diagnosis
}

// RunAll evaluates the rules in priority order and returns the first match.
// The boolean reports whether any rule matched; when it is false the caller
// may fall back to the LLM.
func RunAll(ctx *DiagnosticContext) (*Diagnosis, bool) {
	m, ok := Evaluate(ctx)
	if !ok {
		return nil, false
	}
	d := m.Diagnosis
	return &d, true
}

// Evaluate is RunAll, but it also reports which rule fired. The CLI uses it to
// name the rule in its output.
func Evaluate(ctx *DiagnosticContext) (*Match, bool) {
	if ctx == nil {
		return nil, false
	}
	for _, r := range registry {
		if r.Match == nil || !r.Match(ctx) {
			continue
		}
		return &Match{Rule: r.Name, Diagnosis: r.Explain(ctx)}, true
	}
	return nil, false
}
