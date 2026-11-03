// Package apitypes defines the JSON documents of Moorage's REST API, shared
// by the server and the CLI.
package apitypes

import "time"

// App is an app and its production environment.
type App struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
	Production *Release  `json:"production,omitempty"`
}

// Release is one numbered release of an environment.
type Release struct {
	Version   int       `json:"version"`
	Build     string    `json:"build"`
	Reason    string    `json:"reason"`
	Files     int       `json:"files"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	Current   bool      `json:"current"`
}

// Deployment is the result of a deploy or rollback: the release now serving.
type Deployment struct {
	App        string  `json:"app"`
	Env        string  `json:"env"`
	URL        string  `json:"url"`
	AppCreated bool    `json:"app_created,omitempty"`
	Release    Release `json:"release"`
}

// RollbackRequest asks for an earlier release to be published again; with
// Version 0, the release before the current one.
type RollbackRequest struct {
	Version int `json:"version,omitempty"`
}

// Problem is an RFC 9457 error document.
type Problem struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
}

// Content types.
const (
	ProblemType = "application/problem+json"
	// StaticPart and ArchivePart are the multipart form fields of a static
	// deploy: the [static] rules as JSON, then the gzip-compressed tar.
	StaticPart  = "static"
	ArchivePart = "archive"
)
