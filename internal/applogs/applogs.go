// Package applogs reads what an app has to say, for the API and the console alike: the
// output of its instances, its builds, its access log and its events, merged by time,
// searched and followed (spec/07-observability.md, "Reading").
package applogs

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// The sources of an app's logs.
const (
	Stdout  = "stdout"  // what its instances write to standard output
	Stderr  = "stderr"  // and to standard error
	Moorage = "moorage" // notes from Moorage: starts, exits, restarts
	Build   = "build"   // the logs of its deployments
	Access  = "access"  // the requests it served
	Events  = "events"  // what happened to it
)

// AllSources lists the sources, in the order they are offered.
var AllSources = []string{Stdout, Stderr, Moorage, Build, Access, Events}

// DefaultSources are what an app's logs are, unless asked otherwise.
var DefaultSources = []string{Stdout, Stderr, Moorage}

// MaxLines bounds how many lines one read returns.
const MaxLines = 200000

// Query says which lines to read.
type Query struct {
	// Process keeps one process, and Instance one instance: its number or its ID.
	Process, Instance string
	// Sources lists the sources to read; none means DefaultSources.
	Sources []string
	// Grep keeps the lines that have this text, whatever its case.
	Grep string
	// Problems keeps the lines that report one: errors and warnings, standard error,
	// and requests that failed.
	Problems bool
	// Since and Until bound the lines by time, and After is an exclusive Since: where a
	// follower that reconnects got to.
	Since, Until, After time.Time
	// Tail is how many of the newest lines to read.
	Tail int
}

func (q Query) has(source string) bool {
	if len(q.Sources) == 0 {
		return slices.Contains(DefaultSources, source)
	}
	return slices.Contains(q.Sources, source)
}

// Entry is one line.
type Entry struct {
	Time     time.Time
	Source   string
	Process  string
	Index    int
	Instance string
	Text     string
	// Level is what the line reports: "error", "warn", "info" or "debug", if it says.
	Level string
	// Message is the line as a person reads it, for one that is structured.
	Message string

	stream string
}

// Who says where a line came from.
func (e Entry) Who() string {
	switch e.Source {
	case Build, Access, Events:
		return e.Source
	}
	return e.Process + "." + strconv.Itoa(e.Index)
}

// Stream is the line's stream as a log file has it.
func (e Entry) Stream() string {
	switch {
	case e.stream != "":
		return e.stream
	case e.Source == Events:
		return logs.Moorage
	}
	return logs.Stdout
}

// Display is the text to show.
func (e Entry) Display() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Text
}

// Problem reports whether the line reports a problem.
func (e Entry) Problem() bool {
	return e.Level == "error" || e.Level == "warn" || (e.Level == "" && e.Source == Stderr)
}

// keeps reports whether a line that has been read is wanted.
func (q Query) keeps(e Entry) bool {
	switch {
	case !q.Since.IsZero() && e.Time.Before(q.Since):
		return false
	case !q.Until.IsZero() && e.Time.After(q.Until):
		return false
	case !q.After.IsZero() && !e.Time.After(q.After):
		return false
	case q.Problems && !e.Problem():
		return false
	case q.Grep != "" && !containsFold(e.Text, q.Grep):
		return false
	}
	return true
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

var (
	levelWord = regexp.MustCompile(`(?i)(?:^|[\s\[(])(error|err|fatal|panic|critical|warn|warning)(?:[\s\]):]|$)`)
	levelPair = regexp.MustCompile(`(?i)\b(?:level|lvl|severity)=["']?([a-z]+)`)
)

// levelOf reduces what programs call their levels to four.
func levelOf(name string) string {
	switch strings.ToLower(name) {
	case "error", "err", "fatal", "panic", "critical", "crit", "alert", "emerg", "emergency", "severe":
		return "error"
	case "warn", "warning":
		return "warn"
	case "info", "notice", "information":
		return "info"
	case "debug", "trace", "verbose":
		return "debug"
	}
	return ""
}

// describe works out what a line of an app's output reports. A line that is a JSON object
// says so in its fields; one of plain text may name a level, as loggers do.
func describe(text string) (level, message string) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var fields map[string]any
		if json.Unmarshal([]byte(trimmed), &fields) == nil {
			return describeFields(fields)
		}
	}
	if m := levelPair.FindStringSubmatch(text); m != nil {
		if level = levelOf(m[1]); level != "" {
			return level, ""
		}
	}
	// Only near the start of a line: further on, a word is more likely to be about
	// something ("no errors").
	head := text
	if len(head) > 48 {
		head = head[:48]
	}
	if m := levelWord.FindStringSubmatch(head); m != nil {
		return levelOf(m[1]), ""
	}
	return "", ""
}

