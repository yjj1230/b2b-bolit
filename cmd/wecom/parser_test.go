package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/config"
)

func TestBuildIntentParserLLMRequiresAPIKey(t *testing.T) {
	_, err := buildIntentParser(config.Config{
		AgentParserProvider: "llm",
		AgentLLMBaseURL:     "http://llm.test/v1",
		AgentLLMModel:       "gpt-5.6",
		AgentLLMTimeout:     time.Second,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "B2B_AGENT_LLM_API_KEY")
}

func TestBuildIntentParserLLMUsesConfiguredGateway(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[]"}}]}`))
	}))
	defer server.Close()

	parser, err := buildIntentParser(config.Config{
		AgentParserProvider: "llm",
		AgentLLMBaseURL:     server.URL + "/v1",
		AgentLLMAPIKey:      "test-key",
		AgentLLMModel:       "gpt-5.6",
		AgentLLMTimeout:     time.Second,
		AgentPromptVersion:  "v2",
	})
	require.NoError(t, err)
	got, err := parser.Parse(t.Context(), "tenant", "group", "msg", "SKU-C68 10件")
	require.NoError(t, err)
	require.Equal(t, "[]", strings.TrimSpace(string(got)))
}

func TestBuildIntentParserDefaultsToMock(t *testing.T) {
	parser, err := buildIntentParser(config.Config{})
	require.NoError(t, err)
	require.NotNil(t, parser)
}
