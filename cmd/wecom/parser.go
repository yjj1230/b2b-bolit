package main

import (
	"fmt"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/wecom"
)

// buildIntentParser is the single provider assembly point for agent ordering.
// The provider controls only candidate extraction; all business validation and
// approval remains in the backend workflow.
func buildIntentParser(cfg config.Config) (wecom.IntentParser, error) {
	switch cfg.AgentParserProvider {
	case "", "mock":
		return wecom.NewMockParser(), nil
	case "llm":
		if cfg.AgentLLMAPIKey == "" {
			return nil, fmt.Errorf("agent parser provider llm requires B2B_AGENT_LLM_API_KEY")
		}
		client, err := wecom.NewOpenAIIntentClient(cfg.AgentLLMBaseURL, cfg.AgentLLMAPIKey, cfg.AgentLLMModel, cfg.AgentLLMTimeout)
		if err != nil {
			return nil, fmt.Errorf("configure llm intent client: %w", err)
		}
		return wecom.NewLLMParserWithComplete(client.Complete, cfg.AgentLLMModel, cfg.AgentLLMModel, cfg.AgentPromptVersion), nil
	default:
		return nil, fmt.Errorf("unsupported agent parser provider %q (want mock or llm)", cfg.AgentParserProvider)
	}
}
