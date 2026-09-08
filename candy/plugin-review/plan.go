package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// =============================================================================
// runPlan — the runtime orchestration executor (charly review --plan <path>).
//
// review-plan.yml (path from --plan / REVIEW_PLAN_PATH, default <cwd>/review-plan.yml)
// declares an ordered step list + optional runtime plugins. ANY runtime plugin can
// join the review workflow purely through this file; the workflow YAML never changes.
// =============================================================================

type planFile struct {
	Version int          `yaml:"version"`
	Plugins []planPlugin `yaml:"plugins"`
	Steps   []planStep   `yaml:"steps"`
}
type planPlugin struct {
	Source  string `yaml:"source"`  // opencharly/plugin-xyz
	Version string `yaml:"version"` // release tag, e.g. v2026.251.1234
	Asset   string `yaml:"asset"`   // asset base name (default: repo basename)
}
type planStep struct {
	ID              string   `yaml:"id"`
	Kind            string   `yaml:"kind"` // "review" (core engine) | "command" (arbitrary shell/plugin word)
	Command         string   `yaml:"command"`
	Verdict         string   `yaml:"verdict"` // required | none (default: none)
	ContinueOnError bool     `yaml:"continue_on_error"`
	Env             []string `yaml:"env"` // env NAMES to inherit from the process env for this step
}

func runPlan(ctx context.Context, cfg reviewConfig) (int, error) {
	path := cfg.PlanPath
	if path == "" {
		path = "review-plan.yml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 1, fmt.Errorf("review plan %s: %w", path, err)
	}
	plan, err := decodePlan(raw)
	if err != nil {
		return 1, fmt.Errorf("review plan %s: %w", path, err)
	}
	if plan.Version != 1 {
		return 1, fmt.Errorf("review plan: unsupported version %d (want 1)", plan.Version)
	}
	if len(plan.Steps) == 0 {
		return 1, fmt.Errorf("review plan: no steps declared")
	}
	if err := materializePlugins(plan.Plugins); err != nil {
		return 1, err
	}

	var sectionOut []string
	var verdicts []string
	for _, step := range plan.Steps {
		stepOut, stepVerdict, err := runPlanStep(ctx, cfg, step)
		if err != nil {
			if step.ContinueOnError {
				fmt.Printf("[step: %s] FAILED (continue_on_error): %v\n", step.ID, err)
				sectionOut = append(sectionOut, fmt.Sprintf("\n\n### [step: %s] — FAILED (continue_on_error)\n%s", step.ID, stepOut))
				continue
			}
			return 1, fmt.Errorf("[step: %s] %w", step.ID, err)
		}
		fmt.Printf("[step: %s] done (verdict=%s)\n", step.ID, stepVerdict)
		sectionOut = append(sectionOut, fmt.Sprintf("\n\n### [step: %s]\n%s", step.ID, stepOut))
		if step.Verdict == "required" {
			if stepVerdict == "" {
				return 2, fmt.Errorf("[step: %s] verdict: required but no Verdict line produced", step.ID)
			}
			verdicts = append(verdicts, stepVerdict)
		}
	}

	full := strings.Join(sectionOut, "")
	distinct := dedupe(verdicts)
	ok := true
	for _, d := range distinct {
		if d != verdicts[0] {
			ok = false
		}
	}
	writeGHOutputs(full, ok && len(distinct) == 1, distinct)
	if cfg.OutPath != "" {
		_ = os.WriteFile(cfg.OutPath, []byte(full), 0o644)
	}
	fmt.Println("plugin-review: plan done — steps=" + fmt.Sprint(len(plan.Steps)) + " verdicts=" + fmt.Sprint(distinct))
	// ONE comment with the concatenated step output (B6 preserved; best-effort)
	if cfg.PR != 0 && cfg.Repo != "" {
		footer := fmt.Sprintf("\n\n---\n%s/%s — action-review.\n\n[View action run](%s/%s/actions/runs/%s)",
			cfg.Provider, cfg.Model, cfg.ServerURL, cfg.RepoEnv, cfg.RunID)
		if err := newGHClient().postComment(ctx, cfg.owner(), cfg.repo(), cfg.PR, full+footer); err != nil {
			fmt.Println("plugin-review: comment post failed (non-fatal): " + err.Error())
		}
	}
	if !ok {
		return 2, fmt.Errorf("ambiguous verdict across required steps: %v", distinct)
	}
	return 0, nil
}

