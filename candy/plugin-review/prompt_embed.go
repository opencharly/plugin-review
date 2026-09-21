package pluginreview

import _ "embed"

// The review rulebook is EMBEDDED in the binary — there is no runtime prompt file
// for a PR or the environment to redirect. AI_REVIEW_PROMPT_EXTRA appends
// operator text without replacing the shipped rulebook.
//
//go:embed prompt.md
var embeddedPrompt string
