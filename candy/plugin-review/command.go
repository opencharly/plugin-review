package pluginreview

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// command.go — the ONE command parser for `charly review`. It overlays CLI flags
// onto the Config FromEnv already built (env is the base; flags are the override),
// then runs the single pipeline. There is no mode switch: `--self-test` and
// `--self-test-verdict` are the only non-review modes (they exist so a bed can
// assert the engine is present and the verdict matcher is exact).
func parseCommand(args []string) (Config, string, error) {
	cfg := FromEnv()
	mode := "review"
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "--self-test":
			mode = "self-test"
		case a == "--self-test-verdict":
			mode = "self-test-verdict"
		case a == "--repo":
			if v, ok := next(); ok {
				cfg.Repo = v
			}
		case a == "--out" || a == "-o":
			if v, ok := next(); ok {
				cfg.OutPath = v
			}
		case a == "pr":
			if v, ok := next(); ok {
				if n, e := parseInt(v); e == nil {
					cfg.PR = n
				}
			}
		default:
			if n, e := parseInt(a); e == nil {
				cfg.PR = n
			}
		}
	}
	return cfg, mode, nil
}

func getenvAny(names ...string) string {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && v != "" {
			return v
		}
	}
	return ""
}

func parseInt(s string) (int, error) {
	n := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, fmt.Errorf("not an integer: %q", s)
	}
	return n, nil
}

// prFromEventPath reads the PR number from a GitHub Actions pull_request event
// payload ($GITHUB_EVENT_PATH). It is the fallback identity when PR_NUMBER is
// unset, so a run under a pull_request trigger needs no explicit number.
func prFromEventPath(path string) int {
	if path == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var ev struct {
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Number int `json:"number"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return 0
	}
	if ev.PullRequest.Number > 0 {
		return ev.PullRequest.Number
	}
	return ev.Number
}

// firstLine returns at most max BYTES of s collapsed to one line, with an
// ellipsis when it is shorter than the body.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + "…"
}
