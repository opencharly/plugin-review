// The plugin's OWN CUE schema — the typed plugin_input for the verb:pr check
// verbs. SINGLE SOURCE for this plugin's params, used two ways (the same contract
// the reference plugin-example-external uses):
//
//  1. GENERATE the Go param struct — `cue exp gengotypes` (wrapping with
//     `package params` + @go annotations) emits params/cue_types_gen.go, so the
//     provider decodes plugin_input into a TYPED struct, never a hand-parsed
//     map[string]any.
//  2. VALIDATE authored input AT RUNTIME — the provider SERVES this source over
//     the Describe channel (schema_cue via sdk.BuildCapabilities); the host
//     splices it onto its base and validates every authored `pr:` step's
//     plugin_input against #PrInput. The host never reads this file from disk.
//
// SELF-CONTAINED (no package clause, no base references): compiles standalone
// (gengotypes + the SDK serve-side check) and splices onto the base.
//
// command:review declares NO input def — its args are pass-through CLI tokens.

// #PrInput — the plugin_input shape for the `pr` verb (read-only check tools).
#PrInput: {
	// repo — owner/repo of the PR; default: the triggering repository.
	repo?: string @go(Repo)
	// pr — PR number (required unless an offline fixture is used).
	pr?: int @go(Pr)
	// method — which read-only tool to run.
	method: "pr_diff" | "pr_commits" | "pr_thread" | "pr_meta" @go(Method)
	// fixture — name of a committed offline fixture (deterministic, no network).
	fixture?: string @go(Fixture)
}
