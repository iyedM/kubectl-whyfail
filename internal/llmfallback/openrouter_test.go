package llmfallback

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iyedM/kubectl-why-fail/internal/collector"
	"github.com/iyedM/kubectl-why-fail/internal/rules"
)

func testContext() *collector.DiagnosticContext {
	return &collector.DiagnosticContext{
		Pod: collector.PodInfo{Name: "weird-pod", Namespace: "default", Phase: "Running"},
		Containers: []collector.ContainerInfo{
			{Name: "app", Image: "app:1", Logs: "something unusual happened"},
		},
	}
}

// answerWith builds a server that replies with a normal chat completion.
func answerWith(t *testing.T, content string) (*httptest.Server, *[]chatRequest) {
	t.Helper()
	var seen []chatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		seen = append(seen, req)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: content}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func clientFor(srv *httptest.Server, models ...string) *Client {
	if len(models) == 0 {
		models = ModelCandidates
	}
	return &Client{
		APIKey:     "sk-test-key",
		Endpoint:   srv.URL,
		Models:     models,
		HTTPClient: srv.Client(),
	}
}

func TestExplainParsesJSONAnswer(t *testing.T) {
	srv, seen := answerWith(t, `{"cause":"The sidecar exits before the main container is ready.","suggestion":"Add a startupProbe to the sidecar."}`)

	d, err := clientFor(srv).Explain(context.Background(), testContext())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(d.Cause, "sidecar") {
		t.Errorf("cause not parsed: %q", d.Cause)
	}
	if !strings.Contains(d.Suggestion, "startupProbe") {
		t.Errorf("suggestion not parsed: %q", d.Suggestion)
	}
	// An LLM guess must never claim the confidence of a deterministic rule.
	if d.Confidence != rules.ConfidenceMedium {
		t.Errorf("confidence = %q, want %q", d.Confidence, rules.ConfidenceMedium)
	}

	if len(*seen) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*seen))
	}
	if got := (*seen)[0].Model; got != "openrouter/auto" {
		t.Errorf("first attempt used model %q, want openrouter/auto", got)
	}
	// The snapshot must actually reach the model.
	if !strings.Contains((*seen)[0].Messages[1].Content, "weird-pod") {
		t.Error("the pod snapshot was not included in the prompt")
	}
}

func TestExplainStripsMarkdownFence(t *testing.T) {
	srv, _ := answerWith(t, "```json\n{\"cause\":\"Disk full.\",\"suggestion\":\"Free space.\"}\n```")

	d, err := clientFor(srv).Explain(context.Background(), testContext())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if d.Cause != "Disk full." {
		t.Errorf("fenced JSON not unwrapped: %q", d.Cause)
	}
}

func TestExplainAcceptsProseAnswer(t *testing.T) {
	srv, _ := answerWith(t, "The container cannot resolve its database hostname.")

	d, err := clientFor(srv).Explain(context.Background(), testContext())
	if err != nil {
		t.Fatalf("a non-JSON answer should still be usable: %v", err)
	}
	if !strings.Contains(d.Cause, "resolve") {
		t.Errorf("prose answer not carried through: %q", d.Cause)
	}
	if d.Confidence != rules.ConfidenceMedium {
		t.Errorf("confidence = %q, want medium", d.Confidence)
	}
}

