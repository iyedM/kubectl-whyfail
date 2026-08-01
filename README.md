# kubectl-whyfai

[![CI](https://github.com/iyedM/kubectl-whyfail/actions/workflows/ci.yml/badge.svg)](https://github.com/iyedM/kubectl-whyfail/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/iyedM/kubectl-whyfail)](https://github.com/iyedM/kubectl-whyfail/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/iyedM/kubectl-whyfail)](https://goreportcard.com/report/github.com/iyedM/kubectl-whyfail)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Stop reading YAML at 3am. Ask your cluster why the pod is broken.**

`kubectl-whyfail` is a `kubectl` plugin that diagnoses a failing pod and explains the cause
in plain language — not raw JSON.

```bash
kubectl whyfail pod my-app-7d9f8b6c-x2kpl -n production
```

![demo](docs/demo.gif)

---

## The problem

Every Kubernetes engineer knows the ritual. A pod is in `CrashLoopBackOff`, and you spend the
next 30 minutes correlating three commands by hand:

```console
$ kubectl get pod my-app-7d9f8b6c-x2kpl -n production
NAME                     READY   STATUS             RESTARTS   AGE
my-app-7d9f8b6c-x2kpl    0/1     CrashLoopBackOff   7          13m
```

That tells you *that* it is broken. It does not tell you *why*. So you run `kubectl describe`,
scroll past 80 lines of YAML to the events at the bottom, run `kubectl logs --previous`
because the current container has no logs yet, and finally piece the story together yourself.

**whyfail does the correlation for you:**

```console
$ kubectl whyfail pod my-app-7d9f8b6c-x2kpl -n production

  checkout-api-7d9f8b6c-x2kpl  (namespace: production)
  status: CrashLoopBackOff   restarts: 7   node: ip-10-0-3-17.eu-west-1.compute.internal
   ✔ MATCHED RULE   crashloop_probe
  confidence: high

  WHY IT FAILS
  Container "api" is in CrashLoopBackOff because its liveness probe
  (http://:8080/healthz) keeps failing: the kubelet kills the container, restarts
  it, and the probe fails again (7 restarts so far). The probe is configured with
  initialDelaySeconds=3, periodSeconds=5, timeoutSeconds=1, failureThreshold=2.
  The application is most likely still starting up when the first probe fires,
  rather than being genuinely dead.
  Last probe error: Liveness probe failed: Get "http://10.0.3.42:8080/healthz":
  context deadline exceeded (Client.Timeout exceeded while awaiting headers)

  HOW TO FIX IT
  Give the app time to boot before the liveness probe counts:
    • Add a startupProbe on http://:8080/healthz and let it own the slow start (recommended).
    • Or raise initialDelaySeconds (currently 3) above the app's real boot time.
    • Or raise failureThreshold / timeoutSeconds so a slow response is not fatal.
  Check the boot time first:  kubectl logs checkout-api-7d9f8b6c-x2kpl -n production -c api --previous
```

---

## Installation

```bash
go install github.com/iyedM/kubectl-whyfail/cmd/whyfail@latest
```

`kubectl` discovers plugins by looking for executables named `kubectl-<name>` on your `PATH`,
so rename the installed binary once:

```bash
mv "$(go env GOPATH)/bin/whyfail" "$(go env GOPATH)/bin/kubectl-whyfail"
```

Check it worked:

```bash
kubectl plugin list | grep whyfail
kubectl whyfail --version
```

**Or download a prebuilt binary**

Grab the archive for your platform from the [latest release](https://github.com/iyedM/kubectl-whyfail/releases/latest), then:

```bash
tar -xzf kubectl-whyfail_linux_amd64.tar.gz
sudo mv kubectl-whyfail_linux_amd64 /usr/local/bin/kubectl-whyfail
```

(Windows: unzip the `.zip` and place the `.exe` anywhere on your `PATH`.)

<details>
<summary>Build from source</summary>

```bash
git clone https://github.com/iyedM/kubectl-whyfail
cd kubectl-whyfail
go build -o kubectl-whyfail ./cmd/whyfail
sudo mv kubectl-whyfail /usr/local/bin/
```

</details>

You can also run it as a plain binary without kubectl: `./whyfail pod my-app -n production`.

---

## Usage

```
kubectl whyfail pod <name> [-n namespace] [flags]

  -n, --namespace string   namespace of the pod (default: current context's namespace)
      --lang string        output language: en or fr (default "en")
      --no-ai              never call the LLM fallback, even if no rule matches
      --no-color           disable coloured output
      --kubeconfig string  path to the kubeconfig file
      --context string     kubeconfig context to use
      --timeout duration   overall time budget (default 60s)
  -v, --version            print the version and exit
```

```bash
# The usual case
kubectl whyfail pod my-app-7d9f8b6c-x2kpl -n production

# En français
kubectl whyfail pod my-app-7d9f8b6c-x2kpl --lang fr

# Rules only, guaranteed no network calls beyond the cluster
kubectl whyfail pod my-app-7d9f8b6c-x2kpl --no-ai
```

---

## What it detects

Ten failure modes, each a deterministic rule with no network access and no AI involved.
When one matches, you get an answer instantly and with **high confidence**.

| # | Rule | Catches |
|---|------|---------|
| 1 | `crashloop_probe` | `CrashLoopBackOff` caused by a liveness probe that fires before the app has booted |
| 2 | `oomkilled` | `OOMKilled` — the memory limit is too low (or the app leaks) |
| 3 | `imagepull` | `ImagePullBackOff` / `ErrImagePull` — typo, missing tag, bad credentials, unreachable registry |
| 4 | `pending_resources` | `Pending` — no node has enough CPU/memory for the pod's requests |
| 5 | `pending_pvc` | `Pending` — PVC never bound: missing StorageClass, no default class, zone conflict |
| 6 | `configerror` | `CreateContainerConfigError` — a referenced ConfigMap/Secret or key does not exist |
| 7 | `evicted` | `Evicted` — the node ran out of memory or disk and picked this pod |
| 8 | `crashloop_command` | `CrashLoopBackOff` from a bad `CMD`/`ENTRYPOINT` (exit 127/126) |
| 9 | `readiness_never_ready` | Pod runs, never becomes ready — readiness probe on the wrong port/path |
| 10 | `imagepull_arch` | Architecture mismatch — the arm64-image-on-amd64-cluster trap |

Rules are evaluated in this order and the first match wins, so the narrow explanation always
beats the vague one.

### The optional AI fallback

If **none** of the ten rules matches, and only then, whyfail can ask an LLM to take a guess.

- It is **off by default**. Set `OPENROUTER_API_KEY` to enable it.
- It uses [OpenRouter](https://openrouter.ai) with `openrouter/auto` plus a list of fallback
  models, so a retired model never breaks the plugin.
- No local model, no Ollama, no daemon. Just a key you provide.
- Its answers are labelled **`✦ AI GUESS`** with **medium** confidence, and are visually
  impossible to confuse with a rule match.

```bash
export OPENROUTER_API_KEY=sk-or-v1-...
kubectl whyfail pod weird-pod-9f8b6c -n production
```

Your API key is read from the environment only. It is never logged, never written to disk,
and is stripped from error messages.

---

## How to add a rule

Contributions are very welcome, and a rule is deliberately small to write. Each one is a
single file, a single test, and two fixtures.

**1. Write the rule** in `internal/rules/my_rule.go`:

```go
package rules

import "fmt"

var myRule = Rule{
	Name: "my_rule",

	Match: func(ctx *DiagnosticContext) bool {
		c, ok := waitingWithReason(ctx, "SomeWaitReason")
		return ok && c.RestartCount > 0
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := waitingWithReason(ctx, "SomeWaitReason")
		return Diagnosis{
			Cause: ctx.L(
				fmt.Sprintf("Container %q failed because ...", c.Name),
				fmt.Sprintf("Le conteneur %q a échoué parce que ...", c.Name),
			),
			Suggestion: ctx.L("Run: kubectl ...", "Lancez : kubectl ..."),
			Confidence: ConfidenceHigh,
		}
	},
}
```

Use `ctx.L(english, french)` for anything the user reads — the `--lang` flag depends on it.
Helpers like `waitingWithReason`, `lastEventWithReason`, `isCrashLooping`, `wasOOMKilled` and
`containsAnyFold` live in [`internal/rules/helpers.go`](internal/rules/helpers.go).

**2. Register it** in the `registry` slice in [`internal/rules/rule.go`](internal/rules/rule.go).
Position matters: it is the priority order. If your rule is narrower than an existing one that
would shadow it, add an explicit exclusion to the broader rule rather than reordering.

**3. Add two fixtures** in `internal/rules/testdata/` — `my_rule_match.json` and
`my_rule_nomatch.json`. The *nomatch* one is the important half: make it a realistic **near
miss**, a pod that looks similar but has a different root cause. That is what stops false
positives.

**4. Test it** in `internal/rules/rules_test.go`:

```go
func TestMyRule(t *testing.T) {
	d := assertMatches(t, myRule, "my_rule_match")
	if !strings.Contains(d.Cause, "something specific") {
		t.Errorf("cause should name the culprit, got: %s", d.Cause)
	}

	assertDoesNotMatch(t, myRule, "my_rule_nomatch")
	assertDoesNotMatch(t, myRule, "healthy")
}
```

Then add your fixture to the table in `TestRunAllPriority` so the registry order stays pinned.

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

Two rules that the suite enforces automatically, and which are the whole point of the project:

- **No rule may match `testdata/healthy.json`.** A confident wrong answer is worse than no
  answer.
- **The LLM is never called when a rule matched.** Guarded by an integration test in
  [`cmd/whyfail/main_test.go`](cmd/whyfail/main_test.go).

> The v1 scope is intentionally closed at ten rules to keep them 100% reliable. If you want to
> add an eleventh, please open an issue first so we can discuss it — that is a scope decision,
> not a code review.

---

## Architecture

A strict three-stage pipeline, one direction only:

```
  ┌─────────────┐     ┌──────────────┐     ┌──────────────────┐
  │  collector  │ ──▶ │ rules engine │ ──▶ │  LLM fallback    │
  │             │     │  (10 rules)  │     │  (only if no     │
  │  reads only │     │  no network  │     │   rule matched)  │
  └─────────────┘     └──────────────┘     └──────────────────┘
        │                    │                      │
   describe, events     deterministic           OpenRouter,
   logs (current +      high confidence         medium confidence
   previous), spec
```

| Package | Responsibility |
|---------|----------------|
| [`internal/collector`](internal/collector) | Reads the pod, its events, its logs, its node and its PVCs into a flat `DiagnosticContext`. Contains **zero** diagnosis logic. |
| [`internal/rules`](internal/rules) | The ten deterministic rules. No network, no clock, fully testable from JSON fixtures. |
| [`internal/llmfallback`](internal/llmfallback) | Minimal OpenRouter client. Only reachable when no rule matched. |
| [`internal/output`](internal/output) | Terminal rendering, with distinct badges for rule vs AI answers. |
| [`cmd/whyfail`](cmd/whyfail) | Argument parsing, kubeconfig, and pipeline orchestration. |

Because `DiagnosticContext` is plain JSON with no Kubernetes types in it, every rule can be
tested against a realistic fixture without a cluster.

---

## Development

```bash
go build -o whyfail ./cmd/whyfail
go test ./...
gofmt -l .
```

---

## License

MIT — see [LICENSE](LICENSE).
