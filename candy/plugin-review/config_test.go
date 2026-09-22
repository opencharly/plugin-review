package pluginreview

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestConfigSurfaceMatchesCharlyYML asserts the candy's declared env surface
// (charly.yml) EQUALS the env the engine reads (config.go FromEnv). It keeps the
// two lists in lockstep, so a NEW knob cannot be added to the code without being
// declared (and vice versa). The values themselves arrive via the process
// environment (charly passes the ambient env to a command plugin); the charly.yml
// declaration is the documented surface and the home the future charly
// env-injection capability will populate.
func TestConfigSurfaceMatchesCharlyYML(t *testing.T) {
	declared := declaredEnv(t, "charly.yml")
	read := readsEnv(t)

	missingDecl := diff(read, declared)
	if len(missingDecl) > 0 {
		t.Errorf("the engine reads env vars NOT declared in charly.yml (the host will not pass them): %v", missingDecl)
	}
	declaredNotRead := diff(declared, read)
	if len(declaredNotRead) > 0 {
		t.Errorf("charly.yml declares env vars the engine never reads (dead surface): %v", declaredNotRead)
	}
}

// allowedUnread are vars that are read by the SHARED layer (ghkit) or by GitHub
// Actions itself, so they are declared for the container but not referenced in
// this package's source.
var allowedUnread = map[string]bool{
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "GITHUB_OUTPUT": true,
	"GITHUB_EVENT_PATH": true, "GITHUB_SERVER_URL": true, "GITHUB_RUN_ID": true,
	"GITHUB_REPOSITORY": true, "PR_NUMBER": true,
}

// readsEnv scans the package source for os.LookupEnv / envStr / envInt / envBool
// / envSeconds / envList / envFloatPtr / envInt64Ptr / getenvAny / os.Getenv
// literals. It is deliberately a source scan: it catches a NEW knob the moment it
// is added, before any runtime test could.
func readsEnv(t *testing.T) map[string]bool {
	t.Helper()
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?:LookupEnv|Getenv|envStr|envStrLookup|envInt|envInt64|envInt64Ptr|envSeconds|envFloatPtr|envBool|envList)\("([A-Z][A-Z0-9_]+)"`)
	anyRe := regexp.MustCompile(`getenvAny\(([^)]*)\)`)
	strRe := regexp.MustCompile(`"([A-Z][A-Z0-9_]+)"`)
	out := map[string]bool{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		for _, m := range anyRe.FindAllStringSubmatch(string(b), -1) {
			for _, s := range strRe.FindAllStringSubmatch(m[1], -1) {
				out[s[1]] = true
			}
		}
	}
	return out
}

// declaredEnv extracts the env_accept names from charly.yml.
func declaredEnv(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`- name: ([A-Z][A-Z0-9_]+)`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out
}

func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] && !allowedUnread[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestModelDefaultsMatchCharlyYML pins the SHIPPED model-behaviour defaults to a
// single value set across both surfaces: config.go's constants (used when charly
// invokes the plugin with a bare environment) and charly.yml's var: block (used
// when charly materializes the candy's declared defaults). A change to one
// without the other would silently ship two different best-known configurations.
func TestModelDefaultsMatchCharlyYML(t *testing.T) {
	b, err := os.ReadFile("charly.yml")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	declared := map[string]string{}
	inVar := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "var:") {
			inVar = true
			continue
		}
		if inVar && strings.HasPrefix(strings.TrimSpace(line), "env_accept:") {
			break
		}
		if !inVar {
			continue
		}
		re := regexp.MustCompile(`^\s+([A-Z][A-Z0-9_]+):\s*"?([^"\s]+)"?\s*$`)
		if m := re.FindStringSubmatch(line); m != nil {
			declared[m[1]] = m[2]
		}
	}
	wantFloat := map[string]float64{
		"AI_REVIEW_TEMPERATURE":       reviewTemperature,
		"AI_REVIEW_TOP_P":             reviewTopP,
		"AI_REVIEW_FREQUENCY_PENALTY": defaultFrequencyPenalty,
		"AI_REVIEW_PRESENCE_PENALTY":  defaultPresencePenalty,
	}
	for k, v := range wantFloat {
		got, ok := declared[k]
		if !ok {
			t.Errorf("charly.yml var:%s missing — the shipped default must match config.go (%g)", k, v)
			continue
		}
		f, err := strconv.ParseFloat(got, 64)
		if err != nil || f != v {
			t.Errorf("charly.yml var:%s = %q, want %g — the shipped default must match config.go", k, got, v)
		}
	}
	wantStr := map[string]string{
		"AI_REVIEW_REASONING_EFFORT": DefaultReasoningEffort,
		"AI_REVIEW_MAX_TOKENS":       fmt.Sprintf("%d", DefaultMaxTokens),
		// POST_COMMENT is a bool; its shipped default is FALSE (opt-in). It must
		// agree across both surfaces, like every other shipped default.
		"AI_REVIEW_POST_COMMENT": "false",
	}
	for k, v := range wantStr {
		if got, ok := declared[k]; !ok || got != v {
			t.Errorf("charly.yml var:%s = %q (present=%v), want %q — the shipped default must match config.go", k, got, ok, v)
		}
	}
}

// TestPostCommentDefaultsOff pins the outward-facing default: AI_REVIEW_POST_COMMENT
// is FALSE when unset AND when explicitly empty. Posting to a GitHub PR is a side
// effect, so a caller must opt in; the org sets the variable to "true" in its
// Actions settings and the workflow forwards it. This test fails if the default is
// flipped back to true, or if envBool starts treating "" as the default rather than
// false (which would make the workflow's `vars.X || ”` forward an opt-IN for an
// unset org var — the 2026-09-22 no-comment incident).
func TestPostCommentDefaultsOff(t *testing.T) {
	os.Unsetenv("AI_REVIEW_POST_COMMENT")
	if c := FromEnv(); c.PostComment {
		t.Errorf("AI_REVIEW_POST_COMMENT unset must default to false, got %v", c.PostComment)
	}
	t.Setenv("AI_REVIEW_POST_COMMENT", "")
	if c := FromEnv(); c.PostComment {
		t.Errorf("AI_REVIEW_POST_COMMENT explicitly empty must be false (the workflow forwards \"\"), got %v", c.PostComment)
	}
	t.Setenv("AI_REVIEW_POST_COMMENT", "true")
	if c := FromEnv(); !c.PostComment {
		t.Errorf("AI_REVIEW_POST_COMMENT=true must opt in, got %v", c.PostComment)
	}
}

// TestVerdictMatching pins the line-anchored verdict contract the gate parses.
func TestVerdictMatching(t *testing.T) {
	cases := []struct {
		in       string
		wantN    int
		wantPass string
	}{
		{"## Review — PASS\n\nVerdict: PASS\n", 1, "PASS"},
		{"Verdict: BLOCK\n", 1, "BLOCK"},
		{"Verdict:  PASS  \n", 1, "PASS"},
		{"a mention of Verdict: PASS inside prose\n", 0, ""}, // line-anchored: prose is NOT a verdict
		{"no verdict\n", 0, ""},
		{"Verdict: PASS\nVerdict: BLOCK\n", 2, ""},
	}
	for _, c := range cases {
		_, distinct, n := extractVerdict(c.in)
		if n != c.wantN {
			t.Errorf("extractVerdict(%q): n=%d want %d", c.in, n, c.wantN)
		}
		if c.wantPass != "" && (len(distinct) != 1 || distinct[0] != c.wantPass) {
			t.Errorf("extractVerdict(%q): distinct=%v want [%s]", c.in, distinct, c.wantPass)
		}
	}
}
