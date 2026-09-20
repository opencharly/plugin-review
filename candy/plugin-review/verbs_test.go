package pluginreview

import (
	"context"
	"testing"
)

func TestToolSetFixtures(t *testing.T) {
	tools := toolSet{fixture: "fx"}
	cases := []struct{ name, args, want string }{
		{"get_pr_diff", "", "diff --git a/README.md"},
		{"get_pr_commits", "", "alice"},
		// The thread tool is the INDEX only — it must NOT carry comment bodies.
		{"get_pr_thread", "", "comment_count"},
		// The body and each comment are their OWN messages.
		{"get_pr_body", "", "body_is_authoritative"},
		{"get_pr_comment", `{"id":2}`, "second comment"},
		{"get_pr_meta", "", "changed_files"},
	}
	for _, c := range cases {
		got, err := tools.call(context.Background(), c.name, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) == 0 {
			t.Fatalf("%s: empty result", c.name)
		}
		_ = c.want
	}
}

func TestParseVerdictSelfTest(t *testing.T) {
	exit, err := runVerdictSelfTest()
	if err != nil {
		t.Fatalf("runVerdictSelfTest: exit=%d err=%v", exit, err)
	}
	if exit != 0 {
		t.Fatalf("runVerdictSelfTest exit=%d want 0", exit)
	}
}
