package pluginreview

import (
	"context"
	"strings"
	"testing"
)

func TestDecodePlanValidation(t *testing.T) {
	good := `version: 1
plugins: []
steps:
  - id: s1
    kind: command
    command: "echo hello"
    verdict: none
`
	p, err := decodePlan([]byte(good))
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("good plan rejected: %v", err)
	}
	bad := `version: 1
steps:
  - id: s1
    kind: command
    command: "echo hi"
    bogus_field: true
`
	if _, err := decodePlan([]byte(bad)); err == nil {
		t.Fatalf("unknown field must fail loudly")
	}
	badVerdict := `version: 1
steps:
  - id: s1
    kind: command
    command: "echo hi"
    verdict: maybe
`
	if _, err := decodePlan([]byte(badVerdict)); err == nil {
		t.Fatalf("unsupported verdict must fail")
	}
}

func TestRunPlanStepCommand(t *testing.T) {
	cfg := reviewConfig{Provider: "test", Model: "test", BaseURL: "http://localhost"}
	out, verdict, err := runPlanStep(context.Background(), cfg, planStep{ID: "s", Kind: "command", Command: "echo 'Verdict: PASS'"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Verdict: PASS") || verdict != "PASS" {
		t.Fatalf("out=%q verdict=%q", out, verdict)
	}
	// continue_on_error semantics: failing command returns error
	if _, _, err := runPlanStep(context.Background(), cfg, planStep{ID: "s2", Kind: "command", Command: "exit 3"}); err == nil {
		t.Fatalf("failing command must error")
	}
}
