// Package logs writes and reads Moorage's log files: plain text, one line per
// entry, rotated by size, so they can be read with ordinary tools
// (spec/07-observability.md, "Logs").
//
// An app log line is "<RFC 3339 time> <stream> <text>", where the stream is
// O (stdout), E (stderr) or M (a note from Moorage, such as a restart).
package logs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/platform"
)

// Streams of an app log line.
const (
	Stdout  = "O"
	Stderr  = "E"
	Moorage = "M"
)

// TimeFormat is the timestamp at the start of every line: RFC 3339 in UTC
// with a fixed number of fractional digits, so lines sort as text.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// MaxLine is the longest text kept on one line; longer output is split.
const MaxLine = 16 << 10

// Defaults for rotation.
const (
	DefaultMaxSize = 10 << 20
	DefaultKeep    = 5
)

// File is an append-only log file rotated by size: when a write would take it
// past MaxSize, file is renamed file.1 (file.1 becomes file.2, and so on,
// keeping Keep old files) and a new file is started. It is safe for
// concurrent use.
type File struct {
	Path    string
	MaxSize int64
	Keep    int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens path for appending, creating it and its directory if needed.
func Open(path string, maxSize int64, keep int) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	lf := &File{Path: path, MaxSize: maxSize, Keep: keep}
	if err := lf.open(); err != nil {
		return nil, err
	}
	return lf, nil
}

