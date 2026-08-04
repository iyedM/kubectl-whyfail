// Package output renders a diagnosis for a terminal.
//
// The one thing this package must never do is let a rule match and an LLM
// guess look alike: a deterministic answer and a plausible-sounding
// hallucination deserve visibly different badges.
package output

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"

	"github.com/iyedM/kubectl-why-fail/internal/collector"
	"github.com/iyedM/kubectl-why-fail/internal/rules"
)

// Source says where a diagnosis came from.
type Source int

const (
	// SourceRule is a deterministic rule match.
	SourceRule Source = iota
	// SourceLLM is an AI guess, shown only when no rule matched.
	SourceLLM
)

// Result is everything the CLI wants to print.
type Result struct {
	Source    Source
	RuleName  string
	Diagnosis rules.Diagnosis
	Context   *collector.DiagnosticContext
}

// Printer renders results. The zero value writes plain output to stdout.
type Printer struct {
	Out  io.Writer
	Lang string
}

// NewPrinter returns a printer writing to w. Colour is disabled automatically
// when w is not a terminal or when NO_COLOR is set, so piping to a file or to
// grep produces clean text.
func NewPrinter(w io.Writer, lang string) *Printer {
	if w == nil {
		w = os.Stdout
	}
	return &Printer{Out: w, Lang: lang}
}

// SetColorEnabled forces colour on or off, overriding auto-detection.
func SetColorEnabled(enabled bool) {
	color.NoColor = !enabled
}

var (
	bold      = color.New(color.Bold)
	dim       = color.New(color.Faint)
	red       = color.New(color.FgRed, color.Bold)
	yellow    = color.New(color.FgYellow, color.Bold)
	green     = color.New(color.FgGreen, color.Bold)
	cyan      = color.New(color.FgCyan, color.Bold)
	ruleBadge = color.New(color.BgGreen, color.FgBlack, color.Bold)
	llmBadge  = color.New(color.BgMagenta, color.FgWhite, color.Bold)
)

func (p *Printer) tr(en, fr string) string {
	if p.Lang == "fr" {
		return fr
	}
	return en
}

// Print renders a full diagnosis.
func (p *Printer) Print(r Result) {
	w := p.writer()

	p.printHeader(r)
	fmt.Fprintln(w)

	// Cause
	red.Fprint(w, p.tr("  WHY IT FAILS", "  POURQUOI ÇA ÉCHOUE"))
	fmt.Fprintln(w)
	fmt.Fprintln(w, indent(r.Diagnosis.Cause, "  "))
	fmt.Fprintln(w)

	// Suggestion
	if strings.TrimSpace(r.Diagnosis.Suggestion) != "" {
		green.Fprint(w, p.tr("  HOW TO FIX IT", "  COMMENT CORRIGER"))
		fmt.Fprintln(w)
		fmt.Fprintln(w, indent(r.Diagnosis.Suggestion, "  "))
		fmt.Fprintln(w)
	}

	p.printFooter(r)
}

// printHeader prints the pod line and the badge that distinguishes a rule
// match from an AI guess.
func (p *Printer) printHeader(r Result) {
	w := p.writer()

	pod := "pod"
	ns := ""
	if r.Context != nil {
		pod = r.Context.Pod.Name
		ns = r.Context.Pod.Namespace
	}

	bold.Fprintf(w, "\n  %s", pod)
	if ns != "" {
		dim.Fprintf(w, "  (namespace: %s)", ns)
	}
	fmt.Fprintln(w)

	if r.Context != nil {
		p.printStatusLine(r.Context)
	}

	fmt.Fprint(w, "  ")
	switch r.Source {
	case SourceLLM:
		llmBadge.Fprint(w, p.tr(" ✦ AI GUESS ", " ✦ HYPOTHÈSE IA "))
		dim.Fprint(w, p.tr(
			"  no rule matched — this is a model's best guess, verify it",
			"  aucune règle n'a matché — hypothèse d'un modèle, à vérifier"))
	default:
		ruleBadge.Fprint(w, p.tr(" ✔ MATCHED RULE ", " ✔ RÈGLE MATCHÉE "))
		if r.RuleName != "" {
			dim.Fprintf(w, "  %s", r.RuleName)
		}
	}
	fmt.Fprintln(w)

	conf := r.Diagnosis.Confidence
	if conf != "" {
		fmt.Fprint(w, "  ")
		dim.Fprint(w, p.tr("confidence: ", "confiance : "))
		if conf == rules.ConfidenceHigh {
			green.Fprintln(w, conf)
		} else {
			yellow.Fprintln(w, conf)
		}
	}
}

