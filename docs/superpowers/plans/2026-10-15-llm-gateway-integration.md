# LLM Gateway Integration Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

Goal: Connect the existing Agent intent parser to the company OpenAI-compatible gateway using model gpt-5.6, while keeping API credentials outside the repository and preserving deterministic backend validation.

Architecture: Add a small HTTP client in internal/wecom that calls POST {base_url}/chat/completions, extracts the assistant message, and returns the existing candidate-array contract consumed by LLMParser. Inject the client through NewLLMParserWithComplete; wire it in cmd/wecom/main.go only when B2B_AGENT_PARSER_PROVIDER=llm. Configuration comes from environment variables, with the API key required at runtime and never logged or persisted.

Tech Stack: Go 1.25.4, net/http, encoding/json, httptest, existing testify assertions.

Spec: docs/superpowers/specs/2026-10-15-agent-multiturn-ordering-design.md.

## Global Constraints

- LLM output is an untrusted candidate array; it never authorizes, approves, reserves stock, or creates a formal order.
- The API key must be read from B2B_AGENT_LLM_API_KEY; do not hardcode it, write it to files, or include it in logs/errors.
- Default parser remains mock; selecting llm without a key must fail closed during service startup.
- The gateway is OpenAI-compatible and uses POST /chat/completions below the configured /v1 base URL.
- Model default is gpt-5.6; timeout default is 30 seconds.
- Tests must run without network access by using httptest.Server.

---

### Task 1: Add configuration for the LLM gateway

Files:
- Modify: internal/config/config.go
- Modify: internal/config/config_test.go

Interfaces:
- Produces Config.AgentLLMBaseURL, Config.AgentLLMAPIKey, Config.AgentLLMModel, and Config.AgentLLMTimeout.

- [ ] Step 1: Write the failing configuration tests

Add assertions that defaults are:

~~~go
if c.AgentLLMBaseURL != "http://121.40.34.96:18097/v1" ||
   c.AgentLLMModel != "gpt-5.6" ||
   c.AgentLLMTimeout != 30*time.Second ||
   c.AgentLLMAPIKey != "" {
    t.Fatalf("agent llm defaults mismatch: %+v", c)
}
~~~

Add environment override coverage for B2B_AGENT_LLM_BASE_URL, B2B_AGENT_LLM_API_KEY, B2B_AGENT_LLM_MODEL, and B2B_AGENT_LLM_TIMEOUT, and unset each variable with defer.

- [ ] Step 2: Run the focused tests and verify they fail

Run: go test ./internal/config -run 'TestLoad(Default|EnvOverrides)$'

Expected: compile failure because the new Config fields and environment loading do not exist.

- [ ] Step 3: Implement configuration fields and environment loading

Add fields to Config and load them with:

~~~go
AgentLLMBaseURL: getenv("B2B_AGENT_LLM_BASE_URL", "http://121.40.34.96:18097/v1"),
AgentLLMAPIKey:  getenv("B2B_AGENT_LLM_API_KEY", ""),
AgentLLMModel:   getenv("B2B_AGENT_LLM_MODEL", "gpt-5.6"),
AgentLLMTimeout: getenvDuration("B2B_AGENT_LLM_TIMEOUT", 30*time.Second),
~~~

- [ ] Step 4: Run the focused tests and verify they pass

Run: go test ./internal/config -run 'TestLoad(Default|EnvOverrides)$'

Expected: PASS.

### Task 2: Implement the OpenAI-compatible intent client

Files:
- Create: internal/wecom/llm_client.go
- Create: internal/wecom/llm_client_test.go

Interfaces:
- Produces NewOpenAIIntentClient(baseURL, apiKey, model string, timeout time.Duration) (*OpenAIIntentClient, error).
- Produces (*OpenAIIntentClient).Complete(ctx, tenantID, groupID, msgID, text string) ([]byte, error) with the exact signature required by NewLLMParserWithComplete.

- [ ] Step 1: Write failing HTTP contract tests

Use httptest.NewServer and write tests for:

~~~go
func TestOpenAIIntentClientCompleteSendsAuthModelAndPrompt(t *testing.T)
func TestOpenAIIntentClientCompleteRejectsNon2xxWithoutLeakingKey(t *testing.T)
func TestOpenAIIntentClientCompleteRejectsMalformedResponse(t *testing.T)
func TestOpenAIIntentClientCompleteHonorsContextCancellation(t *testing.T)
~~~

The success handler must assert request path is /v1/chat/completions when the configured base URL is the server URL plus /v1; Authorization is Bearer <test-key>; request model is gpt-5.6; messages include a system instruction that says the model only extracts candidate fields and must return a JSON array; and the user message contains the source text.

The error test must return a body containing the test key and verify the returned error does not contain that key.

- [ ] Step 2: Run the focused tests and verify they fail

Run: go test ./internal/wecom -run 'TestOpenAIIntentClient'

Expected: compile failure because the client does not exist.

- [ ] Step 3: Implement the minimal client

Implement:
- base URL normalization that removes trailing slashes and appends /chat/completions exactly once;
- JSON request types for model, messages, and temperature: 0; omit response_format for gateway compatibility and enforce JSON through the system prompt;
- Authorization: Bearer <apiKey> and Content-Type: application/json;
- request creation with http.NewRequestWithContext;
- response status validation before decoding;
- bounded error-body reading (4 KiB), never including request headers in errors;
- decoding choices[0].message.content and errors for empty choices/content;
- timeout through the injected http.Client.

