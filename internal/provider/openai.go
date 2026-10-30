package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/himanshupphp/lazyfinger/internal/config"
)

type OpenAI struct {
	name string
	cfg  config.Provider
	key  string
}

func (p *OpenAI) Name() string { return p.name }
func (p *OpenAI) Remote() bool { return isRemote(p.cfg.BaseURL) }

func (p *OpenAI) Complete(ctx context.Context, system string, msgs []Message) (string, error) {
	all := append([]Message{{Role: "system", Content: system}}, msgs...)
	body := map[string]any{"model": p.cfg.Model, "messages": all}
	// Only send these if configured: some newer hosted models reject them.
	if p.cfg.MaxTokens > 0 {
		body["max_tokens"] = p.cfg.MaxTokens
	}
	if p.cfg.Temperature != nil {
		body["temperature"] = *p.cfg.Temperature
	}
	headers := map[string]string{}
	if p.key != "" {
		headers["Authorization"] = "Bearer " + p.key
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	endpoint := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
	if err := postJSON(ctx, endpoint, headers, body, &out); err != nil {
		if !p.Remote() {
			return "", fmt.Errorf("couldn't reach %s (is your local model server running?): %w", p.cfg.BaseURL, err)
		}
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("model returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}
