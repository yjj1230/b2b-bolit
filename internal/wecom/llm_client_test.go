package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIIntentClientCompleteSendsAuthModelAndPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		var req chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "gpt-5.6" || req.Temperature == nil || *req.Temperature != 0 {
			t.Fatalf("request = %+v", req)
		}
		if len(req.Messages) != 2 || !strings.Contains(req.Messages[0].Content, "candidate") ||
			!strings.Contains(req.Messages[1].Content, "SKU-C68 10件") {
			t.Fatalf("messages = %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"product_sku\":\"SKU-C68\",\"quantity\":10}]"}}]}`))
	}))
	defer server.Close()

	client, err := NewOpenAIIntentClient(server.URL+"/v1", "test-key", "gpt-5.6", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Complete(context.Background(), "tenant", "group", "msg", "SKU-C68 10件")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "SKU-C68") {
		t.Fatalf("response = %s", got)
	}
}

func TestOpenAIIntentClientCompleteRejectsNon2xxWithoutLeakingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("upstream rejected test-key"))
	}))
	defer server.Close()

	client, err := NewOpenAIIntentClient(server.URL+"/v1", "test-key", "gpt-5.6", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), "tenant", "group", "msg", "text")
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatalf("error leaked API key: %v", err)
	}
}

func TestOpenAIIntentClientCompleteRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	client, err := NewOpenAIIntentClient(server.URL, "test-key", "gpt-5.6", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), "tenant", "group", "msg", "text")
	if err == nil || !strings.Contains(err.Error(), "choices") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAIIntentClientCompleteHonorsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := NewOpenAIIntentClient(server.URL, "test-key", "gpt-5.6", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Complete(ctx, "tenant", "group", "msg", "text")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
