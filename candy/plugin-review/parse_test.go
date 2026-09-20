package pluginreview

import "testing"

func TestSplitRepo(t *testing.T) {
	o, n := splitRepo("opencharly/plugin-review")
	if o != "opencharly" || n != "plugin-review" {
		t.Fatalf("splitRepo = %q,%q", o, n)
	}
}
