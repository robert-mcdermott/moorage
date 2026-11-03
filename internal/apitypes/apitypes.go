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

// Deployment is the job that produces a release: a deploy, a rollback. A
// finished deployment that went live carries the release now serving.
type Deployment struct {
	ID         string    `json:"id"`
	App        string    `json:"app"`
	Env        string    `json:"env"`
	URL        string    `json:"url"`
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	Build      string    `json:"build,omitempty"`
	AppCreated bool      `json:"app_created,omitempty"`
	Release    *Release  `json:"release,omitempty"`
	Error      string    `json:"error,omitempty"`
	Output     []string  `json:"output,omitempty"`
	CreatedBy  string    `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Deployment statuses: queued, building, releasing and rolling_out while it
// runs; live, failed, superseded or canceled once it has finished.
const (
	StatusLive   = "live"
	StatusFailed = "failed"
)

// Finished reports whether the deployment has ended.
func (d Deployment) Finished() bool {
	switch d.Status {
	case "live", "failed", "superseded", "canceled":
		return true
	}
	return false
}

// RollbackRequest asks for an earlier release to be published again; with
// Version 0, the release before the current one.
type RollbackRequest struct {
	Version int `json:"version,omitempty"`
}

// Problem is an RFC 9457 error document. Output, when present, holds the
// last lines of a failing instance's log.
type Problem struct {
	Type   string   `json:"type,omitempty"`
	Title  string   `json:"title"`
	Status int      `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Code   string   `json:"code,omitempty"`
	Output []string `json:"output,omitempty"`
}

// Instance is one running copy of a process.
type Instance struct {
	ID        string    `json:"id"`
	Process   string    `json:"process"`
	Index     int       `json:"index"`
	Release   int       `json:"release"`
	Port      int       `json:"port,omitempty"`
	Status    string    `json:"status"`
	Healthy   bool      `json:"healthy"`
	PID       int       `json:"pid,omitempty"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at,omitempty"`
	LastExit  *Exit     `json:"last_exit,omitempty"`
}

// Exit is how an instance's app last ended.
type Exit struct {
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
}

// LogLine is one line of an app's log. Stream is "O" (stdout), "E"
// (stderr) or "M" (a note from Moorage).
type LogLine struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
	Process  string    `json:"process"`
	Index    int       `json:"index"`
	Stream   string    `json:"stream"`
	Text     string    `json:"text"`
}

// Content types and form fields.
const (
	ProblemType = "application/problem+json"
	NDJSONType  = "application/x-ndjson"
	// PlanPart and ArchivePart are the multipart form fields of the deploy
	// of an upload that needs no build: the plan (manifest.Plan) as JSON,
	// then the gzip-compressed tar. SourcePart alone is source for the
	// server to build.
	PlanPart    = "plan"
	ArchivePart = "archive"
	SourcePart  = "source"
)
