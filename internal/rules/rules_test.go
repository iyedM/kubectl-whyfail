package rules

import (
	"encoding/json"
	"fmt"
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

// TestOOMKilledRestartPluralisation pins the restart sentence, which is built
// from a counter and therefore reads wrong at exactly the boundary values a
// fixture with 5 restarts never exercises.
func TestOOMKilledRestartPluralisation(t *testing.T) {
	// oomCtx builds a crash-looping, OOM-killed container with n restarts.
	oomCtx := func(n int32, lang string) *DiagnosticContext {
		ctx := loadFixture(t, "oomkilled_match")
		ctx.Lang = lang
		ctx.Containers[0].RestartCount = n
		return ctx
	}

	t.Run("one restart is singular", func(t *testing.T) {
		d := oomKilledRule.Explain(oomCtx(1, "en"))
		if !strings.Contains(d.Cause, "restarted 1 time,") {
			t.Errorf("expected \"restarted 1 time\", got:\n%s", d.Cause)
		}
		if strings.Contains(d.Cause, "1 times") {
			t.Errorf("\"1 times\" is not English:\n%s", d.Cause)
		}
	})

	t.Run("two restarts are plural", func(t *testing.T) {
		d := oomKilledRule.Explain(oomCtx(2, "en"))
		if !strings.Contains(d.Cause, "restarted 2 times") {
			t.Errorf("expected \"restarted 2 times\", got:\n%s", d.Cause)
		}
	})

	t.Run("zero restarts omits the sentence entirely", func(t *testing.T) {
		d := oomKilledRule.Explain(oomCtx(0, "en"))
		if strings.Contains(d.Cause, "restarted") {
			t.Errorf("the recurrence sentence must not appear with 0 restarts:\n%s", d.Cause)
		}
		// "0 times ... repeatedly, not once" would contradict itself.
		if strings.Contains(d.Cause, "repeatedly") {
			t.Errorf("must not claim a repeated kill with 0 restarts:\n%s", d.Cause)
		}
		// The rest of the diagnosis must still be intact.
		if !strings.Contains(d.Cause, "OOMKilled") || !strings.Contains(d.Cause, "128Mi") {
			t.Errorf("dropping the sentence damaged the diagnosis:\n%s", d.Cause)
		}
	})

	t.Run("french fois is invariable", func(t *testing.T) {
		for _, n := range []int32{1, 2} {
			d := oomKilledRule.Explain(oomCtx(n, "fr"))
			want := fmt.Sprintf("redémarré %d fois", n)
			if !strings.Contains(d.Cause, want) {
				t.Errorf("expected %q, got:\n%s", want, d.Cause)
			}
		}
		if d := oomKilledRule.Explain(oomCtx(0, "fr")); strings.Contains(d.Cause, "redémarré") {
			t.Errorf("the French sentence must be dropped too with 0 restarts:\n%s", d.Cause)
		}
	})
}

// assertRestartAgreement renders a rule at a given restart count and checks
// that both languages agree correctly. The expected fragments include their
// closing parenthesis on purpose: without it, "1 restart" would also match the
// broken "1 restarts" and the test would pass on the bug it exists to catch.
func assertRestartAgreement(t *testing.T, r Rule, fixture string, n int32, wantEN, wantFR string) {
	t.Helper()

	for _, tc := range []struct{ lang, want string }{{"en", wantEN}, {"fr", wantFR}} {
		ctx := loadFixture(t, fixture)
		ctx.Lang = tc.lang
		ctx.Containers[0].RestartCount = n

		d := r.Explain(ctx)
		if !strings.Contains(d.Cause, tc.want) {
			t.Errorf("%s at %d restarts (lang=%s): expected %q in:\n%s", r.Name, n, tc.lang, tc.want, d.Cause)
		}
	}
}

// TestCrashLoopProbeRestartPluralisation covers rule 1.
//
// Zero is not tested against the text because the rule cannot fire there:
// probeKilledContainer requires at least one restart, since a probe that has
// killed nothing yet is not the cause of anything. That guarantee is asserted
// instead.
func TestCrashLoopProbeRestartPluralisation(t *testing.T) {
	assertRestartAgreement(t, crashLoopProbeRule, "crashloop_probe_match", 1,
		"(1 restart so far)", "(1 redémarrage)")
	assertRestartAgreement(t, crashLoopProbeRule, "crashloop_probe_match", 2,
		"(2 restarts so far)", "(2 redémarrages)")

	t.Run("zero restarts is unreachable by design", func(t *testing.T) {
		ctx := loadFixture(t, "crashloop_probe_match")
		ctx.Containers[0].RestartCount = 0
		if crashLoopProbeRule.Match(ctx) {
			t.Error("a liveness probe that has restarted nothing must not be blamed")
		}
	})
}

// TestCrashLoopCommandRestartPluralisation covers rule 8. Zero is reachable
// here: an entrypoint that cannot be exec'd fails on the very first attempt,
// before any restart is recorded.
func TestCrashLoopCommandRestartPluralisation(t *testing.T) {
	for _, tc := range []struct {
		n              int32
		wantEN, wantFR string
	}{
		{0, "(0 restarts)", "(0 redémarrage)"},
		{1, "(1 restart)", "(1 redémarrage)"},
		{2, "(2 restarts)", "(2 redémarrages)"},
	} {
		assertRestartAgreement(t, crashLoopCommandRule, "crashloop_command_match", tc.n, tc.wantEN, tc.wantFR)
	}

	t.Run("the rule still fires at zero restarts", func(t *testing.T) {
		ctx := loadFixture(t, "crashloop_command_match")
		ctx.Containers[0].RestartCount = 0
		if !crashLoopCommandRule.Match(ctx) {
			t.Error("exit code 127 is conclusive on its own, restarts or not")
		}
	})
}

// TestReadinessRestartPluralisation covers rule 9, where the agreement matters
// most: the rule only fires below three restarts, so 0 and 1 — the two values
// the old code got wrong — are the everyday case, not an edge case.
func TestReadinessRestartPluralisation(t *testing.T) {
	for _, tc := range []struct {
		n              int32
		wantEN, wantFR string
	}{
		{0, "(0 restarts)", "(0 redémarrage)"},
		{1, "(1 restart)", "(1 redémarrage)"},
		{2, "(2 restarts)", "(2 redémarrages)"},
	} {
		assertRestartAgreement(t, readinessNeverReadyRule, "readiness_never_ready_match", tc.n, tc.wantEN, tc.wantFR)

		// Every one of these counts is reachable: the rule bails out only
		// above two restarts.
		ctx := loadFixture(t, "readiness_never_ready_match")
		ctx.Containers[0].RestartCount = tc.n
		if !readinessNeverReadyRule.Match(ctx) {
			t.Errorf("rule should still match at %d restarts", tc.n)
		}
	}
}

// TestNoBrokenPluralAnywhere is the backstop: it renders every rule against
// every match fixture at the counts that trip agreement, and fails on any
// "<n> restarts"/"<n> redémarrages" that should have been singular.
func TestNoBrokenPluralAnywhere(t *testing.T) {
	fixtures := []string{
		"crashloop_probe_match", "oomkilled_match", "imagepull_match",
		"pending_resources_match", "pending_pvc_match", "configerror_match",
		"evicted_match", "crashloop_command_match", "readiness_never_ready_match",
		"imagepull_arch_match", "imagepull_dns_match", "imagepull_ratelimit_match",
		"imagepull_ecr_auth_match",
	}
	// Wrong at one, and — in French only — wrong at zero as well.
	banned := []string{"1 restarts", "1 redémarrages", "1 times", "0 redémarrages"}

	for _, fixture := range fixtures {
		for _, n := range []int32{0, 1, 2} {
			for _, lang := range []string{"en", "fr"} {
				ctx := loadFixture(t, fixture)
				ctx.Lang = lang
				for i := range ctx.Containers {
					ctx.Containers[i].RestartCount = n
				}

				for _, r := range All() {
					if !r.Match(ctx) {
						continue
					}
					d := r.Explain(ctx)
					for _, bad := range banned {
						if strings.Contains(d.Cause, bad) || strings.Contains(d.Suggestion, bad) {
							t.Errorf("rule %s on %s (n=%d, lang=%s) produced %q", r.Name, fixture, n, lang, bad)
						}
					}
				}
			}
		}
	}
}

// TestCountHelpers pins the two agreement rules, which differ at zero.
func TestCountHelpers(t *testing.T) {
	cases := []struct {
		n      int32
		wantEN string
		wantFR string
	}{
		{0, "0 restarts", "0 redémarrage"},
		{1, "1 restart", "1 redémarrage"},
		{2, "2 restarts", "2 redémarrages"},
		{17, "17 restarts", "17 redémarrages"},
	}
	for _, tc := range cases {
		if got := countEN(tc.n, "restart", "restarts"); got != tc.wantEN {
			t.Errorf("countEN(%d) = %q, want %q", tc.n, got, tc.wantEN)
		}
		if got := countFR(tc.n, "redémarrage", "redémarrages"); got != tc.wantFR {
			t.Errorf("countFR(%d) = %q, want %q", tc.n, got, tc.wantFR)
		}
	}
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

// TestImagePullPrefersInformativeEvent reproduces a real k3d failure: the
// kubelet emits the registry's actual error once, then repeats
// "Error: ErrImagePull" and "Error: ImagePullBackOff" on every retry. Reading
// the most recent Failed event therefore loses the cause and drops the
// diagnosis into the generic branch.
func TestImagePullPrefersInformativeEvent(t *testing.T) {
	d := assertMatches(t, imagePullRule, "imagepull_dns_match")

	// The real error must be the one quoted back to the user.
	if !strings.Contains(d.Cause, "dial tcp") || !strings.Contains(d.Cause, "registry-1.docker.io") {
		t.Errorf("cause should quote the DNS error from the first Failed event, got:\n%s", d.Cause)
	}

	// The generic repeats must never be what "Registry said".
	for _, generic := range []string{"Registry said: Error: ImagePullBackOff", "Registry said: Error: ErrImagePull"} {
		if strings.Contains(d.Cause, generic) {
			t.Errorf("the kubelet's generic retry message won over the real error: %q", generic)
		}
	}

	// It must land in the network branch, not the default one.
	if !strings.Contains(d.Cause, "could not reach the registry") {
		t.Errorf("expected the network/DNS branch, got:\n%s", d.Cause)
	}
	if strings.Contains(d.Suggestion, "Verify the image exists") {
		t.Errorf("fell through to the default branch:\n%s", d.Suggestion)
	}
}

// TestImagePullClassificationAcrossEnvironments is the portability guard.
//
// The wording of a pull error comes from the container runtime and the
// registry, never from Kubernetes, so a rule tuned on one cluster silently
// misclassifies on the next. Each case below is a real message from a
// different runtime/registry combination.
func TestImagePullClassificationAcrossEnvironments(t *testing.T) {
	cases := []struct {
		env    string
		reason string
		msg    string
		want   imagePullKind
	}{
		// Rate limiting — the most common production pull failure.
		{
			env:  "containerd + Docker Hub, anonymous quota",
			msg:  `failed to copy: httpReadSeeker: failed open: unexpected status code 429 Too Many Requests - Server message: toomanyrequests: You have reached your pull rate limit.`,
			want: pullRateLimited,
		},
		{
			env:  "Docker daemon + Docker Hub quota",
			msg:  `Error response from daemon: toomanyrequests: You have reached your pull rate limit.`,
			want: pullRateLimited,
		},

		// Credentials, across four registries and three runtimes.
		{
			env:  "CRI-O + ECR, expired token",
			msg:  `initializing source docker://123.dkr.ecr.eu-west-1.amazonaws.com/app:v1: reading manifest v1: denied: Your authorization token has expired. Reauthenticate and try again.`,
			want: pullAuth,
		},
		{
			env:  "Docker daemon + private repo",
			msg:  `Error response from daemon: pull access denied for acme/private, repository does not exist or may require 'docker login': denied: requested access to the resource is denied`,
			want: pullAuth,
		},
		{
			env:  "containerd + Artifact Registry, IAM denied",
			msg:  `failed to resolve reference "europe-docker.pkg.dev/proj/repo/app:v3": unexpected status from HEAD request: 403 Forbidden`,
			want: pullAuth,
		},
		{
			env:  "containerd + ACR",
			msg:  `failed to resolve reference "acme.azurecr.io/app:v1": unauthorized: authentication required, visit https://aka.ms/acr/authorization`,
			want: pullAuth,
		},
		{
			env:  "containerd + Harbor, robot account scope",
			msg:  `failed to authorize: failed to fetch anonymous token: unexpected status: 401 Unauthorized: insufficient_scope`,
			want: pullAuth,
		},

		// Network, DNS, proxy and TLS.
		{
			env:  "k3d + Docker Hub, DNS EAI_AGAIN (the original report)",
			msg:  `failed to do request: Head "https://registry-1.docker.io/v2/polinux/stress/manifests/latest": dial tcp: lookup registry-1.docker.io on 10.43.0.10:53: Try again`,
			want: pullNetwork,
		},
		{
			env:  "CRI-O + Docker Hub, DNS server misbehaving",
			msg:  `pinging container registry registry-1.docker.io: Get "https://registry-1.docker.io/v2/": dial tcp: lookup registry-1.docker.io on 10.96.0.10:53: server misbehaving`,
			want: pullNetwork,
		},
		{
			env:  "corporate proxy refusing the connection",
			msg:  `failed to do request: Head "https://registry-1.docker.io/v2/library/nginx/manifests/latest": proxyconnect tcp: dial tcp 10.0.0.9:3128: connect: connection refused`,
			want: pullNetwork,
		},
		{
			env:  "on-prem Harbor with a self-signed CA",
			msg:  `failed to do request: Head "https://harbor.corp.local/v2/team/api/manifests/1.2": x509: certificate signed by unknown authority`,
			want: pullNetwork,
		},
		{
			env:  "Quay, TLS handshake timeout",
			msg:  `failed to do request: Head "https://quay.io/v2/": net/http: TLS handshake timeout`,
			want: pullNetwork,
		},
		{
			env:  "registry served over plain HTTP",
			msg:  `failed to do request: Head "https://registry.internal:5000/v2/": http: server gave HTTP response to HTTPS client`,
			want: pullNetwork,
		},
		{
			// Regression: a bare "404" in the list matched the hex digest and
			// reported a missing tag for what is plainly a network timeout.
			env:  "digest whose hex contains 404, on a timing-out registry",
			msg:  `Failed to pull image "acme/app@sha256:c404f1e9d40412ab": failed to do request: Head "https://reg/v2/": dial tcp 10.1.2.3:443: i/o timeout`,
			want: pullNetwork,
		},

		// Genuinely missing repository or tag.
		{
			env:  "containerd + GHCR, tag never pushed",
			msg:  `failed to resolve reference "ghcr.io/acme/app:v9": ghcr.io/acme/app:v9: not found`,
			want: pullMissing,
		},
		{
			env:  "Docker daemon, unknown manifest",
			msg:  `Error response from daemon: manifest for acme/app:nope not found: manifest unknown`,
			want: pullMissing,
		},
		{
			env:  "ECR, repository does not exist",
			msg:  `name unknown: The repository with name 'orders' does not exist in the registry`,
			want: pullMissing,
		},

		// Malformed reference: decided by the kubelet's status, not the text.
		{
			env:    "any runtime, malformed reference",
			reason: "InvalidImageName",
			msg:    `couldn't parse image name "My_Registry/app:v1": invalid reference format`,
			want:   pullInvalidName,
		},

		// Boilerplate carries no cause at all and must stay unclassified, so
		// imagePullMessage keeps looking for something better.
		{env: "kubelet retry boilerplate", msg: `Error: ImagePullBackOff`, want: pullUnknown},
		{env: "kubelet retry boilerplate", msg: `Error: ErrImagePull`, want: pullUnknown},
	}

	names := map[imagePullKind]string{
		pullUnknown:     "unknown",
		pullInvalidName: "invalid-name",
		pullRateLimited: "rate-limited",
		pullAuth:        "auth",
		pullNetwork:     "network",
		pullMissing:     "missing",
	}

	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			got := classifyImagePull(tc.reason, tc.msg)
			if got != tc.want {
				t.Errorf("classified as %s, want %s\nmessage: %s", names[got], names[tc.want], tc.msg)
			}
		})
	}
}

