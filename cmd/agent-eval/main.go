// Command agent-eval runs parser-only evaluation against the versioned Agent
// ordering golden dataset. It never creates orders or starts workflows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/evaluation"
	"b2b_pilot_v3/internal/wecom"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("agent-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	datasetPath := flags.String("dataset", "tests/fixtures/agent-ordering-golden.jsonl", "golden JSONL dataset")
	outPath := flags.String("out", "-", "report output path, or - for stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	if cfg.AgentLLMAPIKey == "" {
		return errors.New("agent evaluation requires B2B_AGENT_LLM_API_KEY")
	}
	datasetFile, err := os.Open(*datasetPath)
	if err != nil {
		return fmt.Errorf("open dataset: %w", err)
	}
	defer datasetFile.Close()
	cases, err := evaluation.LoadJSONL(datasetFile)
	if err != nil {
		return err
	}
	client, err := wecom.NewOpenAIIntentClient(cfg.AgentLLMBaseURL, cfg.AgentLLMAPIKey, cfg.AgentLLMModel, cfg.AgentLLMTimeout)
	if err != nil {
		return fmt.Errorf("configure evaluator client: %w", err)
	}
	parser := wecom.NewLLMParserWithComplete(client.Complete, cfg.AgentLLMModel, cfg.AgentLLMModel, cfg.AgentPromptVersion)
	report, err := evaluation.Evaluate(context.Background(), parser, cases, evaluation.Options{
		DatasetVersion: "v1",
		Model:          cfg.AgentLLMModel,
		PromptVersion:  cfg.AgentPromptVersion,
	})
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	encoded = append(encoded, '\n')
	if *outPath == "-" {
		_, err = stdout.Write(encoded)
		return err
	}
	if dir := filepath.Dir(*outPath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create report directory: %w", err)
		}
	}
	if err := os.WriteFile(*outPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
