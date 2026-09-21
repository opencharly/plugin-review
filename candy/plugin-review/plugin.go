// Package pluginreview — the charly REVIEW plugin: the read-only `pr` check
// verbs (the PR facts a bed can probe) and `command:review`, the gate's review
// engine. Usable in BOTH placements with zero authoring change: COMPILED INTO
// charly in-process (registerCompiledPlugin → Invoke) OR served OUT-OF-PROCESS by
// the cmd/serve shim (sdk.Main dual mode).
package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
)

const calver = "2026.263.2100"

func NewProvider() pb.ProviderServer { return &provider{} }

func NewMeta() pb.PluginMetaServer {
	// Input-less: command:review's args are pass-through CLI tokens, so there is
	// no typed plugin_input and NO CUE schema — the SDK's documented "input-less
	// plugin passes a nil schemaFS" path.
	return sdk.NewMeta(calver,
		[]sdk.ProvidedCapability{
			{Class: "command", Word: "review"},
		},
		nil)
}

// CliMain is the OUT-OF-PROCESS CLI-mode entry (sdk.Main dual mode): fork/exec'd
// by charly with the pass-through tokens after `charly review <args>`.
// run is the ONE command core: parse the args, dispatch the mode, and return the
// exit code AND the error (never stderr-only). Both the out-of-process CLI entry
// (CliMain) and the in-proc Invoke path use it, so they cannot diverge.
func run(args []string) (int, error) {
	cfg, mode, err := parseCommand(args)
	if err != nil {
		return 2, err
	}
	switch mode {
	case "self-test":
		fmt.Println("plugin-review self-test: ok (command:review resolves)")
		return 0, nil
	case "self-test-verdict":
		return runVerdictSelfTest()
	default:
		return Run(context.Background(), cfg)
	}
}

// CliMain is the OUT-OF-PROCESS CLI entry (fork/exec'd by charly). It reports the
// error on stderr and returns the exit code — the only place stderr is used.
func CliMain(args []string) int {
	exit, err := run(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "review: "+err.Error())
	}
	return exit
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
		exit, err := run(in.Args)
		if err != nil {
			return nil, err
		}
		if exit != 0 {
			return nil, fmt.Errorf("review: exit %d", exit)
		}
		return &pb.InvokeReply{}, nil
	}

	return nil, fmt.Errorf("review: unknown op %v", req.GetOp())
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
