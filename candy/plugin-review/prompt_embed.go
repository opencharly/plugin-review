package pluginreview

import _ "embed"

// The SHIPPED review rulebook — a fully GENERIC default. It is embedded so the
// binary always carries a sane rulebook, and it is a DEFAULT only: an operator
// REPLACES it wholesale with AI_REVIEW_PROMPT (an org variable passed to the
// runner). No project-specific rule is baked in, and there is no file path.
//
//go:embed prompt.md
var embeddedPrompt string
