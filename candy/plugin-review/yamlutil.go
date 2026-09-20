package pluginreview

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var osStderr = os.Stderr

// yamlUnmarshalStrict: strict YAML decode (unknown fields are errors — a plan
// with a typo'd key fails loudly instead of being silently ignored).
func yamlUnmarshalStrict(raw []byte, v any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	return dec.Decode(v)
}
