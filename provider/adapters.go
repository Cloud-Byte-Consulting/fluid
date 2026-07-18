package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Cloud-Byte-Consulting/fluid/runtime"
)

const maxOutputTokens = 8192

var httpClient = &http.Client{Timeout: 120 * time.Second}

// classify maps an HTTP status to fluid's retry semantics.
func classify(status int, body []byte) error {
	err := fmt.Errorf("provider returned %d: %s", status, truncate(body, 300))
	if status == 429 || status >= 500 {
		return runtime.Transient(err)
	}
	return err
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// post sends a JSON body and decodes a JSON response, classifying failures.
func post(ctx context.Context, url string, headers map[string]string, body, into any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return runtime.Transient(err) // network errors and timeouts are retryable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return runtime.Transient(err)
	}
	if resp.StatusCode != http.StatusOK {
		return classify(resp.StatusCode, data)
	}
	return json.Unmarshal(data, into)
}

// Anthropic speaks the Messages API.
type Anthropic struct {
	Key     string
	BaseURL string // default https://api.anthropic.com; overridable for tests
}

func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if a.Key == "" {
		return Response{}, fmt.Errorf("missing_credential: ANTHROPIC_API_KEY is not set")
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	err := post(ctx, base+"/v1/messages", map[string]string{
		"x-api-key":         a.Key,
		"anthropic-version": "2023-06-01",
	}, map[string]any{
		"model":      req.Model,
		"max_tokens": maxOutputTokens,
		"messages":   []map[string]string{{"role": "user", "content": req.Prompt}},
	}, &out)
	if err != nil {
		return Response{}, err
	}
	text := ""
	for _, c := range out.Content {
		text += c.Text
	}
	return Response{Text: text, Tokens: out.Usage.InputTokens + out.Usage.OutputTokens}, nil
}

// OpenAI speaks the Chat Completions API.
type OpenAI struct {
	Key     string
	BaseURL string // default https://api.openai.com; overridable for tests
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	if o.Key == "" {
		return Response{}, fmt.Errorf("missing_credential: OPENAI_API_KEY is not set")
	}
	base := o.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	err := post(ctx, base+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + o.Key,
	}, map[string]any{
		"model":    req.Model,
		"messages": []map[string]string{{"role": "user", "content": req.Prompt}},
	}, &out)
	if err != nil {
		return Response{}, err
	}
	text := ""
	if len(out.Choices) > 0 {
		text = out.Choices[0].Message.Content
	}
	return Response{Text: text, Tokens: out.Usage.TotalTokens}, nil
}
