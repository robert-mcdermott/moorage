package manifest

// Plan is the resolved description of how a build is served, stored with
// the build. This release knows one kind: "static", a directory served as is.
type Plan struct {
	Kind   string  `json:"kind"`
	Static *Static `json:"static,omitempty"`
}