func (lf *File) open() error {
	f, err := os.OpenFile(lf.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	lf.f, lf.size = f, fi.Size()
	return nil
}

// Write appends p, rotating first if needed.
func (lf *File) Write(p []byte) (int, error) {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.f == nil {
		return 0, os.ErrClosed
	}
	if lf.MaxSize > 0 && lf.size > 0 && lf.size+int64(len(p)) > lf.MaxSize {
		if err := lf.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := lf.f.Write(p)
	lf.size += int64(n)
	return n, err
}

func (lf *File) rotate() error {
	if err := lf.f.Close(); err != nil {
		return err
	}
	lf.f = nil
	for i := lf.Keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", lf.Path, i), fmt.Sprintf("%s.%d", lf.Path, i+1))
	}
	// If it cannot be moved aside, which another program holding it open can bring about on Windows, it goes on being
	// written to, as what an app says matters more than the size of a file. The next write tries again.
	var err error
	if lf.Keep > 0 {
		err = os.Rename(lf.Path, lf.Path+".1")
	} else {
		err = os.Remove(lf.Path)
	}
	if opened := lf.open(); opened != nil {
		return opened
	}
	if err != nil && !errors.Is(err, fs.ErrPermission) && !isBusy(err) {
		return err
	}
	return nil
}

// isBusy says that a file could not be moved for someone having it open, as Windows says it.
func isBusy(err error) bool {
	var link *os.LinkError
	return errors.As(err, &link) && strings.Contains(strings.ToLower(link.Err.Error()), "used by another process")
}

// Close closes the file.
func (lf *File) Close() error {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.f == nil {
		return nil
	}
	err := lf.f.Close()
	lf.f = nil
	return err
}

// Line is one parsed log line.
type Line struct {
	Time   time.Time
	Stream string
	Text   string
}

// Format renders a line as it is stored.
func Format(t time.Time, stream, text string) string {
	return t.UTC().Format(TimeFormat) + " " + stream + " " + text + "\n"
}

// Parse reads a stored line. Lines that do not follow the format are
// returned whole as text, with a zero time.
func Parse(s string) Line {
	s = strings.TrimRight(s, "\r\n")
	ts, rest, ok := strings.Cut(s, " ")
	if ok {
		if t, err := time.Parse(TimeFormat, ts); err == nil {
			stream, text, _ := strings.Cut(rest, " ")
			return Line{Time: t, Stream: stream, Text: text}
		}
	}
	return Line{Text: s}
}

// Writer returns an io.Writer that turns what is written into lines of the
// given stream, splitting at newlines and at MaxLine, and appends them to dst.
// Call Flush when the source is done, to write an unterminated last line.
type Writer struct {
	dst    io.Writer
	stream string
	now    func() time.Time
	mu     sync.Mutex
	buf    []byte
	redact *strings.Replacer
}

// NewWriter returns a Writer for stream.
func NewWriter(dst io.Writer, stream string) *Writer {
	return &Writer{dst: dst, stream: stream, now: time.Now}
}

// MinRedacted is the length below which Redact leaves a value alone: shorter
// values would mangle ordinary output.
const MinRedacted = 6

// Redact makes the writer replace each of values with [secret] wherever it
// appears in a line (best effort: a value split across MaxLine is missed).
func (w *Writer) Redact(values ...string) *Writer {
	var pairs []string
	for _, v := range values {
		if len(v) >= MinRedacted {
			pairs = append(pairs, v, "[secret]")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.redact = nil
	if len(pairs) > 0 {
		w.redact = strings.NewReplacer(pairs...)
	}
	return w
}

func (w *Writer) line(s string) string {
	if w.redact != nil {
		s = w.redact.Replace(s)
	}
	return Format(w.now(), w.stream, s)
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 && len(w.buf) < MaxLine {
			return len(p), nil
		}
		end, next := i, i+1
		if i < 0 || i > MaxLine {
			end, next = MaxLine, MaxLine
		}
		line := strings.TrimSuffix(string(w.buf[:end]), "\r")
		w.buf = w.buf[next:]
		if _, err := io.WriteString(w.dst, w.line(line)); err != nil {
			return len(p), err
		}
	}
}

// Flush writes any unterminated last line.
func (w *Writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		io.WriteString(w.dst, w.line(strings.TrimSuffix(string(w.buf), "\r")))
		w.buf = nil
	}
}

// Tail returns the last n lines of the files of a log (path.N oldest first,
// then path), in order.
func Tail(path string, n int) ([]Line, error) {
	files := Files(path)
	var lines []Line
	for i := len(files) - 1; i >= 0 && len(lines) < n; i-- {
		got, err := readLines(files[i])
		if err != nil {
			return nil, err
		}
		lines = append(got, lines...)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// Files lists a log's files, oldest first: path.N … path.1, path.
func Files(path string) []string {
	matches, _ := filepath.Glob(path + ".*")
	var rotated []string
	for _, m := range matches {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(m, path+"."), "%d", &n); err == nil {
			rotated = append(rotated, m)
		}
	}
	sort.Slice(rotated, func(i, j int) bool {
		var a, b int
		fmt.Sscanf(strings.TrimPrefix(rotated[i], path+"."), "%d", &a)
		fmt.Sscanf(strings.TrimPrefix(rotated[j], path+"."), "%d", &b)
		return a > b
	})
	if _, err := os.Stat(path); err == nil {
		rotated = append(rotated, path)
	}
	return rotated
}

// ReadAll reads one file of a log.
func ReadAll(path string) ([]Line, error) { return readLines(path) }

func readLines(path string) ([]Line, error) {
	f, err := platform.OpenForReading(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4*MaxLine)
	for sc.Scan() {
		out = append(out, Parse(sc.Text()))
	}
	return out, sc.Err()
}

// Follower reads lines appended to a log file after a position, and keeps up with rotation: what was added to a file
// before it was moved aside is read from where it went.
type Follower struct {
	Path    string
	offset  int64
	partial string
	info    os.FileInfo
	// older lists files of the log to read all of first.
	older []string
}

// NewFollower starts following path from its current end.
func NewFollower(path string) *Follower {
	f := &Follower{Path: path}
	if fi, err := os.Stat(path); err == nil {
		f.offset = fi.Size()
	}
	return f
}

func (f *Follower) take(data []byte) []Line {
	text := f.partial + string(data)
	parts := strings.Split(text, "\n")
	f.partial = parts[len(parts)-1]
	var out []Line
	for _, p := range parts[:len(parts)-1] {
		out = append(out, Parse(p))
	}
	return out
}

// restOf reads what a file that was moved aside has past a position, if it is the one that was being read.
func (f *Follower) restOf(path string, offset int64) []byte {
	file, err := platform.OpenForReading(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	if fi, err := file.Stat(); err != nil || (f.info != nil && !os.SameFile(f.info, fi)) || fi.Size() <= offset {
		return nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil
	}
	return data
}

// Read returns the complete lines appended since the last call.
func (f *Follower) Read() ([]Line, error) {
	var out []Line
	for len(f.older) > 0 {
		data := f.restOf(f.older[0], 0)
		f.older = f.older[1:]
		f.partial = ""
		out = append(out, f.take(data)...)
		f.partial = ""
	}
	file, err := platform.OpenForReading(f.Path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		return out, err
	}
	if (f.info != nil && !os.SameFile(f.info, fi)) || fi.Size() < f.offset {
		// Rotated: what came after the last look is in the file that it went to.
		if rest := f.restOf(f.Path+".1", f.offset); len(rest) > 0 {
			out = append(out, f.take(rest)...)
		}
		f.offset, f.partial = 0, ""
	}
	f.info = fi
	if fi.Size() == f.offset {
		return out, nil
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return out, err
	}
	data, err := io.ReadAll(io.LimitReader(file, fi.Size()-f.offset))
	if err != nil {
		return out, err
	}
	f.offset += int64(len(data))
	return append(out, f.take(data)...), nil
}

// FollowSince follows path from its beginning, and starts with those of its older files that have been written to since a
// moment ago, as a log can rotate with no one following it. Lines from before that moment are for the reader to skip.
func FollowSince(path string, since time.Time) *Follower {
	f := &Follower{Path: path}
	for _, file := range Files(path) {
		if file == path {
			continue
		}
		if fi, err := os.Stat(file); err == nil && fi.ModTime().After(since) {
			f.older = append(f.older, file)
		}
	}
	return f
}

// FollowFromStart follows path from its beginning.
func FollowFromStart(path string) *Follower {
	return &Follower{Path: path}
}
