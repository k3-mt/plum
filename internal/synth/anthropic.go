package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Anthropic is the fresh-context synthesis provider. It deliberately carries no
// conversation history: the whole point is that this model has never seen the
// building agent's reasoning (P2).
type Anthropic struct {
	Model  string
	APIKey string
	Client *http.Client
	// think asks the model to reason before answering. Off by default: the
	// synthesis pass it was built for is a structured extraction, not a
	// question.
	think bool
}

func NewAnthropic(model string) (*Anthropic, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set (or set synthesis.provider = \"offline\" in .plum/config.toml)")
	}
	if model == "" {
		model = "claude-opus-5"
	}
	return &Anthropic{Model: model, APIKey: key, Client: &http.Client{Timeout: 5 * time.Minute}}, nil
}

// Thinking returns a copy that lets the model reason before answering.
//
// Adaptive rather than a fixed budget: the model decides how much a given
// question needs, which is right here because the questions vary enormously —
// "what does this line do" and "why does this loop terminate" are not the same
// amount of work. Synthesis does not use it, because that pass is a structured
// extraction with a fixed shape; explaining a fragment of code somebody pointed
// at is open-ended, and is the case reasoning is for.
func (a *Anthropic) Thinking() Provider {
	c := *a
	c.think = true
	return &c
}

func (a *Anthropic) Name() string { return "anthropic/" + a.Model }

func (a *Anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	req := map[string]any{
		"model": a.Model,
		// Room for the answer and, when thinking is on, for the reasoning that
		// precedes it. Too small a ceiling truncates mid-sentence and the retry
		// costs more than the headroom would have.
		"max_tokens": 16000,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	}
	if a.think {
		req["thinking"] = map[string]any{"type": "adaptive"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.Client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic api %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	// Only the text blocks. With thinking on, the response carries reasoning
	// blocks first; those are the model working, not its answer.
	var sb bytes.Buffer
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}