// printStatusLine gives the one-line "what state is it in" summary a user
// would otherwise get from kubectl get pod.
func (p *Printer) printStatusLine(dc *collector.DiagnosticContext) {
	w := p.writer()

	state := dc.Pod.Phase
	if dc.Pod.Reason != "" {
		state = dc.Pod.Reason
	}
	for _, c := range dc.Containers {
		if c.State.Type == "Waiting" && c.State.Reason != "" {
			state = c.State.Reason
			break
		}
	}

	restarts := int32(0)
	for _, c := range dc.Containers {
		restarts += c.RestartCount
	}

	fmt.Fprint(w, "  ")
	dim.Fprint(w, p.tr("status: ", "état : "))
	yellow.Fprint(w, state)
	if restarts > 0 {
		dim.Fprintf(w, p.tr("   restarts: %d", "   redémarrages : %d"), restarts)
	}
	if dc.Pod.NodeName != "" {
		dim.Fprintf(w, p.tr("   node: %s", "   node : %s"), dc.Pod.NodeName)
	}
	fmt.Fprintln(w)
}

func (p *Printer) printFooter(r Result) {
	w := p.writer()
	if r.Source != SourceLLM {
		return
	}
	dim.Fprintln(w, p.tr(
		"  ─ AI answers are not verified. Check the commands before running them.",
		"  ─ Les réponses de l'IA ne sont pas vérifiées. Vérifiez les commandes avant de les exécuter."))
	fmt.Fprintln(w)
}

// PrintNoDiagnosis is what the user sees when no rule matched and the LLM
// fallback is unavailable. Saying "I don't know" honestly beats guessing.
func (p *Printer) PrintNoDiagnosis(dc *collector.DiagnosticContext, llmErr error) {
	w := p.writer()

	pod := "pod"
	ns := ""
	if dc != nil {
		pod = dc.Pod.Name
		ns = dc.Pod.Namespace
	}

	bold.Fprintf(w, "\n  %s", pod)
	if ns != "" {
		dim.Fprintf(w, "  (namespace: %s)", ns)
	}
	fmt.Fprintln(w)
	if dc != nil {
		p.printStatusLine(dc)
	}
	fmt.Fprintln(w)

	yellow.Fprintln(w, p.tr(
		"  No rule matched this pod.",
		"  Aucune règle ne correspond à ce pod."))
	fmt.Fprintln(w)

	if llmErr != nil {
		fmt.Fprintln(w, indent(p.tr(
			"The AI fallback did not run: "+llmErr.Error(),
			"Le fallback IA n'a pas été utilisé : "+llmErr.Error()), "  "))
		fmt.Fprintln(w)
		cyan.Fprintln(w, p.tr(
			"  Set OPENROUTER_API_KEY to let an AI take a guess:",
			"  Définissez OPENROUTER_API_KEY pour laisser une IA proposer une hypothèse :"))
		dim.Fprintln(w, "    export OPENROUTER_API_KEY=sk-or-v1-...")
		fmt.Fprintln(w)
	}

	dim.Fprintln(w, p.tr(
		"  If this is a failure mode whyfail should recognise, please open an issue:",
		"  Si whyfail devrait reconnaître ce cas, ouvrez une issue :"))
	dim.Fprintln(w, "    https://github.com/iyedM/kubectl-why-fail/issues/new")
	fmt.Fprintln(w)
}

// PrintHealthy tells the user their pod looks fine, rather than manufacturing
// a problem to justify the command.
func (p *Printer) PrintHealthy(dc *collector.DiagnosticContext) {
	w := p.writer()

	bold.Fprintf(w, "\n  %s", dc.Pod.Name)
	dim.Fprintf(w, "  (namespace: %s)\n", dc.Pod.Namespace)
	fmt.Fprint(w, "  ")
	green.Fprint(w, p.tr(" ✔ LOOKS HEALTHY ", " ✔ TOUT VA BIEN "))
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, indent(p.tr(
		"This pod is Running, all its containers are ready and nothing has restarted.\n"+
			"whyfail has nothing to report.",
		"Ce pod est Running, tous ses conteneurs sont ready et rien n'a redémarré.\n"+
			"whyfail n'a rien à signaler."), "  "))
	fmt.Fprintln(w)
}

// PrintError renders a fatal error.
func (p *Printer) PrintError(err error) {
	w := p.writer()
	fmt.Fprint(w, "  ")
	red.Fprint(w, p.tr("error: ", "erreur : "))
	fmt.Fprintln(w, err)
}

func (p *Printer) writer() io.Writer {
	if p.Out == nil {
		return os.Stdout
	}
	return p.Out
}

// indent prefixes every line, so multi-line causes stay aligned under their
// heading.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
