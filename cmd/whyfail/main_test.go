package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/iyedM/kubectl-whyfail/internal/collector"
	"github.com/iyedM/kubectl-whyfail/internal/output"
	"github.com/iyedM/kubectl-whyfail/internal/rules"
)

// spyLLM records whether the fallback was consulted. Any call is a contract
// violation on a path where a rule already answered.
type spyLLM struct {
	calls  int
	answer *rules.Diagnosis
	err    error
}

func (s *spyLLM) Explain(_ context.Context, _ *collector.DiagnosticContext) (*rules.Diagnosis, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.answer != nil {
		return s.answer, nil
	}
	return &rules.Diagnosis{
		Cause:      "the model's guess",
		Suggestion: "the model's suggestion",
		Confidence: rules.ConfidenceMedium,
	}, nil
}

// oomPod builds a pod that rule 2 matches with certainty.
func oomPod() *corev1.Pod {
	started := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "resizer-1", Namespace: "media", UID: "uid-oom"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name:  "resizer",
				Image: "media/resizer:1.4.0",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "resizer",
				Started:      &started,
				RestartCount: 5,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
				},
			}},
		},
	}
}

// mysteryPod is broken in a way none of the ten rules describes, so the
// fallback is allowed to run.
func mysteryPod() *corev1.Pod {
	started := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mystery-1", Namespace: "default", UID: "uid-mystery"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "app", Image: "app:1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "app",
				Ready:   false,
				Started: &started,
				State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func healthyPod() *corev1.Pod {
	started := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fine-1", Namespace: "default", UID: "uid-fine"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "app:1",
				Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
				}},
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "app",
				Ready:   true,
				Started: &started,
				State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func collect(t *testing.T, pod *corev1.Pod, extra ...runtime.Object) *collector.DiagnosticContext {
	t.Helper()
	objs := append([]runtime.Object{pod}, extra...)

	dc, err := collector.Collect(fake.NewSimpleClientset(objs...), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return dc
}

// TestLLMIsNotCalledWhenARuleMatched is the contract from CLAUDE.md: the LLM
// fallback must be unreachable once a rule has produced an answer.
func TestLLMIsNotCalledWhenARuleMatched(t *testing.T) {
	dc := collect(t, oomPod())
	spy := &spyLLM{}

	res, err := diagnose(context.Background(), dc, spy)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if res == nil {
		t.Fatal("expected a diagnosis for an OOMKilled pod")
	}

	if spy.calls != 0 {
		t.Fatalf("the LLM fallback was called %d time(s) even though rule %q matched", spy.calls, res.RuleName)
	}
	if res.Source != output.SourceRule {
		t.Errorf("source = %v, want SourceRule", res.Source)
	}
	if res.RuleName != "oomkilled" {
		t.Errorf("rule = %q, want oomkilled", res.RuleName)
	}
	if res.Diagnosis.Confidence != rules.ConfidenceHigh {
		t.Errorf("a rule match should be high confidence, got %q", res.Diagnosis.Confidence)
	}
	if strings.Contains(res.Diagnosis.Cause, "model's guess") {
		t.Error("the model's answer leaked into a rule match")
	}
}

// TestEveryRuleFixtureBypassesTheLLM widens the guarantee: no scenario the
// rules cover may ever reach the model.
func TestEveryRuleFixtureBypassesTheLLM(t *testing.T) {
	pods := map[string]*corev1.Pod{
		"oomkilled": oomPod(),
		"imagepull": {
			ObjectMeta: metav1.ObjectMeta{Name: "puller-1", Namespace: "default", UID: "uid-pull"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "acme/app:nope"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "app",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "Back-off pulling image \"acme/app:nope\"",
					}},
				}},
			},
		},
		"configerror": {
			ObjectMeta: metav1.ObjectMeta{Name: "cfg-1", Namespace: "default", UID: "uid-cfg"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "app",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CreateContainerConfigError",
						Message: "secret \"app-secrets\" not found",
					}},
				}},
			},
		},
		"evicted": {
			ObjectMeta: metav1.ObjectMeta{Name: "eve-1", Namespace: "default", UID: "uid-eve"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1"}}},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory.",
			},
		},
	}

	for name, pod := range pods {
		t.Run(name, func(t *testing.T) {
			dc := collect(t, pod)
			spy := &spyLLM{}

			res, err := diagnose(context.Background(), dc, spy)
			if err != nil {
				t.Fatalf("diagnose: %v", err)
			}
			if res == nil {
				t.Fatalf("expected rule %s to match", name)
			}
			if res.RuleName != name {
				t.Errorf("matched rule %q, want %q", res.RuleName, name)
			}
			if spy.calls != 0 {
				t.Errorf("the LLM was called %d time(s) despite rule %q matching", spy.calls, res.RuleName)
			}
		})
	}
}

// TestLLMIsCalledOnlyWhenNoRuleMatched is the other half of the contract.
func TestLLMIsCalledOnlyWhenNoRuleMatched(t *testing.T) {
	dc := collect(t, mysteryPod())
	spy := &spyLLM{}

	res, err := diagnose(context.Background(), dc, spy)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call for an unmatched pod, got %d", spy.calls)
	}
	if res == nil {
		t.Fatal("expected the model's answer to be used")
	}
	if res.Source != output.SourceLLM {
		t.Errorf("source = %v, want SourceLLM", res.Source)
	}
	if res.RuleName != "" {
		t.Errorf("an LLM answer must not be attributed to a rule, got %q", res.RuleName)
	}
	if res.Diagnosis.Confidence != rules.ConfidenceMedium {
		t.Errorf("an LLM answer should be medium confidence, got %q", res.Diagnosis.Confidence)
	}
}

