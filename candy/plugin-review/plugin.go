// Package pluginreview — the importable form of the charly REVIEW plugin: the
// read-only verb:pr check tools + the command:review engine (a 1:1 port of the
// retired opencharly/pi-review-action index.js) + the --plan runtime orchestration
// executor. Usable in BOTH placements with zero authoring change: COMPILED INTO
// charly in-process (registerCompiledPlugin → Invoke) OR served OUT-OF-PROCESS by
// the cmd/serve shim (sdk.Main dual-mode; command dispatch = fork/exec CLI mode →
// CliMain, verb dispatch = go-plugin gRPC Invoke).
package pluginreview

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/opencharly/plugin-review/candy/plugin-review/params"
	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
)

//go:embed schema/*.cue
var schemaFS embed.FS

const calver = "2026.251.0000"

// NewProvider returns the provider for in-proc registration or out-of-proc serving.
func NewProvider() pb.ProviderServer { return &provider{} }

// NewMeta advertises command:review (no InputDef — pass-through CLI tokens) and
// verb:pr (typed plugin_input #PrInput, validated over the served schema).
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
	exit, err := RunReviewFromArgs(args)
	if err != nil {
		fmt.Fprintln(osStderr, "review: "+err.Error())
		if exit == 0 {
			exit = 1
		}
		return exit
	}
	return exit
}

type provider struct{ pb.UnimplementedProviderServer }

// Invoke dispatches: OpRun → command:review (in-proc compiled-in path); any other
// op (verb dispatch from a check step / bed) → verb:pr with the typed plugin_input.
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
		exit, err := RunReviewFromArgs(in.Args)
		if err != nil {
			return nil, err
		}
		if exit != 0 {
			return nil, fmt.Errorf("review: exit %d", exit)
		}
		return &pb.InvokeReply{}, nil
	}

	// verb:pr dispatch — decode the authored plugin_input into the CUE-GENERATED
	// typed struct (never a hand-parsed map).
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
	pr := input.Pr
	if pr == 0 {
		if v := getenvAny("PR_NUMBER"); v != "" {
			n, _ := parseInt(v)
			pr = int64(n)
		}
	}

	tools := toolSet{gh: newGHClient(), repo: repo, pr: int(pr), fixture: input.Fixture}
	if o, r := splitRepo(repo); o != "" {
		tools.owner, tools.repo = o, r
	}
	method := ""
	switch input.Method {
	case "pr_diff":
		method = "get_pr_diff"
	case "pr_commits":
		method = "get_pr_commits"
	case "pr_thread":
		method = "get_pr_thread"
	case "pr_meta":
		method = "get_pr_meta"
	default:
		return nil, fmt.Errorf("pr verb: unknown method %q", input.Method)
	}
	result, err := tools.call(context.Background(), method)
	if err != nil {
		return nil, err
	}
	return &pb.InvokeReply{ResultJson: []byte(result)}, nil
}

func splitRepo(repo string) (owner, name string) {
	if i := indexByte(repo, '/'); i >= 0 {
		return repo[:i], repo[i+1:]
	}
	return "", ""
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