func describeFields(fields map[string]any) (level, message string) {
	take := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := fields[key].(string); ok {
				delete(fields, key)
				return v
			}
		}
		return ""
	}
	level = levelOf(take("level", "lvl", "severity"))
	message = take("msg", "message", "event")
	for _, key := range []string{"time", "ts", "timestamp", "@timestamp"} {
		delete(fields, key)
	}
	if message == "" {
		return level, ""
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(message)
	for _, key := range keys {
		value, err := json.Marshal(fields[key])
		if err != nil {
			continue
		}
		b.WriteString("  " + key + "=" + strings.Trim(string(value), `"`))
	}
	return level, b.String()
}

// logFile is a file of lines, and what they are.
type logFile struct {
	path     string
	source   string // Build or Access, or "" for an instance's
	process  string
	index    int
	instance string
}

func (f logFile) entry(l logs.Line) Entry {
	e := Entry{Time: l.Time, Text: l.Text, Process: f.process, Index: f.index, Instance: f.instance, stream: l.Stream}
	switch f.source {
	case Build:
		e.Source = Build
		if l.Stream != logs.Moorage {
			e.Level, e.Message = describe(l.Text)
		}
	case Access:
		return accessEntry(l)
	default:
		switch l.Stream {
		case logs.Stderr:
			e.Source = Stderr
		case logs.Moorage:
			e.Source = Moorage
		default:
			e.Source = Stdout
		}
		if e.Source != Moorage {
			e.Level, e.Message = describe(l.Text)
		}
	}
	return e
}

// wanted reports whether a query reads this file's lines at all.
func (f logFile) wanted(q Query) bool {
	switch f.source {
	case Build, Access:
		return q.has(f.source)
	}
	return q.has(Stdout) || q.has(Stderr) || q.has(Moorage)
}

// keepsSource reports whether a query wants a line of this source.
func (q Query) keepsSource(e Entry) bool { return q.has(e.Source) }

// accessEntry turns a line of the access log, a JSON object, into something to read.
func accessEntry(l logs.Line) Entry {
	var a struct {
		Time     time.Time `json:"time"`
		Method   string    `json:"method"`
		Path     string    `json:"path"`
		Status   int       `json:"status"`
		Bytes    int64     `json:"bytes"`
		Millis   float64   `json:"ms"`
		Client   string    `json:"client"`
		Instance string    `json:"instance"`
	}
	e := Entry{Source: Access, Text: l.Text, Time: l.Time}
	if err := json.Unmarshal([]byte(l.Text), &a); err != nil {
		return e
	}
	e.Time, e.Instance = a.Time, a.Instance
	e.Message = fmt.Sprintf("%s %s %d  %.1f ms  %d B  %s", a.Method, a.Path, a.Status, a.Millis, a.Bytes, a.Client)
	switch {
	case a.Status >= 500:
		e.Level = "error"
	case a.Status >= 400:
		e.Level = "warn"
	}
	return e
}

// Reader reads an app's logs.
type Reader struct {
	DB      *store.DB
	DataDir string
}