// TestNoLLMConfiguredIsNotAnError: without a key the tool still works, it just
// has nothing more to say.
func TestNoLLMConfiguredIsNotAnError(t *testing.T) {
	dc := collect(t, mysteryPod())

	res, err := diagnose(context.Background(), dc, nil)
	if err != nil {
		t.Fatalf("a missing LLM must not be an error: %v", err)
	}
	if res != nil {
		t.Errorf("expected no diagnosis, got %+v", res)
	}
}

func TestLLMErrorIsReportedNotSwallowed(t *testing.T) {
	dc := collect(t, mysteryPod())
	spy := &spyLLM{err: errors.New("all models failed")}

	res, err := diagnose(context.Background(), dc, spy)
	if err == nil {
		t.Fatal("expected the LLM error to surface")
	}
	if res != nil {
		t.Error("expected no result alongside an error")
	}
}

func TestLooksHealthy(t *testing.T) {
	if !looksHealthy(collect(t, healthyPod())) {
		t.Error("a running, ready, never-restarted pod should look healthy")
	}
	if looksHealthy(collect(t, oomPod())) {
		t.Error("a crash-looping pod must not look healthy")
	}
	if looksHealthy(nil) {
		t.Error("a nil context must not look healthy")
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantPod string
		wantNS  string
		wantErr bool
		check   func(*testing.T, *options)
	}{
		{
			name:    "canonical form",
			args:    []string{"pod", "my-app-x2kpl", "-n", "production"},
			wantPod: "my-app-x2kpl",
			wantNS:  "production",
		},
		{
			name:    "flags before positionals",
			args:    []string{"-n", "production", "pod", "my-app-x2kpl"},
			wantPod: "my-app-x2kpl",
			wantNS:  "production",
		},
		{
			name:    "long namespace flag with equals",
			args:    []string{"pod", "my-app-x2kpl", "--namespace=staging"},
			wantPod: "my-app-x2kpl",
			wantNS:  "staging",
		},
		{
			name:    "pod name without the pod keyword",
			args:    []string{"my-app-x2kpl"},
			wantPod: "my-app-x2kpl",
		},
		{
			name:    "kubectl style resource path",
			args:    []string{"pod/my-app-x2kpl", "-n", "prod"},
			wantPod: "my-app-x2kpl",
			wantNS:  "prod",
		},
		{
			name:    "french output",
			args:    []string{"pod", "x", "--lang", "fr"},
			wantPod: "x",
			check: func(t *testing.T, o *options) {
				if o.lang != "fr" {
					t.Errorf("lang = %q, want fr", o.lang)
				}
			},
		},
		{
			name:    "no-ai",
			args:    []string{"pod", "x", "--no-ai"},
			wantPod: "x",
			check: func(t *testing.T, o *options) {
				if !o.noAI {
					t.Error("--no-ai was not parsed")
				}
			},
		},
		{
			name:    "default timeout and language",
			args:    []string{"pod", "x"},
			wantPod: "x",
			check: func(t *testing.T, o *options) {
				if o.lang != "en" {
					t.Errorf("default lang = %q, want en", o.lang)
				}
				if o.timeout != 60*time.Second {
					t.Errorf("default timeout = %v, want 60s", o.timeout)
				}
			},
		},
		{name: "no arguments", args: nil, wantErr: true},
		{name: "pod keyword with no name", args: []string{"pod"}, wantErr: true},
		{name: "unknown subcommand", args: []string{"deployment", "web"}, wantErr: true},
		{name: "unsupported language", args: []string{"pod", "x", "--lang", "de"}, wantErr: true},
		{name: "non-positive timeout", args: []string{"pod", "x", "--timeout", "0s"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if opts == nil {
				t.Fatal("expected options")
			}
			if opts.podName != tc.wantPod {
				t.Errorf("pod = %q, want %q", opts.podName, tc.wantPod)
			}
			if tc.wantNS != "" && opts.namespace != tc.wantNS {
				t.Errorf("namespace = %q, want %q", opts.namespace, tc.wantNS)
			}
			if tc.check != nil {
				tc.check(t, opts)
			}
		})
	}
}

// TestDiagnoseIsLanguageAware checks the --lang flag reaches the rule text.
func TestDiagnoseIsLanguageAware(t *testing.T) {
	dc := collect(t, oomPod())

	en, err := diagnose(context.Background(), dc, nil)
	if err != nil || en == nil {
		t.Fatalf("diagnose(en): %v", err)
	}

	dc.Lang = "fr"
	fr, err := diagnose(context.Background(), dc, nil)
	if err != nil || fr == nil {
		t.Fatalf("diagnose(fr): %v", err)
	}

	if en.Diagnosis.Cause == fr.Diagnosis.Cause {
		t.Error("--lang fr produced the English text")
	}
	if !strings.Contains(fr.Diagnosis.Cause, "conteneur") {
		t.Errorf("French output does not look French: %s", fr.Diagnosis.Cause)
	}
}
