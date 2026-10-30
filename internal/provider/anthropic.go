package provider

import (
	"context"
	"strings"

	"github.com/himanshupphp/lazyfinger/internal/config"
)

type Anthropic struct {
	name string
	cfg  config.Provider
	key  string
}

func (p *Anthropic) Name() string { return p.name }
func (p *Anthropic) Remote() bool { return p.cfg.BaseURL == "" || isRemote(p.cfg.BaseURL) }

func (p *Anthropic) Complete(ctx context.Context, system string, msgs []Message) (string, error) {
	base := p.cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	maxTokens := p.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096 // required by this API
	}
	body := map[string]any{
		"model":      p.cfg.Model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages":   msgs,
	}
	if p.cfg.Temperature != nil {
		body["temperature"] = *p.cfg.Temperature
	}
	headers := map[string]string{
		"x-api-key":         p.key,
		"anthropic-version": "2023-06-01",
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := postJSON(ctx, strings.TrimRight(base, "/")+"/v1/messages", headers, body, &out); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}