// files lists the log files a query reads.
func (r Reader) files(app, env string, q Query) []logFile {
	var out []logFile
	if q.has(Stdout) || q.has(Stderr) || q.has(Moorage) {
		out = append(out, instanceFiles(reconcile.LogDir(r.DataDir, app, env), q)...)
	}
	if q.has(Build) {
		// These are kept together for an app, and belong to the environment they were for.
		mine, err := r.DB.LogIDs(context.Background(), app, env)
		if err == nil {
			matches, _ := filepath.Glob(deploy.LogPath(r.DataDir, app, "*"))
			for _, path := range matches {
				if mine[strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))] {
					out = append(out, logFile{path: path, source: Build, process: "build"})
				}
			}
		}
	}
	if q.has(Access) {
		path := filepath.Join(r.DataDir, "logs", "router", app, env+".log")
		if _, err := os.Stat(path); err == nil {
			out = append(out, logFile{path: path, source: Access, process: "access"})
		}
	}
	return out
}

// instanceFiles lists an environment's log files (<process>.<index>.<instance>.log).
func instanceFiles(dir string, q Query) []logFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []logFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(name, ".log"), ".", 3)
		if len(parts) != 3 || (q.Process != "" && parts[0] != q.Process) {
			continue
		}
		if q.Instance != "" && q.Instance != parts[1] && q.Instance != parts[2] {
			continue
		}
		index, _ := strconv.Atoi(parts[1])
		out = append(out, logFile{path: filepath.Join(dir, name), process: parts[0], index: index, instance: parts[2]})
	}
	return out
}

