package main

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunRequiresLLMAPIKey(t *testing.T) {
	old, had := os.LookupEnv("B2B_AGENT_LLM_API_KEY")
	_ = os.Unsetenv("B2B_AGENT_LLM_API_KEY")
	defer func() {
		if had {
			_ = os.Setenv("B2B_AGENT_LLM_API_KEY", old)
		} else {
			_ = os.Unsetenv("B2B_AGENT_LLM_API_KEY")
		}
	}()

	err := run([]string{"-dataset", "tests/fixtures/agent-ordering-golden.jsonl"}, io.Discard, io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "B2B_AGENT_LLM_API_KEY")
}
