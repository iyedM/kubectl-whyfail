// Command whyfail is a kubectl plugin that explains why a pod is failing.
//
// It runs a strict pipeline: collect the pod's state, try the deterministic
// rules, and only if none of them matched, ask an LLM. The LLM is never
// consulted when a rule has already produced an answer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/iyedM/kubectl-why-fail/internal/collector"
	"github.com/iyedM/kubectl-why-fail/internal/llmfallback"
	"github.com/iyedM/kubectl-why-fail/internal/output"
	"github.com/iyedM/kubectl-why-fail/internal/rules"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `kubectl-why-fail — explain why a Kubernetes pod is failing, in plain language.

Usage:
  kubectl whyfail pod <name> [-n namespace] [flags]

Flags:
  -n, --namespace string   namespace of the pod (default: the current context's namespace)
      --lang string        output language: en or fr (default "en")
      --no-ai              never call the LLM fallback, even if a rule fails to match
      --no-color           disable coloured output
      --kubeconfig string  path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)
      --context string     kubeconfig context to use
      --timeout duration   overall time budget (default 60s)
  -v, --version            print the version and exit
  -h, --help               show this help

Examples:
  kubectl whyfail pod my-app-7d9f8b6c-x2kpl -n production
  kubectl whyfail pod my-app-7d9f8b6c-x2kpl --lang fr
  kubectl whyfail pod my-app-7d9f8b6c-x2kpl --no-ai

The AI fallback is optional and off unless OPENROUTER_API_KEY is set. It only
ever runs when none of the built-in rules matched.
`

type options struct {
	namespace   string
	lang        string
	noAI        bool
	noColor     bool
	kubeconfig  string
	kubeContext string
	timeout     time.Duration
	showVer     bool
	podName     string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, usage)
		os.Exit(2)
	}
	if opts == nil {
		// --help / --version already handled.
		return
	}

	if opts.noColor {
		output.SetColorEnabled(false)
	}
	printer := output.NewPrinter(os.Stdout, opts.lang)

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	// Ctrl-C should abort an in-flight LLM call cleanly.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		<-stop
		cancel()
	}()

	if err := run(ctx, opts, printer); err != nil {
		printer.PrintError(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opts *options, printer *output.Printer) error {
	client, namespace, err := connect(opts)
	if err != nil {
		return err
	}

	dc, err := collector.CollectWithContext(ctx, client, namespace, opts.podName)
	if err != nil {
		return err
	}
	dc.Lang = opts.lang

	// The LLM client is built lazily and only handed to the pipeline when the
	// user has opted in; --no-ai keeps it nil so it cannot be called at all.
	var llm explainer
	var llmErr error
	if opts.noAI {
		llmErr = errors.New("--no-ai was set")
	} else if c, err := llmfallback.New(); err != nil {
		llmErr = err
	} else {
		llm = c
	}

	res, err := diagnose(ctx, dc, llm)
	switch {
	case err != nil:
		printer.PrintNoDiagnosis(dc, err)
	case res == nil:
		if looksHealthy(dc) {
			printer.PrintHealthy(dc)
		} else {
			printer.PrintNoDiagnosis(dc, llmErr)
		}
	default:
		printer.Print(*res)
	}
	return nil
}

// explainer is the LLM fallback seen from the orchestration layer. Keeping it
// an interface is what lets the integration test prove the fallback is never
// called once a rule has matched.
type explainer interface {
	Explain(ctx context.Context, dc *collector.DiagnosticContext) (*rules.Diagnosis, error)
}

// diagnose runs the pipeline: rules first, LLM only as a fallback.
//
// The ordering here is the core contract of the tool. A rule match returns
// immediately, and llm.Explain is unreachable from that path.
func diagnose(ctx context.Context, dc *collector.DiagnosticContext, llm explainer) (*output.Result, error) {
	if m, ok := rules.Evaluate(dc); ok {
		return &output.Result{
			Source:    output.SourceRule,
			RuleName:  m.Rule,
			Diagnosis: m.Diagnosis,
			Context:   dc,
		}, nil
	}

	// No rule matched. Only now may the model be asked.
	if llm == nil {
		return nil, nil
	}

	d, err := llm.Explain(ctx, dc)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, nil
	}

	return &output.Result{
		Source:    output.SourceLLM,
		Diagnosis: *d,
		Context:   dc,
	}, nil
}