// TestImagePullRateLimitAndECR walks the two new environments end to end, so
// the wording the user actually sees is pinned, not just the classification.
func TestImagePullRateLimitAndECR(t *testing.T) {
	t.Run("docker hub rate limit", func(t *testing.T) {
		d := assertMatches(t, imagePullRule, "imagepull_ratelimit_match")
		if !strings.Contains(d.Cause, "rate-limiting") {
			t.Errorf("expected the rate-limit branch, got:\n%s", d.Cause)
		}
		// The old behaviour sent the user hunting for a non-existent image.
		if strings.Contains(d.Suggestion, "Verify the image exists") {
			t.Errorf("fell through to the default branch:\n%s", d.Suggestion)
		}
		if !strings.Contains(d.Suggestion, "imagePullPolicy: IfNotPresent") {
			t.Errorf("suggestion should cover re-pull avoidance, got:\n%s", d.Suggestion)
		}
	})

	t.Run("ecr expired token on cri-o", func(t *testing.T) {
		d := assertMatches(t, imagePullRule, "imagepull_ecr_auth_match")
		if !strings.Contains(d.Cause, "unauthenticated or the credentials") {
			t.Errorf("expected the credentials branch, got:\n%s", d.Cause)
		}
		// The pod already has a pull secret; naming it is what makes the
		// answer actionable rather than generic.
		if !strings.Contains(d.Cause, "ecr-credentials") {
			t.Errorf("cause should list the pod's existing imagePullSecrets, got:\n%s", d.Cause)
		}
	})
}

