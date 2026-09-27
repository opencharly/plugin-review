// plugin-review's OWN self-contained CUE schema — the SINGLE SOURCE for this
// plugin's declaration surface, used two ways exactly like every other plugin's
// schema (there is no schema-less plugin):
//
//  1. GENERATE the Go params — `cue exp gengotypes` → ../params/cue_types_gen.go.
//  2. SERVE over Describe — the host splices `base ++ plugin` at the load gate
//     (registerPluginUnitSchema), so the plugin's declarations travel WITH it and
//     a self-contained schema that will not splice is a LOUD load failure.
//
// The `command:review` capability's authored input is its pass-through CLI grammar
// (the OpRun `{args: [...]}` envelope), so this schema DOCUMENTS the command
// contract and the plugin's configuration surface — the AI_REVIEW_* vars declared
// in charly.yml and consumed by config.go (FromEnv). SELF-CONTAINED: it references
// no base def, so it compiles STANDALONE (the property `cue exp gengotypes` needs
// and the property that lets the SDK compile it serve-side).
#ReviewPlugin: {
	// The command word the plugin serves.
	command: "review"

	// What the command does, in one line (the public-docs surface).
	contract: string & !=""

	// The configuration surface: the env var names the engine reads. Declared here
	// so the charly.yml `env_accept:` list and this schema cannot silently drift.
	config?: [string]: string
}
