package manifest

import _ "embed"

//go:embed schema.json
var schema []byte

// Schema returns the JSON Schema of moorage.toml, for editors.
func Schema() []byte { return schema }
