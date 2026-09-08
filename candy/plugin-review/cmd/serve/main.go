// Command serve is the OUT-OF-PROCESS entrypoint: dual-mode sdk.Main (serve OR
// CLI). charly fork/execs this binary in CLI mode for command:review dispatch
// (→ CliMain); the serve half is the go-plugin gRPC transport for verb:pr.
package main

import (
	pluginreview "github.com/opencharly/plugin-review/candy/plugin-review"
	"github.com/opencharly/sdk"
)

func main() {
	sdk.Main(pluginreview.NewProvider(), pluginreview.NewMeta(), pluginreview.CliMain)
}