func runPlanStep(ctx context.Context, cfg reviewConfig, step planStep) (string, string, error) {
	var env []string
	for _, name := range step.Env {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	cmdEnv := append(os.Environ(), env...)
	switch step.Kind {
	case "review":
		subCfg := cfg
		out := ""
		exit, err := runCoreReview(ctx, subCfg)
		if err != nil {
			return out, "", err
		}
		_ = exit
		if cfg.OutPath != "" {
			b, _ := os.ReadFile(cfg.OutPath)
			out = string(b)
		}
		_, distinct, n := extractVerdict(out)
		verdict := ""
		if n == 1 {
			verdict = distinct[0]
		}
		return out, verdict, nil
	case "command":
		cmd := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail", "-c", step.Command)
		cmd.Env = cmdEnv
		raw, err := cmd.CombinedOutput()
		out := string(raw)
		if err != nil {
			return out, "", err
		}
		_, distinct, n := extractVerdict(out)
		verdict := ""
		if n == 1 {
			verdict = distinct[0]
		}
		return out, verdict, nil
	}
	return "", "", fmt.Errorf("unknown step kind %q (want review|command)", step.Kind)
}

// decodePlan: YAML with strict unknown-field rejection — unknown keys fail loudly.
func decodePlan(raw []byte) (planFile, error) {
	var p planFile
	dec := newYAMLDecoder(raw)
	if err := dec.decodeStrict(&p); err != nil {
		return p, err
	}
	for _, s := range p.Steps {
		if s.ID == "" {
			return p, fmt.Errorf("step missing id")
		}
		if s.Kind == "" {
			return p, fmt.Errorf("step %q missing kind", s.ID)
		}
		if s.Verdict == "" {
			s.Verdict = "none"
		}
		if s.Verdict != "required" && s.Verdict != "none" {
			return p, fmt.Errorf("step %q: unsupported verdict %q (want required|none)", s.ID, s.Verdict)
		}
	}
	return p, nil
}

// materializePlugins: download non-welded runtime plugins (release assets:
// <asset>-linux-<arch> + <asset>.providers) into $CHARLY_PLUGIN_DIR so their
// words resolve baked on any runner. Idempotent: skip when already present.
func materializePlugins(plugins []planPlugin) error {
	dir := getenvAny("CHARLY_PLUGIN_DIR")
	if dir == "" || len(plugins) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, p := range plugins {
		if p.Source == "" || p.Version == "" {
			return fmt.Errorf("plugins[]: each entry needs source + version")
		}
		asset := p.Asset
		if asset == "" {
			asset = p.Source
			if i := strings.LastIndex(asset, "/"); i >= 0 {
				asset = asset[i+1:]
			}
		}
		bin := filepath.Join(dir, asset)
		if _, err := os.Stat(bin); err == nil {
			continue // already materialized
		}
		arch := runtime.GOARCH
		if arch == "amd64" {
			arch = "amd64"
		}
		// download via gh release (the plugin-generate-packages asset pattern)
		tmp, err := os.MkdirTemp("", "review-plugin-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		out, err := exec.Command("gh", "release", "download", p.Version, "--repo", p.Source,
			"--pattern", asset+"-linux-"+runtime.GOARCH+"*",
			"--pattern", asset+".providers",
			"--dir", tmp).CombinedOutput()
		if err != nil {
			return fmt.Errorf("materialize plugin %s@%s: %w: %s", p.Source, p.Version, err, truncateStr(string(out), 300))
		}
		entries, _ := os.ReadDir(tmp)
		for _, e := range entries {
			src := filepath.Join(tmp, e.Name())
			dst := filepath.Join(dir, strings.TrimPrefix(e.Name(), asset+"-linux-"+runtime.GOARCH))
			if dst == filepath.Join(dir, e.Name()) {
				dst = filepath.Join(dir, e.Name())
			}
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
		fmt.Printf("plugin-review: materialized %s@%s into %s\n", p.Source, p.Version, dir)
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if strings.Contains(src, "linux-") {
		mode = 0o755
	}
	return os.WriteFile(dst, b, mode)
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func newYAMLDecoder(raw []byte) *yamlDecoder { return &yamlDecoder{raw: raw} }

// minimal strict YAML decoder over yaml.v3 (already an sdk dependency).
type yamlDecoder struct{ raw []byte }

func (d *yamlDecoder) decodeStrict(v any) error {
	return yamlUnmarshalStrict(d.raw, v)
}

// yamlUnmarshalStrict is implemented in yamlutil.go (kept separate so the file
// reads like a spec; see there for the yaml.v3 wiring).
func yamlUnmarshalStrict(raw []byte, v any) error { return yamlUnmarshalStrictImpl(raw, v) }

var _ = json.Marshal // keep encoding/json imported in this file for future use
