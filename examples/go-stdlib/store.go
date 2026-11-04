package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

// store keeps the links in a file of JSON lines that only grows: a line
// when a link is added, a line when it is visited. Appending a line is safe
// while two instances write at once, as the old and the new release do for
// a moment during a deploy, and each instance reads the lines the others
// appended before it answers.
type store struct {
	path string

	mu     sync.Mutex
	offset int64 // how much of the file has been read
	links  map[string]*link
}

type link struct {
	Code    string
	URL     string
	Added   time.Time
	Visits  int
	visited time.Time
}

type event struct {
	Kind string    `json:"kind"` // "add" or "visit"
	Code string    `json:"code"`
	URL  string    `json:"url,omitempty"`
	Time time.Time `json:"time"`
}

var (
	errInvalid = errors.New("that is not a link: give a full address, such as https://example.com/page")
	errBadCode = errors.New("a short name is up to 32 letters, digits, - and _")
	errTaken   = errors.New("that short name is taken: choose another")
	codeChars  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	reserved   = []string{"healthz", "links", "static"}
)

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &store{path: path, links: map[string]*link{}}
	return s, s.refresh()
}

// Add adds a link under code, or under a random code when code is empty.
func (s *store) Add(target, code string) (link, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return link{}, errInvalid
	}
	if code != "" && !codeChars.MatchString(code) {
		return link{}, errBadCode
	}
	if slices.Contains(reserved, code) {
		return link{}, errTaken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return link{}, err
	}
	if code == "" {
		code = s.newCode()
	} else if _, ok := s.links[code]; ok {
		return link{}, errTaken
	}
	if err := s.append(event{Kind: "add", Code: code, URL: u.String(), Time: time.Now().UTC()}); err != nil {
		return link{}, err
	}
	// Another instance may have taken the code at the same moment: the
	// first line in the file wins.
	if l := s.links[code]; l.URL != u.String() {
		return link{}, errTaken
	}
	return *s.links[code], nil
}

// Visit counts a visit to code and returns where it leads.
func (s *store) Visit(code string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return "", false, err
	}
	l, ok := s.links[code]
	if !ok {
		return "", false, nil
	}
	return l.URL, true, s.append(event{Kind: "visit", Code: code, Time: time.Now().UTC()})
}

// List returns the links, the most recently visited or added first.
func (s *store) List() ([]link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.refresh()
	out := make([]link, 0, len(s.links))
	for _, l := range s.links {
		out = append(out, *l)
	}
	last := func(l link) time.Time { return later(l.Added, l.visited) }
	slices.SortFunc(out, func(a, b link) int { return last(b).Compare(last(a)) })
	return out, err
}

// append writes one event as one line, then reads it back with whatever
// other instances wrote meanwhile.
func (s *store) append(e event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return s.refresh()
}

// refresh reads the lines appended since the last read.
func (s *store) refresh() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return nil // a line still being written is read next time
		} else if err != nil {
			return err
		}
		s.offset += int64(len(line))
		var e event
		if err := json.Unmarshal(bytes.TrimSpace(line), &e); err != nil {
			continue
		}
		s.apply(e)
	}
}

func (s *store) apply(e event) {
	switch l := s.links[e.Code]; {
	case e.Kind == "add" && l == nil:
		s.links[e.Code] = &link{Code: e.Code, URL: e.URL, Added: e.Time}
	case e.Kind == "visit" && l != nil:
		l.Visits++
		l.visited = later(l.visited, e.Time)
	}
}

func (s *store) newCode() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no l, o, 0 or 1
	for {
		b := make([]byte, 6)
		rand.Read(b)
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		if _, taken := s.links[string(b)]; !taken {
			return string(b)
		}
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
