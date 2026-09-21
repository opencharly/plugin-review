package pluginreview

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestConfigSurfaceMatchesCharlyYML is the mechanical guard for the defect that
// made CI runs undebuggable: AI_REVIEW_DEBUG was set in the workflow but absent
// from the candy's charly.yml env_accept, so the charly host never passed it to
// the plugin. Every env var the engine READS must be DECLARED in charly.yml, and
// vice versa — one list, asserted.
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