// TestExplainFallsBackThroughModels is the reason the model list exists:
// OpenRouter's catalogue shifts, and a retired model must not break the plugin.
func TestExplainFallsBackThroughModels(t *testing.T) {
	var attempts int32
	var used []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		used = append(used, req.Model)

		n := atomic.AddInt32(&attempts, 1)
		switch n {
		case 1: // model retired
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"No endpoints found for this model"}}`))
		case 2: // rate limited
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(chatResponse{
				Choices: []struct {
					Message chatMessage `json:"message"`
				}{
					{Message: chatMessage{Content: `{"cause":"Third model answered.","suggestion":"Ship it."}`}},
				},
			})
		}
	}))
	defer srv.Close()

	d, err := clientFor(srv).Explain(context.Background(), testContext())
	if err != nil {
		t.Fatalf("Explain should have recovered on the third model: %v", err)
	}
	if d.Cause != "Third model answered." {
		t.Errorf("unexpected answer: %q", d.Cause)
	}
	if len(used) != 3 {
		t.Fatalf("expected 3 attempts, got %d (%v)", len(used), used)
	}
	if used[0] != ModelCandidates[0] {
		t.Errorf("first attempt should be %q, got %q", ModelCandidates[0], used[0])
	}
}

func TestExplainFailsWhenEveryModelFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	defer srv.Close()

	_, err := clientFor(srv).Explain(context.Background(), testContext())
	if err == nil {
		t.Fatal("expected an error when no model answers")
	}
	if !strings.Contains(err.Error(), "all models failed") {
		t.Errorf("error should say every model was tried, got: %v", err)
	}
}

// TestNoKeyNeverCallsOut: without a key the fallback must be inert, not
// half-configured.
func TestNoKeyNeverCallsOut(t *testing.T) {
	t.Setenv(APIKeyEnv, "")

	if _, err := New(); err != ErrNoAPIKey {
		t.Errorf("New() without a key = %v, want ErrNoAPIKey", err)
	}

	var c *Client
	if _, err := c.Explain(context.Background(), testContext()); err != ErrNoAPIKey {
		t.Errorf("nil client Explain = %v, want ErrNoAPIKey", err)
	}
	if _, err := (&Client{}).Explain(context.Background(), testContext()); err != ErrNoAPIKey {
		t.Errorf("keyless client Explain = %v, want ErrNoAPIKey", err)
	}
}

func TestNewReadsKeyFromEnvironment(t *testing.T) {
	t.Setenv(APIKeyEnv, "  sk-or-v1-secret  ")

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.APIKey != "sk-or-v1-secret" {
		t.Errorf("key not trimmed: %q", c.APIKey)
	}
	if c.Endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want %q", c.Endpoint, DefaultEndpoint)
	}
	if len(c.Models) == 0 || c.Models[0] != "openrouter/auto" {
		t.Errorf("models = %v, want openrouter/auto first", c.Models)
	}
}

// TestAPIKeyIsSentAsBearerAndNeverLeaked checks both halves of the contract:
// the key authenticates the request, and it never escapes into an error.
func TestAPIKeyIsSentAsBearerAndNeverLeaked(t *testing.T) {
	const key = "sk-or-v1-super-secret"
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: key, Endpoint: srv.URL, Models: []string{"openrouter/auto"}, HTTPClient: srv.Client()}
	_, err := c.Explain(context.Background(), testContext())
	if err == nil {
		t.Fatal("expected an error")
	}
	if gotAuth != "Bearer "+key {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the API key leaked into an error message: %v", err)
	}
}

func TestExplainRejectsNilContext(t *testing.T) {
	c := &Client{APIKey: "k", Models: []string{"m"}}
	if _, err := c.Explain(context.Background(), nil); err == nil {
		t.Error("expected an error for a nil context")
	}
}

func TestPromptTruncatesHugeLogs(t *testing.T) {
	dc := testContext()
	dc.Containers[0].Logs = strings.Repeat("x", 50_000)

	prompt, err := buildPrompt(dc)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > 20_000 {
		t.Errorf("prompt is %d bytes; huge logs should be truncated", len(prompt))
	}
	if !strings.Contains(prompt, "[truncated]") {
		t.Error("truncation should be visible to the model")
	}
	// Truncating must not mutate the caller's context.
	if len(dc.Containers[0].Logs) != 50_000 {
		t.Error("buildPrompt mutated the caller's context")
	}
}

func TestSystemPromptSwitchesLanguage(t *testing.T) {
	if !strings.Contains(systemPrompt("fr"), "French") {
		t.Error("fr should ask the model for French output")
	}
	if !strings.Contains(systemPrompt(""), "English") {
		t.Error("the default should ask the model for English output")
	}
}
