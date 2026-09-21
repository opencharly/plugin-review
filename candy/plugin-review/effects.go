package pluginreview

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"

	"github.com/opencharly/sdk/llmkit"
)

// effects.go — $GITHUB_OUTPUT compatibility + the debug usage formatter. These
// are the only places the engine writes outside its return value, so they live
// together and are called from exactly one site each (Emit, dbg).

// writeGHOutputs writes response/success/verdict to $GITHUB_OUTPUT using the
// heredoc form for multi-line values (GitHub's output-file spec).
func writeGHOutputs(response string, ok bool, distinct []string) {
	p := getenvAny("GITHUB_OUTPUT")
	if p == "" {
		fmt.Println("plugin-review: GITHUB_OUTPUT not set; outputs skipped")
		return
	}
	verdict := ""
	if ok && len(distinct) == 1 {
		verdict = distinct[0]
	}
	var sb strings.Builder
	writeOutput(&sb, "response", response)
	writeOutput(&sb, "success", fmt.Sprint(ok))
	writeOutput(&sb, "verdict", verdict)
	_ = appendFile(p, []byte(sb.String()))
}

func writeOutput(sb *strings.Builder, name, value string) {
	if !strings.ContainsAny(value, "\n\r") {
		sb.WriteString("\n" + name + "=" + value)
		return
	}
	delim := "piout_" + randAlpha(8)
	for strings.Contains(value, delim) {
		delim = "piout_" + randAlpha(8)
	}
	sb.WriteString("\n" + name + "<<" + delim + "\n" + value + "\n" + delim)
}

// appendFile appends to $GITHUB_OUTPUT (GitHub pre-creates it; a local run may
// not have it).
func appendFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

// randAlpha returns n lowercase-alphanumeric chars from crypto/rand (the heredoc
// delimiter only needs to be unlikely to collide).
func randAlpha(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("x", n)
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// usageString renders the provider's token accounting for the debug trace. It is
// nil-safe (the provider may report none) and surfaces the reasoning-token split
// when present, which is what makes an unbounded generation measurable.
func usageString(u *llmkit.Usage) string {
	if u == nil {
		return "none"
	}
	return fmt.Sprintf("prompt=%d completion=%d total=%d reasoning=%d",
		u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.ReasoningTokens)
}
