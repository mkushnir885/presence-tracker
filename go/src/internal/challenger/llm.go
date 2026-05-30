package challenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
)

const llmTimeout = 3 * time.Minute

const llmTemperature = 0.4

// LLMClient calls an OpenAI-compatible /v1/chat/completions endpoint to turn a
// transcript into a question bank.
type LLMClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func NewLLMClient(cfg config.AIBackendConfig) *LLMClient {
	return &LLMClient{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		http:    &http.Client{Timeout: llmTimeout},
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (c *LLMClient) Complete(ctx context.Context, system, user string) (string, error) {
	if c.baseURL == "" {
		return "", errors.New("challenger: LLM base_url not configured")
	}

	payload := chatRequest{
		Model:       c.model,
		Temperature: llmTemperature,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("challenger: llm encode: %w", err)
	}

	url := c.baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("challenger: llm request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("challenger: llm post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("challenger: llm read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("challenger: llm HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}

	var out chatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("challenger: llm decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("challenger: llm response had no choices")
	}
	return out.Choices[0].Message.Content, nil
}
