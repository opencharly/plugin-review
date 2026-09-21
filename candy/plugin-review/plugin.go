// Package pluginreview — the charly REVIEW plugin: the read-only `pr` check
// verbs (the PR facts a bed can probe) and `command:review`, the gate's review
// engine. Usable in BOTH placements with zero authoring change: COMPILED INTO
// charly in-process (registerCompiledPlugin → Invoke) OR served OUT-OF-PROCESS by
// the cmd/serve shim (sdk.Main dual mode).
package pluginreview

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/opencharly/plugin-review/candy/plugin-review/params"
	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
)

//go:embed schema/*.cue
var schemaFS embed.FS

const calver = "2026.263.2100"

func NewProvider() pb.ProviderServer { return &provider{} }

func NewMeta() pb.PluginMetaServer {
	return sdk.NewMeta(calver,
		[]sdk.ProvidedCapability{
			{Class: "command", Word: "review"},
			{Class: "verb", Word: "pr", InputDef: "#PrInput"},
		},
		schemaFS)
}

// CliMain is the OUT-OF-PROCESS CLI-mode entry (sdk.Main dual mode): fork/exec'd
// by charly with the pass-through tokens after `charly review <args>`.
func CliMain(args []string) int {
	cfg, mode, err := parseCommand(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "review: "+err.Error())
		return 2
	}
	switch mode {
	case "self-test":
		fmt.Println("plugin-review self-test: NEW-CLEAN-ENGINE-MARKER (command:review resolves)")
		return 0
	case "self-test-verdict":
		exit, err := runVerdictSelfTest()
		if err != nil {
			fmt.Fprintln(os.Stderr, "review: "+err.Error())
		}
		return exit
	default:
		exit, err := Run(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "review: "+err.Error())
			if exit == 0 {
				exit = 1
			}
		}
		return exit
	}
}

type provider struct{ pb.UnimplementedProviderServer }

func (provider) Invoke(_ context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	if req.GetOp() == sdk.OpRun {
		var in struct {
			Args []string `json:"args"`
		}
		if len(req.GetParamsJson()) > 0 {
			if err := json.Unmarshal(req.GetParamsJson(), &in); err != nil {
				return nil, fmt.Errorf("review: decode args: %w", err)
			}
		}
		exit, err := CliMain(in.Args), error(nil)
		if err != nil {
			return nil, err
		}
		if exit != 0 {
			return nil, fmt.Errorf("review: exit %d", exit)
		}
		return &pb.InvokeReply{}, nil
	}

	// verb:pr — the read-only PR facts a bed probes. These read the SAME canonical
	// client the engine uses (R3); they exist so a check bed can assert PR state
	// without a review.
	var in struct {
		PluginInput params.PrInput `json:"plugin_input"`
	}
	if len(req.GetParamsJson()) > 0 {
		if err := json.Unmarshal(req.GetParamsJson(), &in); err != nil {
			return nil, fmt.Errorf("pr verb: decode plugin_input: %w", err)
		}
	}
	input := in.PluginInput
	repo := input.Repo
	if repo == "" {
		repo = getenvAny("GITHUB_REPOSITORY")
	}
	if !strings.Contains(repo, "/") {
		return nil, fmt.Errorf("pr verb: repo must be owner/repo (got %q)", repo)
	}
	pr := int(input.Pr)
	if pr == 0 {
		pr = prFromEventPath(getenvAny("GITHUB_EVENT_PATH"))
	}
	if pr == 0 {
		return nil, fmt.Errorf("pr verb: a PR number is required")
	}

	ctx := context.Background()
	gh := newGHClient()
	var (
		out any
		err error
	)
	switch input.Method {
	case "pr_meta":
		out, err = gh.meta(ctx, repo, pr)
	case "pr_commits":
		out, err = gh.commits(ctx, repo, pr)
	case "pr_thread":
		out, err = gh.comments(ctx, repo, pr)
	case "pr_files":
		out, err = gh.files(ctx, repo, pr)
	default:
		return nil, fmt.Errorf("pr verb: unknown method %q", input.Method)
	}
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &pb.InvokeReply{ResultJson: raw}, nil
}

func runVerdictSelfTest() (int, error) {
	cases := []struct {
		in   string
		want bool
	}{
		{"## Review — PASS\n\nVerdict: PASS\n", true},
		{"Verdict: BLOCK\n", true},
		{"Verdict:  PASS  \n", true},
		{"no verdict here\n", false},
		{"Verdict: PASS\nVerdict: BLOCK\n", false},
	}
	fail := 0
	for _, c := range cases {
		_, distinct, n := extractVerdict(c.in)
		got := n == 1 && len(distinct) == 1
		if got != c.want {
			fmt.Printf("verdict self-test FAIL: want ok=%v got ok=%v for %q\n", c.want, got, c.in)
			fail++
		}
	}
	if fail > 0 {
		return 1, fmt.Errorf("verdict self-test: %d case(s) failed", fail)
	}
	fmt.Println("verdict self-test: ok (all " + fmt.Sprint(len(cases)) + " cases)")
	return 0, nil
}