The system prompt must state that the result is an untrusted candidate array and use the existing fields product_sku, product_id, quantity, unit_price_cents, confidence, customer_id, payment_term, supplier_channel, warehouse_id, and receive_address. It must instruct the model to leave unknown fields empty or zero and never invent master-data identifiers.

- [ ] Step 4: Run the focused tests and verify they pass

Run: go test ./internal/wecom -run 'TestOpenAIIntentClient'

Expected: PASS.

### Task 3: Connect the client to LLMParser and verify schema preservation

Files:
- Modify: internal/wecom/intent.go
- Modify: internal/wecom/intent_deferred_test.go
- Create: internal/wecom/llm_parser_test.go

Interfaces:
- LLMParser.Parse remains the existing IntentParser method and continues returning a JSON array of workflows.IntentCandidate.
- The HTTP client remains isolated behind the existing Complete function injection.

- [ ] Step 1: Write failing parser integration tests

Add tests that inject a Complete function returning a valid candidate array and assert LLMParser.Parse returns the same fields. Add a test for a JSON object or markdown-wrapped response and assert it is rejected rather than silently accepted.

~~~go
func TestLLMParserPreservesCandidateSchema(t *testing.T)
func TestLLMParserRejectsNonArraySchema(t *testing.T)
~~~

- [ ] Step 2: Run the focused tests and verify they fail

Run: go test ./internal/wecom -run 'TestLLMParser' -tags deferred

Expected: the schema-preservation test may already pass with existing code; retain it as a regression test. The malformed-response test must fail against any implementation that accepts arbitrary model output.

- [ ] Step 3: Implement only required parser changes

Keep the existing strict candidate-array decoder. If the new gateway client returns content bytes exactly, no parser behavior change is needed. Update only error text if tests require a clearer distinction between missing client configuration and gateway request failure.

- [ ] Step 4: Run parser and package tests

Run: go test ./internal/wecom -tags deferred

Expected: PASS, including existing MockParser and unconfigured LLMParser tests.

### Task 4: Wire the gateway into the WeCom service

Files:
- Modify: cmd/wecom/main.go
- Create: cmd/wecom/parser.go if keeping construction out of main.go improves testability
- Create: cmd/wecom/parser_test.go

Interfaces:
- B2B_AGENT_PARSER_PROVIDER=mock keeps NewMockParser().
- B2B_AGENT_PARSER_PROVIDER=llm requires a non-empty B2B_AGENT_LLM_API_KEY and constructs NewOpenAIIntentClient plus NewLLMParserWithComplete.

- [ ] Step 1: Write the failing wiring test

Test the provider-selection helper so that mock returns a MockParser; llm with an empty key returns an error containing B2B_AGENT_LLM_API_KEY; and llm with configured values returns an IntentParser without making a network request.

- [ ] Step 2: Run the focused test and verify it fails

Run: go test ./cmd/wecom -run 'Test.*Parser'

Expected: compile failure because the provider-selection helper does not exist.

- [ ] Step 3: Implement provider selection and main wiring

Create a helper that receives config.Config, validates the LLM key when provider is llm, constructs the HTTP client with the configured timeout, and returns the parser. In main, log only the provider name and model name; never log the key or the Authorization header. If LLM configuration is invalid, fail closed with log.Fatalf before serving requests.

- [ ] Step 4: Run the focused test and package build

Run: go test ./cmd/wecom -run 'Test.*Parser'
Then: go test ./cmd/wecom

Expected: PASS.

### Task 5: Document deployment configuration and verify the repository

Files:
- Modify: README.md or DEPLOY_NOTES.md
- Modify: .env.example only if the repository already tracks an example env file.

- [ ] Step 1: Add deployment documentation without secrets

Document:

~~~text
B2B_AGENT_PARSER_PROVIDER=llm
B2B_AGENT_LLM_BASE_URL=http://121.40.34.96:18097/v1
B2B_AGENT_LLM_MODEL=gpt-5.6
B2B_AGENT_LLM_TIMEOUT=30
B2B_AGENT_LLM_API_KEY=<secret-manager-value>
~~~

State that the key must come from a secret manager or process environment and must not be committed.

- [ ] Step 2: Run formatting and all Go tests

Run: gofmt -w on all modified Go files.
Then run: go test ./... and go test -tags deferred ./...

Expected: both commands exit 0. If the repository has unrelated pre-existing failures, record the exact package and failure without claiming completion.

- [ ] Step 3: Verify no secret entered the workspace

Run: rg -n --hidden --glob '!*.html' --glob '!*.sum' 'B2B_AGENT_LLM_API_KEY=' .
Expected: only documentation placeholders or empty defaults; the real key must not appear.

## Verification Checklist

- [ ] HTTP client tests prove path, auth header, model, timeout, schema extraction, non-2xx handling, and cancellation.
- [ ] Parser tests prove the existing candidate-array contract remains intact.
- [ ] Provider wiring fails closed when the LLM key is absent.
- [ ] Mock parser remains the default and tests do not call the real gateway.
- [ ] go test ./... and go test -tags deferred ./... pass with fresh output.
- [ ] Repository contains no API key.

