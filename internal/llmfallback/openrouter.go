// Package llmfallback asks an LLM to explain a pod failure that none of the
// deterministic rules could match.
//
// It is strictly optional. Without OPENROUTER_API_KEY the CLI still works and
// still gives a full answer for every scenario the rules cover; the LLM only
// ever sees contexts the rules gave up on.
package llmfallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/iyedM/kubectl-why-fail/internal/collector"
	"github.com/iyedM/kubectl-why-fail/internal/rules"
)

// DefaultEndpoint is the OpenAI-compatible chat completions endpoint.
const DefaultEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// APIKeyEnv is the only place the key is ever read from. It is never logged,
// never written to disk, and never included in an error message.
const APIKeyEnv = "OPENROUTER_API_KEY"

// ModelCandidates is the ordered list of models to try.
//
// "openrouter/auto" comes first and lets OpenRouter route to whatever is
// healthy right now. The rest are fallbacks, tried in order, because
// OpenRouter's free catalogue changes often and any single hard-coded model
// will eventually 404. Adding or reordering entries here needs no code change.
var ModelCandidates = []string{
	"openrouter/auto",
	"anthropic/claude-sonnet-4.5",
	"google/gemini-2.5-flash",
	"meta-llama/llama-3.3-70b-instruct",
}

// Referer and Title identify the plugin to OpenRouter's dashboard. Both are
// optional headers in the API; they carry no user data.
const (
	Referer = "https://github.com/iyedM/kubectl-why-fail"
	Title   = "kubectl-why-fail"
)

// ErrNoAPIKey is returned when no key is configured. The CLI treats it as
// "the user did not opt in", not as a failure.
var ErrNoAPIKey = errors.New("no " + APIKeyEnv + " set")

// Client talks to OpenRouter.
type Client struct {
	APIKey     string
	Endpoint   string
	Models     []string
	HTTPClient *http.Client
}

// New builds a client from the environment. It returns ErrNoAPIKey when the
// user has not opted into the LLM fallback.
func New() (*Client, error) {
	key := strings.TrimSpace(os.Getenv(APIKeyEnv))
	if key == "" {
		return nil, ErrNoAPIKey
	}
	return &Client{
		APIKey:     key,
		Endpoint:   DefaultEndpoint,
		Models:     ModelCandidates,
		HTTPClient: &http.Client{Timeout: 45 * time.Second},
	}, nil
}

// Explain asks the model why the pod is failing and returns a Diagnosis.
//
// The confidence is always "medium": unlike a rule, the model is guessing from
// a snapshot, and the output must never look as authoritative as a
// deterministic match.
func (c *Client) Explain(ctx context.Context, dc *collector.DiagnosticContext) (*rules.Diagnosis, error) {
	if c == nil || c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	if dc == nil {
		return nil, errors.New("llmfallback: nil diagnostic context")
	}

	prompt, err := buildPrompt(dc)
	if err != nil {
		return nil, err
	}

	models := c.Models
	if len(models) == 0 {
		models = ModelCandidates
	}

	var errs []string
	for _, model := range models {
		content, err := c.complete(ctx, model, prompt, dc.Lang)
		if err != nil {
			// A model that is retired, rate-limited or unavailable should not
			// end the attempt — that is the whole point of the fallback list.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			errs = append(errs, fmt.Sprintf("%s: %v", model, err))
			continue
		}
		d, err := parseDiagnosis(content)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", model, err))
			continue
		}
		return d, nil
	}

	return nil, fmt.Errorf("all models failed: %s", strings.Join(errs, "; "))
}