// Instances lists the instances an environment has logs of, as process.index, in order.
func (r Reader) Instances(app, env string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range instanceFiles(reconcile.LogDir(r.DataDir, app, env), Query{}) {
		name := f.process + "." + strconv.Itoa(f.index)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// read reads a log file's wanted lines, up to q.Tail of the newest, and the time of the
// newest line it has, wanted or not.
func (f logFile) read(q Query) ([]Entry, time.Time) {
	var (
		out    []Entry
		newest time.Time
	)
	for _, path := range logs.Files(f.path) {
		// Nothing older than the range: a rotated file ends when it was last written.
		if info, err := os.Stat(path); err == nil && path != f.path && !q.Since.IsZero() && info.ModTime().Before(q.Since) {
			continue
		}
		lines, err := logs.ReadAll(path)
		if err != nil {
			continue
		}
		for _, l := range lines {
			e := f.entry(l)
			if e.Time.After(newest) {
				newest = e.Time
			}
			if q.keepsSource(e) && q.keeps(e) {
				out = append(out, e)
			}
		}
		if q.Tail > 0 && len(out) > q.Tail {
			out = out[len(out)-q.Tail:]
		}
	}
	return out, newest
}

func newestLast(a, b Entry) int {
	if c := a.Time.Compare(b.Time); c != 0 {
		return c
	}
	return cmp.Compare(a.Instance, b.Instance)
}

// Read returns the newest lines a query asks for, oldest first, and whether there are
// older ones it left out.
func (r Reader) Read(ctx context.Context, app, env string, q Query) ([]Entry, bool, error) {
	entries, _, err := r.read(ctx, app, env, q, r.files(app, env, q))
	if err != nil {
		return nil, false, err
	}
	more := false
	if q.Tail > 0 && len(entries) > q.Tail {
		entries, more = entries[len(entries)-q.Tail:], true
	}
	return entries, more, nil
}

func (r Reader) read(ctx context.Context, app, env string, q Query, files []logFile) ([]Entry, map[string]time.Time, error) {
	if q.Tail > 0 {
		q.Tail++ // to tell whether there is more
	}
	newest := map[string]time.Time{}
	var all []Entry
	for _, f := range files {
		got, last := f.read(q)
		newest[f.path] = last
		all = append(all, got...)
	}
	if q.has(Events) {
		got, err := r.events(ctx, app, env, q, "")
		if err != nil {
			return nil, nil, err
		}
		all = append(all, got...)
	}
	slices.SortStableFunc(all, newestLast)
	return all, newest, nil
}

// events reads what happened to the app, after an event if given.
func (r Reader) events(ctx context.Context, app, env string, q Query, after string) ([]Entry, error) {
	limit := MaxLines
	if q.Tail > 0 {
		limit = q.Tail
	}
	list, err := r.DB.Events(ctx, store.EventFilter{App: app, Env: env, After: after, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(list))
	for _, e := range list {
		entry := eventEntry(e)
		if q.keeps(entry) {
			out = append(out, entry)
		}
	}
	return out, nil
}

func eventEntry(e store.Event) Entry {
	entry := Entry{Time: e.At, Source: Events, Process: "events", Instance: e.ID, Text: events.Summary(events.WithoutVariables(e))}
	switch events.Level(e.Type) {
	case "fail":
		entry.Level = "error"
	case "warn":
		entry.Level = "warn"
	}
	return entry
}

// Follow passes a query's newest lines to fn, then new lines as they are written, until
// ctx ends or fn fails. Instances that start, and deployments, are followed as they appear.
func (r Reader) Follow(ctx context.Context, app, env string, q Query, fn func(Entry) error) error {
	files := r.files(app, env, q)
	// Follow first: a line written while the rest are read is then seen twice, not
	// never, and its time tells which.
	followers := map[string]*logs.Follower{}
	known := map[string]logFile{}
	for _, f := range files {
		followers[f.path], known[f.path] = logs.NewFollower(f.path), f
	}
	history, seen, err := r.read(ctx, app, env, q, files)
	if err != nil {
		return err
	}
	if q.Tail > 0 && len(history) > q.Tail {
		history = history[len(history)-q.Tail:]
	}
	for _, e := range history {
		if err := fn(e); err != nil {
			return err
		}
	}
	lastEvent := ""
	if q.has(Events) {
		list, err := r.DB.Events(ctx, store.EventFilter{App: app, Env: env, Limit: 1})
		if err != nil {
			return err
		}
		if len(list) > 0 {
			lastEvent = list[0].ID
		}
	}
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		for _, f := range r.files(app, env, q) {
			if _, ok := followers[f.path]; !ok {
				followers[f.path], known[f.path] = logs.FollowFromStart(f.path), f
			}
		}
		var fresh []Entry
		for path, follower := range followers {
			lines, err := follower.Read()
			if err != nil {
				continue
			}
			for _, l := range lines {
				e := known[path].entry(l)
				if !e.Time.After(seen[path]) {
					continue
				}
				if q.keepsSource(e) && q.keeps(e) {
					fresh = append(fresh, e)
				}
			}
		}
		if q.has(Events) {
			list, err := r.DB.Events(ctx, store.EventFilter{App: app, Env: env, After: lastEvent, Limit: 100})
			if lastEvent == "" {
				slices.Reverse(list) // with nothing to go after, the store lists newest first
			}
			if err == nil && len(list) > 0 {
				lastEvent = list[len(list)-1].ID
				for _, e := range list {
					if entry := eventEntry(e); q.keeps(entry) {
						fresh = append(fresh, entry)
					}
				}
			}
		}
		slices.SortStableFunc(fresh, newestLast)
		for _, e := range fresh {
			if err := fn(e); err != nil {
				return err
			}
		}
	}
}

// ParseTime reads a time as people give one: how long ago ("15m", "2h", "7d"), or a moment
// (RFC 3339, or a date and time in the given zone).
func ParseTime(s string, now time.Time, zone *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(s, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && days >= 0 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, zone); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: give how long ago (15m, 2h, 7d) or a moment (2026-11-03T15:04:05Z)", s)
}

// SplitSources reads a list of sources, such as "stderr,build".
func SplitSources(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if !slices.Contains(AllSources, name) {
			return nil, fmt.Errorf("%q is not a source of logs: there are %s", name, strings.Join(AllSources, ", "))
		}
		out = append(out, name)
	}
	return out, nil
}
