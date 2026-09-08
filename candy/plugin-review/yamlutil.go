package pluginreview

import (
	"os"

	"gopkg.in/yaml.v3"
)

var osStderr = os.Stderr

// yamlUnmarshalStrictImpl: strict YAML decode (unknown fields are errors — a plan
// with a typo'd key fails loudly instead of being silently ignored).
func yamlUnmarshalStrictImpl(raw []byte, v any) error {
	dec := yaml.NewDecoder(stringsReader(string(raw)))
	dec.KnownFields(true)
	return dec.Decode(v)
}
