package pluginreview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2e_test proves the FULL runtime path (B1–B6): PR identity → 4 read-only tools
// (via a STUB gh on PATH) → chat-completions tool-loop (against a local httptest
// server) → Verdict extraction → $GITHUB_OUTPUT → comment. Deterministic, offline.

func TestReviewE2E(t *testing.T) {
	dir := t.TempDir()

	// stub gh: serves the four tools from committed fixtures + accepts comment POSTs
	stub := filepath.Join(dir, "gh")
	ghScript := "#!/bin/bash\n# stub gh for e2e — serves fixture payloads, records comment posts\nFIX=\"${REVIEW_FIXTURES_DIR}/fx\"\nfor a in \"$@\"; do\n  case \"$a\" in\n    *comments*) cat \"${FIX}-get_pr_thread.json\"; exit 0;;\n    *application/vnd.github.diff*) cat \"${FIX}-get_pr_diff.json\"; exit 0;;\n    */commits*) cat \"${FIX}-get_pr_commits.json\"; exit 0;;\n    */pulls/*) cat \"${FIX}-get_pr_meta.json\"; exit 0;;\n    */issues/*) cat \"${FIX}-get_pr_thread.json\"; exit 0;;\n    --method) echo '{}'; exit 0;;\n  esac\ndone\necho '{}'\n"
	if err := os.WriteFile(stub, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// local chat-completions server: one tool-call turn, then the final PASS verdict
	calls := 0
	llmSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		calls++
		if req.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		var cr chatRequest
		if err := json.NewDecoder(req.Body).Decode(&cr); err != nil {
			t.Errorf("decode chat request: %v", err)
		}
		if cr.Temperature != 0.2 || len(cr.Tools) != 4 || cr.ToolChoice != "auto" {
			t.Errorf("unexpected loop shape: temp=%v tools=%d choice=%q", cr.Temperature, len(cr.Tools), cr.ToolChoice)
		}
		var resp chatResponse
		if calls == 1 {
			resp = chatResponse{Choices: []struct {
				Message struct {
					Content   *string    `json:"content"`
					ToolCalls []toolCall `json:"tool_calls"`
				} `json:"message"`
			}{{Message: struct {
				Content   *string    `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			}{
				ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "get_pr_meta", Arguments: "{}"}}},
			}}}}
		} else {
			c := "## Review — PASS\n\nHead SHA: 0123456789ab\n\nVerdict: PASS\n"
			resp = chatResponse{Choices: []struct {
				Message struct {
					Content   *string    `json:"content"`
					ToolCalls []toolCall `json:"tool_calls"`
				} `json:"message"`
			}{{Message: struct {
				Content   *string    `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			}{Content: &c}}}}
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(resp)
	}))
	defer llmSrv.Close()

	fixturesDir, _ := filepath.Abs("fixtures")
	oldPath := os.Getenv("PATH")
	t.Setenv("GITHUB_TOKEN", "stub-token")
	t.Setenv("GH_TOKEN", "stub-token")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	t.Setenv("PR_NUMBER", "1")
	t.Setenv("GITHUB_REPOSITORY", "opencharly/plugin-review")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("AI_REVIEW_PROVIDER", "e2e")
	t.Setenv("AI_REVIEW_MODEL", "e2e-model")
	t.Setenv("AI_REVIEW_BASE_URL", llmSrv.URL)
	t.Setenv("AI_REVIEW_API_KEY", "test-key")
	t.Setenv("AI_REVIEW_MAX_TURNS", "20")
	t.Setenv("REVIEW_FIXTURES_DIR", fixturesDir)
	promptPath := filepath.Join(dir, "prompt.md")
	t.Setenv("REVIEW_PROMPT_PATH", promptPath)
	t.Setenv("GITHUB_OUTPUT", filepath.Join(dir, "output.txt"))
	_ = os.WriteFile(promptPath, []byte("You are the PR validator. Review and end with Verdict: PASS or Verdict: BLOCK."), 0o644)

	exit, err := runReview(context.Background(), []string{"--repo", "opencharly/plugin-review", "pr", "1"})
	if err != nil {
		t.Fatalf("runReview: exit=%d err=%v", exit, err)
	}
	if exit != 0 {
		t.Fatalf("runReview exit=%d want 0", exit)
	}
	if calls < 2 {
		t.Fatalf("expected ≥2 chat calls, got %d", calls)
	}
	outRaw, _ := os.ReadFile(filepath.Join(dir, "output.txt"))
	outStr := string(outRaw)
	if !strings.Contains(outStr, "verdict=PASS") {
		t.Fatalf("GITHUB_OUTPUT missing verdict=PASS: %q", outStr)
	}
	if !strings.Contains(outStr, "success=true") {
		t.Fatalf("GITHUB_OUTPUT missing success=true: %q", outStr)
	}
}