// TestImagePullMessageSelection pins the selection logic itself, independently
// of how Explain then words the answer.
func TestImagePullMessageSelection(t *testing.T) {
	t.Run("prefers a cause over a later generic message", func(t *testing.T) {
		ctx := loadFixture(t, "imagepull_dns_match")
		c, ok := imagePullFailure(ctx)
		if !ok {
			t.Fatal("expected an image pull failure")
		}
		got := imagePullMessage(ctx, c)
		if !strings.Contains(got, "dial tcp") {
			t.Errorf("selected message = %q, want the detailed network error", got)
		}
	})

	t.Run("falls back to the last event when all are boilerplate", func(t *testing.T) {
		ctx := loadFixture(t, "imagepull_dns_match")
		// Strip the informative event, leaving only the kubelet's repeats.
		var kept []event
		for _, e := range ctx.Events {
			if strings.Contains(e.Message, "dial tcp") {
				continue
			}
			kept = append(kept, e)
		}
		ctx.Events = kept

		c, _ := imagePullFailure(ctx)
		got := imagePullMessage(ctx, c)
		if got == "" {
			t.Error("expected some message rather than an empty one")
		}
		// With nothing better available, reporting the symptom is correct.
		if !strings.Contains(got, "ImagePullBackOff") && !strings.Contains(got, "ErrImagePull") {
			t.Errorf("selected message = %q, want one of the remaining events", got)
		}
	})

	t.Run("single detailed event is still chosen", func(t *testing.T) {
		ctx := loadFixture(t, "imagepull_match")
		c, _ := imagePullFailure(ctx)
		if got := imagePullMessage(ctx, c); !strings.Contains(got, "not found") {
			t.Errorf("selected message = %q, want the registry's not-found error", got)
		}
	})
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
		{"imagepull_dns_match", "imagepull"},
		{"imagepull_ratelimit_match", "imagepull"},
		{"imagepull_ecr_auth_match", "imagepull"},
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
