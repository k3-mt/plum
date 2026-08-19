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
}

func NewAnthropic(model string) (*Anthropic, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set (or set synthesis.provider = \"offline\" in .plum/config.toml)")
	}
	if model == "" {
		model = "claude-sonnet-5"
	}
	return &Anthropic{Model: model, APIKey: key, Client: &http.Client{Timeout: 5 * time.Minute}}, nil
}

func (a *Anthropic) Name() string { return "anthropic/" + a.Model }

func (a *Anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      a.Model,
		"max_tokens": 8000,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.Client.Do(req)
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
	var sb bytes.Buffer
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}
