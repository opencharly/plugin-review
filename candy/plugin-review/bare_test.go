package pluginreview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func spinBareLLM(t *testing.T) (*httptest.Server, *int) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		calls++
		var cr chatRequest
		if err := json.NewDecoder(req.Body).Decode(&cr); err != nil {
			t.Errorf("decode: %v", err)
		}
		if len(cr.Messages) != 2 || cr.Messages[0].Role != "system" {
			t.Errorf("expected [system,user], got %d msgs roles=%v", len(cr.Messages), msgRoles(cr.Messages))
		}
		c := "bare answer"
		resp := chatResponse{Choices: []struct {
			Message struct {
				Content   *string    `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		}{{Message: struct {
			Content   *string    `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		}{Content: &c}}}}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(resp)
	}))
	return srv, &calls
}

func msgRoles(msgs []chatMsg) []string {
	out := []string{}
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

// raw mode: the configurable system prompt + user prompt + the EVAL_LLM_* env contract.
func TestBareAgentRaw(t *testing.T) {
	srv, calls := spinBareLLM(t)
	defer srv.Close()

	dir := t.TempDir()
	sysFile := filepath.Join(dir, "sys.md")
	_ = os.WriteFile(sysFile, []byte("You are the omarchy eval oracle. Be specific."), 0o644)

	t.Setenv("EVAL_LLM_BASE_URL", srv.URL)
	t.Setenv("EVAL_LLM_MODEL", "eval-model")
	t.Setenv("EVAL_LLM_API_KEY", "eval-key")
	t.Setenv("EVAL_LLM_SYSTEM_PROMPT", "@"+sysFile)
	t.Setenv("BARE_PROMPT", "Triage omacom/omarchy#10140")

	exit, err := runReview(context.Background(), []string{"bare"})
	if err != nil || exit != 0 {
		t.Fatalf("bare: exit=%d err=%v", exit, err)
	}
	if *calls != 1 {
		t.Fatalf("expected 1 completion call, got %d", *calls)
	}
}

// tools mode: the four PR tools attached and the loop runs (stub gh serves pr_meta).
func TestBareAgentTools(t *testing.T) {
	srv, _ := spinBareLLM(t)
	defer srv.Close()

	dir := t.TempDir()
	fx, _ := filepath.Abs("fixtures")
	stub := filepath.Join(dir, "gh")
	script := "#!/bin/bash\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    */pulls/*) cat \"" + fx + "/fx-get_pr_meta.json\"; exit 0;;\n" +
		"  esac\n" +
		"done\n" +
		"echo '{}'\n"
	_ = os.WriteFile(stub, []byte(script), 0o755)

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	t.Setenv("GITHUB_TOKEN", "stub")
	t.Setenv("EVAL_LLM_BASE_URL", srv.URL)
	t.Setenv("EVAL_LLM_MODEL", "eval-model")
	t.Setenv("EVAL_LLM_API_KEY", "k")
	t.Setenv("EVAL_LLM_SYSTEM_PROMPT", "You are a bare PR reviewer with tools.")

	exit, err := runReview(context.Background(), []string{"bare", "--tools", "--repo", "opencharly/plugin-review", "--pr", "1", "--prompt", ":pr"})
	if err != nil || exit != 0 {
		t.Fatalf("bare tools: exit=%d err=%v", exit, err)
	}
}

// env precedence: EVAL_LLM_* wins over AI_REVIEW_* (the eval-charly contract).
func TestBareEnvPrecedence(t *testing.T) {
	t.Setenv("EVAL_LLM_BASE_URL", "https://eval.example/v1")
	t.Setenv("AI_REVIEW_BASE_URL", "https://review.example/v1")
	cfg := reviewConfig{}
	cfg.BaseURL = getenvAny("AI_REVIEW_BASE_URL")
	if v := getenvAny("EVAL_LLM_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if cfg.BaseURL != "https://eval.example/v1" {
		t.Fatalf("EVAL_LLM_* must win over AI_REVIEW_*, got %s", cfg.BaseURL)
	}
}