// looksHealthy reports whether there is visibly nothing wrong, so the CLI can
// say so instead of implying a hidden problem.
func looksHealthy(dc *collector.DiagnosticContext) bool {
	if dc == nil || !strings.EqualFold(dc.Pod.Phase, "Running") {
		return false
	}
	for _, c := range dc.Containers {
		if c.IsInit {
			continue
		}
		if !c.Ready || c.RestartCount > 0 || c.State.Type != "Running" {
			return false
		}
	}
	return true
}

// connect builds a clientset from the standard kubeconfig resolution rules and
// returns the namespace to use.
func connect(opts *options) (kubernetes.Interface, string, error) {
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.kubeconfig != "" {
		loading.ExplicitPath = opts.kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if opts.kubeContext != "" {
		overrides.CurrentContext = opts.kubeContext
	}

	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides)

	namespace := opts.namespace
	if namespace == "" {
		ns, _, err := cfg.Namespace()
		if err != nil || ns == "" {
			ns = "default"
		}
		namespace = ns
	}

	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("could not load kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, "", fmt.Errorf("could not connect to the cluster: %w", err)
	}
	return client, namespace, nil
}

// parseArgs handles the `pod <name>` sub-command shape kubectl plugins use.
// It returns (nil, nil) when the command has already been fully handled
// (--help, --version).
func parseArgs(args []string) (*options, error) {
	opts := &options{}

	fs := flag.NewFlagSet("whyfail", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	fs.StringVar(&opts.namespace, "namespace", "", "namespace of the pod")
	fs.StringVar(&opts.namespace, "n", "", "namespace of the pod (shorthand)")
	fs.StringVar(&opts.lang, "lang", "en", "output language: en or fr")
	fs.BoolVar(&opts.noAI, "no-ai", false, "never call the LLM fallback")
	fs.BoolVar(&opts.noColor, "no-color", false, "disable coloured output")
	fs.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	fs.StringVar(&opts.kubeContext, "context", "", "kubeconfig context to use")
	fs.DurationVar(&opts.timeout, "timeout", 60*time.Second, "overall time budget")
	fs.BoolVar(&opts.showVer, "version", false, "print the version and exit")
	fs.BoolVar(&opts.showVer, "v", false, "print the version and exit (shorthand)")

	// Split positionals from flags so both orders work:
	//   whyfail pod NAME -n ns   and   whyfail -n ns pod NAME
	var positional []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			flagArgs = append(flagArgs, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			flagArgs = append(flagArgs, a)
			// A flag written as "-n prod" consumes the next argument; one
			// written as "-n=prod" does not.
			if !strings.Contains(a, "=") && takesValue(strings.TrimLeft(a, "-")) && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		default:
			positional = append(positional, a)
		}
	}

	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, err
	}

	if opts.showVer {
		fmt.Printf("kubectl-why-fail %s\n", version)
		return nil, nil
	}

	switch opts.lang {
	case "en", "fr":
	default:
		return nil, fmt.Errorf("unsupported --lang %q: use \"en\" or \"fr\"", opts.lang)
	}

	if opts.timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive")
	}

	// Accept both "whyfail pod NAME" and the shorter "whyfail NAME".
	switch len(positional) {
	case 0:
		return nil, errors.New("no pod given")
	case 1:
		if strings.EqualFold(positional[0], "pod") {
			return nil, errors.New("no pod name given after \"pod\"")
		}
		opts.podName = positional[0]
	default:
		if !strings.EqualFold(positional[0], "pod") && !strings.EqualFold(positional[0], "pods") {
			return nil, fmt.Errorf("unknown subcommand %q: expected \"pod\"", positional[0])
		}
		opts.podName = positional[1]
	}

	// "pod/my-app-x2kpl" is how kubectl itself accepts a resource.
	if _, name, found := strings.Cut(opts.podName, "/"); found {
		opts.podName = name
	}

	if opts.podName == "" {
		return nil, errors.New("no pod name given")
	}
	return opts, nil
}

// takesValue reports whether a flag name expects a following argument.
func takesValue(name string) bool {
	name, _, _ = strings.Cut(name, "=")
	switch name {
	case "n", "namespace", "lang", "kubeconfig", "context", "timeout":
		return true
	}
	return false
}
