package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenAIIntentClient calls an OpenAI-compatible chat-completions gateway.
// The gateway is used only for intent understanding and candidate extraction;
// authorization, master-data matching, validation, and approval remain in the
// deterministic backend workflow.
type OpenAIIntentClient struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model       string                  `json:"model"`
	Messages    []chatCompletionMessage `json:"messages"`
	Temperature *float64                `json:"temperature,omitempty"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatCompletionMessage `json:"message"`
	} `json:"choices"`
}

const intentExtractionSystemPrompt = `You are an intent extraction component for a B2B order workflow.
Extract only candidate order fields explicitly supported by the user's message and return a JSON array.
Each array item must be an object compatible with the backend IntentCandidate schema and may contain only these fields:
product_sku, product_id, quantity, unit_price_cents, confidence, customer_id, payment_term, supplier_channel, warehouse_id, receive_address.
For example: {"product_sku":"SKU-C68","quantity":10,"unit_price_cents":0,"confidence":0.0}.
Use null, zero, or an empty string when a field is not present; never invent customer, product, price, contract, warehouse, permission, or approval data.
Do not perform authorization, master-data matching, pricing decisions, policy decisions, or approval decisions.
Return JSON only: no markdown fences, explanation, or surrounding object.`

// NewOpenAIIntentClient constructs an OpenAI-compatible intent client.
// apiKey is intentionally required here so an llm provider cannot silently
// start without authentication or fall back to another provider.
func NewOpenAIIntentClient(baseURL, apiKey, model string, timeout time.Duration) (*OpenAIIntentClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("llm base URL is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid llm base URL %q", baseURL)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("llm API key is required")
	}
	apiKey = strings.TrimSpace(apiKey)
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("llm model is required")
	}
	model = strings.TrimSpace(model)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OpenAIIntentClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: timeout},
	}, nil
}

// Complete extracts intent candidates from text. The metadata arguments are
// accepted to match IntentParser's supplier adapter; they are not trusted by
// the model and are not used to make business decisions.
func (c *OpenAIIntentClient) Complete(ctx context.Context, tenantID, groupID, msgID, text string) ([]byte, error) {
	if c == nil || c.httpClient == nil {
		return nil, errors.New("llm client is not configured")
	}
	zero := 0.0
	payload := chatCompletionRequest{
		Model: c.model,
		Messages: []chatCompletionMessage{
			{Role: "system", Content: intentExtractionSystemPrompt},
			{Role: "user", Content: text},
		},
		Temperature: &zero,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal llm request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create llm request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("llm gateway status %d: %s", resp.StatusCode, c.safeErrorText(string(detail)))
	}
	var decoded chatCompletionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode llm response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return nil, errors.New("llm response has no choices")
	}
	content := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if content == "" {
		return nil, errors.New("llm response choice has empty content")
	}
	return []byte(content), nil
}

func (c *OpenAIIntentClient) safeErrorText(detail string) string {
	detail = strings.ReplaceAll(detail, c.apiKey, "[REDACTED]")
	detail = strings.TrimSpace(detail)
	if len(detail) > 1024 {
		detail = detail[:1024] + "..."
	}
	return detail
}