// chatRequest is the OpenAI-compatible request body.
type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	MaxTokens      int           `json:"max_tokens"`
	ResponseFormat *responseFmt  `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFmt struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (c *Client) complete(ctx context.Context, model, prompt, lang string) (string, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	body, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt(lang)},
			{Role: "user", Content: prompt},
		},
		Temperature:    0.2,
		MaxTokens:      900,
		ResponseFormat: &responseFmt{Type: "json_object"},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("HTTP-Referer", Referer)
	req.Header.Set("X-Title", Title)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 45 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", redactKey(err.Error(), c.APIKey)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		var parsed chatResponse
		if json.Unmarshal(raw, &parsed) == nil && parsed.Error != nil {
			return "", fmt.Errorf("http %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("malformed response: %w", err)
	}
	if parsed.Error != nil {
		return "", errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("no choices returned")
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("empty completion")
	}
	return content, nil
}

func systemPrompt(lang string) string {
	base := "You are a senior Kubernetes SRE. You are given a JSON snapshot of a single failing pod " +
		"(status, containers, probes, resources, events, logs). Explain, in plain language, the single most " +
		"likely reason it is failing, and what to do about it.\n\n" +
		"Rules:\n" +
		"- Reason only from the snapshot. Never invent an event, a log line or a value that is not there.\n" +
		"- If the snapshot is genuinely inconclusive, say so plainly instead of guessing confidently.\n" +
		"- Be specific: name the container, the image, the port, the resource.\n" +
		"- Suggestions must be actionable, ideally a command the user can run.\n\n" +
		`Answer with a JSON object and nothing else: {"cause": "...", "suggestion": "..."}`
	if lang == "fr" {
		return base + "\n\nWrite the values of \"cause\" and \"suggestion\" in French."
	}
	return base + "\n\nWrite the values of \"cause\" and \"suggestion\" in English."
}

// buildPrompt serialises the context for the model. Logs are truncated: a
// runaway log tail would dominate the prompt and cost without adding signal.
func buildPrompt(dc *collector.DiagnosticContext) (string, error) {
	trimmed := *dc
	trimmed.Containers = make([]collector.ContainerInfo, len(dc.Containers))
	copy(trimmed.Containers, dc.Containers)
	for i := range trimmed.Containers {
		trimmed.Containers[i].Logs = truncate(trimmed.Containers[i].Logs, 4000)
		trimmed.Containers[i].PreviousLogs = truncate(trimmed.Containers[i].PreviousLogs, 4000)
	}

	blob, err := json.MarshalIndent(trimmed, "", "  ")
	if err != nil {
		return "", fmt.Errorf("serialising context: %w", err)
	}
	return "Here is the pod snapshot:\n\n" + string(blob), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Keep the tail: the error that killed the process is at the end.
	return "...[truncated]...\n" + s[len(s)-max:]
}

// parseDiagnosis reads the model's JSON answer, tolerating the markdown fence
// that models add even when asked not to.
func parseDiagnosis(content string) (*rules.Diagnosis, error) {
	cleaned := stripCodeFence(content)

	var payload struct {
		Cause      string `json:"cause"`
		Suggestion string `json:"suggestion"`
	}
	if err := json.Unmarshal([]byte(cleaned), &payload); err != nil {
		// Some models ignore the JSON instruction entirely. Prose is still
		// better than an error, so long as it is clearly marked as a guess.
		if len(strings.TrimSpace(cleaned)) == 0 {
			return nil, errors.New("empty answer")
		}
		return &rules.Diagnosis{
			Cause:      strings.TrimSpace(cleaned),
			Suggestion: "",
			Confidence: rules.ConfidenceMedium,
		}, nil
	}

	if strings.TrimSpace(payload.Cause) == "" && strings.TrimSpace(payload.Suggestion) == "" {
		return nil, errors.New("answer had neither a cause nor a suggestion")
	}

	return &rules.Diagnosis{
		Cause:      strings.TrimSpace(payload.Cause),
		Suggestion: strings.TrimSpace(payload.Suggestion),
		Confidence: rules.ConfidenceMedium,
	}, nil
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// redactKey makes sure a transport error can never leak the API key, which
// http.Client sometimes echoes back inside a URL.
func redactKey(msg, key string) error {
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[REDACTED]")
	}
	return errors.New(msg)
}
