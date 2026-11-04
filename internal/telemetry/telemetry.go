// Package telemetry sends what apps say and do to somewhere else as well: syslog, an OpenTelemetry collector or any HTTP
// endpoint. What is kept here stays, and is what counts: a sink that is down or slow costs it its own place in the queue,
// and nothing else.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const (
	// queueSize is the most lines that one sink can be behind by, past which the oldest go.
	queueSize = 20000
	// batchSize is the most lines sent at once.
	batchSize = 500
	// maxBackoff is the longest it waits before trying a sink that is down again.
	maxBackoff = time.Minute
	// sendTimeout is as long as one batch may take.
	sendTimeout = 20 * time.Second
	// rejectedAgain is how long it waits to send less to a sink that said no.
	rejectedAgain = 100 * time.Millisecond
)

// InvalidError says what is wrong with a sink.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Source is a log file that something is writing to.
type Source struct {
	App, Env, Process, Instance string
	Path                        string
}

// Keys keeps what a sink has to keep secret.
type Keys interface {
	SealChannel(id string, value []byte) []byte
	OpenChannel(id string, sealed []byte) ([]byte, error)
}

// Service sends what is going on to the sinks.
type Service struct {
	DB   *store.DB
	Keys Keys
	Log  *slog.Logger
	// Client and Dial reach what this server may reach on someone's say.
	Client *http.Client
	Dial   func(ctx context.Context, network, address string) (net.Conn, error)
	// Sources lists what is writing logs now.
	Sources func(ctx context.Context) ([]Source, error)
	// Metrics says what the server is doing now.
	Metrics func(ctx context.Context) metrics.Export
	// Server is what this machine calls itself, and Version what is running.
	Server, Version string
	// Tick is how often log files are looked at, and MetricsEvery how often metrics are sent.
	Tick, MetricsEvery time.Duration
	// Life ends it all.
	Life context.Context

	started   time.Time
	mu        sync.Mutex
	sinks     map[string]*sink
	followers map[string]*follower
	cursor    time.Time
	saved     time.Time
	// fresh says that nothing has been read yet, so that what is in the files from before can be skipped.
	fresh bool
	// loaded says that the sinks have been read before, so that this is not the server starting.
	loaded bool
	going  sync.WaitGroup
	reload chan struct{}
	onLogs bool
}

type follower struct {
	source Source
	f      *logs.Follower
	skip   time.Time
	// gone is when it was last seen to be written to, if it no longer is. It is kept for a while after, as a look that
	// did not list it does not prove that it has gone, and starting again from the top would send it all again.
	gone time.Time
}

// followerStays is how long a log that has no one writing to it goes on being followed.
const followerStays = 10 * time.Minute

// sink is one sink, as it goes along.
type sink struct {
	// row is for the service, under its lock. What serves it has what it needs of that here, which does not change while it
	// goes on: when it would, another takes its place.
	row           store.TelemetrySink
	id, name      string
	logs, metrics bool
	sender        sender
	stop          context.CancelFunc
	done          chan struct{}
	// limit is how many lines to send at once, which goes down if a sink says that they are too much.
	limit int

	mu      sync.Mutex
	queue   []Line
	dropped int64
}

func (k *sink) add(lines []Line) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.queue = append(k.queue, lines...)
	if over := len(k.queue) - queueSize; over > 0 {
		k.dropped += int64(over)
		k.queue = append(k.queue[:0:0], k.queue[over:]...)
	}
}

func (k *sink) take() []Line {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := min(k.limit, len(k.queue))
	return k.queue[:n:n]
}

func (k *sink) waiting() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.queue) > 0
}

// takeDropped says how many lines have gone, since it was last asked.
func (k *sink) takeDropped() int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	dropped := k.dropped
	k.dropped = 0
	return dropped
}

func (k *sink) sent(n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.queue = k.queue[n:]
}

// giveUp lets go of a line that a sink will not take.
func (k *sink) giveUp(n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.queue = k.queue[n:]
	k.dropped += int64(n)
}

var (
	sinkName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	headerName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// Form is what someone asks for.
type Form struct {
	Name, Kind, Address string
	Headers             map[string]string
	Logs, Metrics       bool
	AppID               string
}

// Check says what is wrong with a sink, if anything.
func (f Form) Check() error {
	if !sinkName.MatchString(f.Name) {
		return &InvalidError{"A sink's name is letters and numbers, with dots, dashes or underscores."}
	}
	if !f.Logs && !f.Metrics {
		return &InvalidError{"A sink is for logs, for metrics, or for both."}
	}
	switch f.Kind {
	case KindOTLP, KindHTTP:
		u, err := url.Parse(f.Address)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return &InvalidError{"An address for this kind of sink starts with https://."}
		}
		if u.User != nil {
			return &InvalidError{"Leave what signs in out of the address: a header is where it can be kept sealed."}
		}
		if len(f.Headers) > 0 && !notify.SafeForSecrets(f.Address) {
			return &InvalidError{"Headers are not sent in the clear across the internet: use its https address."}
		}
		if f.Kind == KindHTTP && f.Metrics {
			return &InvalidError{"Metrics go to an OTLP collector, or a scrape of /metrics: an HTTP sink is for logs."}
		}
	case KindSyslog:
		if _, _, _, err := syslogAddress(f.Address); err != nil {
			return err
		}
		if f.Metrics {
			return &InvalidError{"Syslog is for logs. Metrics go to an OTLP collector, or a scrape of /metrics."}
		}
		if len(f.Headers) > 0 {
			return &InvalidError{"Syslog has no headers."}
		}
	default:
		return &InvalidError{fmt.Sprintf("%q is not a kind of sink: syslog, otlp or http.", f.Kind)}
	}
	for name, value := range f.Headers {
		if !headerName.MatchString(name) || strings.ContainsAny(value, "\r\n") {
			return &InvalidError{"A header is a name of letters, numbers and dashes, and a value on one line."}
		}
	}
	return nil
}

// syslogAddress reads udp://host:514, tcp://host:601 or tls://host:6514.
func syslogAddress(address string) (network, hostport string, useTLS bool, err error) {
	scheme, rest, found := strings.Cut(address, "://")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", "", false, &InvalidError{"A syslog address is udp://host:514, tcp://host:601 or tls://host:6514."}
	}
	if _, _, err := net.SplitHostPort(rest); err != nil {
		return "", "", false, &InvalidError{"A syslog address says its port, as in tls://host:6514."}
	}
	switch scheme {
	case "udp", "tcp":
		return scheme, rest, false, nil
	case "tls":
		return "tcp", rest, true, nil
	}
	return "", "", false, &InvalidError{"A syslog address is udp://host:514, tcp://host:601 or tls://host:6514."}
}

// Set keeps a sink, by its name. Saving it again without headers keeps those it had, so long as it is going to the same
// address: they are not for sending anywhere else.
func (s *Service) Set(ctx context.Context, f Form, by string) (store.TelemetrySink, error) {
	if err := f.Check(); err != nil {
		return store.TelemetrySink{}, err
	}
	row := store.TelemetrySink{ID: ids.New(ids.Sink), Name: f.Name, Kind: f.Kind, Address: strings.TrimRight(f.Address, "/"), Logs: f.Logs, Metrics: f.Metrics, AppID: f.AppID, CreatedBy: by, CreatedAt: time.Now().UTC()}
	old, err := s.DB.TelemetrySink(ctx, f.Name)
	switch {
	case err == nil:
		row.ID = old.ID
		if f.Headers == nil && old.Address == row.Address {
			row.SealedHeaders = old.SealedHeaders
		}
	case !errors.Is(err, store.ErrNotFound):
		return row, err
	}
	if f.Headers != nil {
		encoded, err := json.Marshal(f.Headers)
		if err != nil {
			return row, err
		}
		row.SealedHeaders = s.Keys.SealChannel(row.ID, encoded)
	}
	if err := s.DB.SetTelemetrySink(ctx, row); err != nil {
		return row, err
	}
	s.Changed()
	return s.DB.TelemetrySink(ctx, f.Name)
}

// Remove lets go of a sink.
func (s *Service) Remove(ctx context.Context, name string) error {
	if err := s.DB.DeleteTelemetrySink(ctx, name); err != nil {
		return err
	}
	s.Changed()
	return nil
}

// Changed says that the sinks are not what they were.
func (s *Service) Changed() {
	s.mu.Lock()
	reload := s.reload
	s.mu.Unlock()
	if reload != nil {
		select {
		case reload <- struct{}{}:
		default:
		}
	}
}

// Headers opens what a sink sends with what it sends.
func (s *Service) Headers(row store.TelemetrySink) (map[string]string, error) {
	headers := map[string]string{}
	if len(row.SealedHeaders) > 0 {
		plain, err := s.Keys.OpenChannel(row.ID, row.SealedHeaders)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(plain, &headers); err != nil {
			return nil, err
		}
	}
	return headers, nil
}

func (s *Service) senderOf(row store.TelemetrySink) (sender, error) {
	headers, err := s.Headers(row)
	if err != nil {
		return nil, err
	}
	switch row.Kind {
	case KindHTTP:
		return &httpSender{address: row.Address, headers: headers, client: s.Client, server: s.Server}, nil
	case KindOTLP:
		return &otlpSender{base: row.Address, headers: headers, client: s.Client, server: s.Server, version: s.Version}, nil
	case KindSyslog:
		network, address, useTLS, err := syslogAddress(row.Address)
		if err != nil {
			return nil, err
		}
		return &syslogSender{network: network, address: address, useTLS: useTLS, dial: s.Dial, server: s.Server}, nil
	}
	return nil, fmt.Errorf("a sink of the kind %q", row.Kind)
}

// Test sends a line to a sink now, and says what happened.
func (s *Service) Test(ctx context.Context, name, by string) error {
	row, err := s.DB.TelemetrySink(ctx, name)
	if err != nil {
		return err
	}
	send, err := s.senderOf(row)
	if err != nil {
		return err
	}
	defer send.close()
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	now := time.Now().UTC()
	if row.Logs {
		err = send.sendLogs(ctx, []Line{{Time: now, App: "moorage", Env: "test", Process: "test", Instance: "test", Stream: "O", Text: "A test from Moorage, sent by " + by}})
	} else {
		err = send.sendMetrics(ctx, metrics.Export{Gauges: []metrics.ExportValue{{Name: "moorage.test", Unit: "1", Description: "A test from Moorage.", Labels: map[string]string{}, Value: 1}}}, now, now)
	}
	problem, reached := "", now
	if err != nil {
		problem, reached = err.Error(), time.Time{}
	}
	if saved := s.DB.NoteTelemetrySink(context.WithoutCancel(ctx), row.ID, reached, problem, 0); saved != nil {
		s.Log.Warn("note how a sink went", "sink", name, "err", saved)
	}
	return err
}

// Run goes on sending until Life ends.
func (s *Service) Run() {
	s.mu.Lock()
	s.reload = make(chan struct{}, 1)
	s.sinks, s.followers = map[string]*sink{}, map[string]*follower{}
	s.started = time.Now().UTC()
	s.mu.Unlock()
	if at, err := s.DB.TelemetryCursor(s.Life); err == nil {
		s.cursor = at
	}
	tick := s.Tick
	if tick == 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	refresh := time.NewTicker(30 * time.Second)
	defer refresh.Stop()
	s.load()
	for {
		select {
		case <-s.Life.Done():
			s.finish()
			return
		case <-s.reload:
			s.load()
		case <-refresh.C:
			s.load()
		case <-t.C:
			s.readLogs()
		}
	}
}

// Wait waits for the sinks to finish what they were at.
func (s *Service) Wait() {
	done := make(chan struct{})
	go func() {
		s.going.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (s *Service) finish() {
	s.mu.Lock()
	for _, k := range s.sinks {
		k.stop()
	}
	s.mu.Unlock()
	if !s.cursor.IsZero() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.DB.SetTelemetryCursor(ctx, s.cursor); err != nil {
			s.Log.Warn("note how far logs have been read", "err", err)
		}
	}
}

// load brings the sinks into line with what is on record.
func (s *Service) load() {
	rows, err := s.DB.TelemetrySinks(s.Life)
	if err != nil {
		if s.Life.Err() == nil {
			s.Log.Warn("read the telemetry sinks", "err", err)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := map[string]bool{}
	logging := false
	for _, row := range rows {
		wanted[row.ID] = true
		logging = logging || row.Logs
		have := s.sinks[row.ID]
		if have != nil && have.row.Address == row.Address && have.row.Kind == row.Kind && have.row.Logs == row.Logs && have.row.Metrics == row.Metrics && have.row.AppID == row.AppID && string(have.row.SealedHeaders) == string(row.SealedHeaders) {
			have.row = row
			continue
		}
		if have != nil {
			have.stop()
		}
		send, err := s.senderOf(row)
		if err != nil {
			s.Log.Warn("set up a telemetry sink", "sink", row.Name, "err", err)
			delete(s.sinks, row.ID)
			continue
		}
		ctx, stop := context.WithCancel(s.Life)
		k := &sink{row: row, id: row.ID, name: row.Name, logs: row.Logs, metrics: row.Metrics, sender: send, stop: stop, done: make(chan struct{}), limit: batchSize}
		s.sinks[row.ID] = k
		s.going.Add(1)
		go func() {
			defer s.going.Done()
			s.serve(ctx, k)
		}()
	}
	for id, k := range s.sinks {
		if !wanted[id] {
			k.stop()
			delete(s.sinks, id)
		}
	}
	if logging && !s.onLogs {
		s.fresh = true
		if s.loaded {
			// Nothing was looking while there was nowhere to send it, and that is not to be sent now. As the server
			// starts it is another matter: what its apps said while it was away is what it can catch up on.
			s.followers = map[string]*follower{}
			s.cursor = time.Now().UTC()
		}
	}
	s.onLogs, s.loaded = logging, true
}

// readLogs passes on what has been written since the last time.
func (s *Service) readLogs() {
	s.mu.Lock()
	logging := s.onLogs
	s.mu.Unlock()
	if !logging {
		return
	}
	sources, err := s.Sources(s.Life)
	if err != nil {
		if s.Life.Err() == nil {
			s.Log.Warn("list the logs to send", "err", err)
		}
		return
	}
	now := time.Now().UTC()
	present := make(map[string]bool, len(sources))
	for _, source := range sources {
		present[source.Path] = true
		if s.followers[source.Path] != nil {
			continue
		}
		f := &follower{source: source, f: logs.FollowFromStart(source.Path)}
		if s.fresh {
			f.skip = s.cursor
			if f.skip.IsZero() {
				f.skip = now
			}
			// It may have rotated while no one was looking.
			f.f = logs.FollowSince(source.Path, f.skip)
		}
		s.followers[source.Path] = f
	}
	s.fresh = false
	newest := s.cursor
	for path, f := range s.followers {
		switch {
		case present[path]:
			f.gone = time.Time{}
		case f.gone.IsZero():
			f.gone = now
		}
		lines, err := f.f.Read()
		if err != nil {
			s.Log.Warn("read a log to send", "path", path, "err", err)
			continue
		}
		out := make([]Line, 0, len(lines))
		for _, l := range lines {
			if !f.skip.IsZero() && !l.Time.After(f.skip) {
				continue
			}
			out = append(out, Line{Time: l.Time, App: f.source.App, Env: f.source.Env, Process: f.source.Process, Instance: f.source.Instance, Stream: l.Stream, Text: l.Text})
			if l.Time.After(newest) {
				newest = l.Time
			}
		}
		if len(out) > 0 {
			s.mu.Lock()
			for _, k := range s.sinks {
				if k.row.Logs && (k.row.App == "" || k.row.App == f.source.App) {
					k.add(out)
				}
			}
			s.mu.Unlock()
		}
		if !f.gone.IsZero() && len(lines) == 0 && now.Sub(f.gone) > followerStays {
			delete(s.followers, path)
		}
	}
	s.cursor = newest
	if time.Since(s.saved) > 10*time.Second && !s.cursor.IsZero() {
		s.saved = time.Now()
		if err := s.DB.SetTelemetryCursor(s.Life, s.cursor); err != nil && s.Life.Err() == nil {
			s.Log.Warn("note how far logs have been read", "err", err)
		}
	}
}

// serve sends what comes for one sink until it is told to stop, waiting longer each time that it fails. With more waiting,
// it goes straight on, or a sink could not take more than one batch each time around.
func (s *Service) serve(ctx context.Context, k *sink) {
	defer k.sender.close()
	every := s.MetricsEvery
	if every == 0 {
		every = time.Minute
	}
	nextMetrics := time.Now().Add(min(every, 5*time.Second))
	backoff := time.Duration(0)
	problem := ""
	var sentSince time.Time
	var lastNote time.Time
	note := func(failure string) {
		dropped := k.takeDropped()
		now := time.Now().UTC()
		if failure == problem && dropped == 0 && (sentSince.IsZero() || now.Sub(lastNote) < 10*time.Second) {
			return
		}
		saving, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.DB.NoteTelemetrySink(saving, k.id, sentSince, failure, dropped); err != nil {
			s.Log.Warn("note how a sink went", "sink", k.name, "err", err)
		}
		problem, lastNote, sentSince = failure, now, time.Time{}
	}
	for {
		wait := 500 * time.Millisecond
		switch {
		case backoff > 0:
			wait = backoff
		case k.logs && k.waiting():
			wait = 0
		}
		select {
		case <-ctx.Done():
			note(problem)
			return
		case <-time.After(wait):
		}
		failure := ""
		tried := false
		now := time.Now().UTC()
		if k.logs {
			if batch := k.take(); len(batch) > 0 {
				tried = true
				sending, cancel := context.WithTimeout(ctx, sendTimeout)
				err := k.sender.sendLogs(sending, slices.Clone(batch))
				cancel()
				var rejected *RejectedError
				switch {
				case err == nil:
					k.sent(len(batch))
					sentSince = now
				case errors.As(err, &rejected) && len(batch) > 1:
					// It may be too much at once, so it gets less, and soon: that was an answer, and not a sink that is down.
					k.limit = max(1, len(batch)/2)
					note(err.Error())
					backoff = 0
					select {
					case <-ctx.Done():
						return
					case <-time.After(rejectedAgain):
					}
					continue
				case errors.As(err, &rejected):
					// Down to the line that it will not have, which is not to stop what comes after it. It goes on to the
					// next line in its own time, as a sink that refuses everything would otherwise be sent everything, and
					// none of it would be left for when someone has put that right.
					k.giveUp(1)
					failure = err.Error()
				default:
					failure = err.Error()
				}
				if err == nil && k.limit < batchSize {
					k.limit = min(batchSize, k.limit*2)
				}
			}
		}
		// What it thinks of logs has nothing to do with metrics, which may be going somewhere else behind the same address.
		if k.metrics && now.After(nextMetrics) {
			tried = true
			nextMetrics = now.Add(every)
			sending, cancel := context.WithTimeout(ctx, sendTimeout)
			err := k.sender.sendMetrics(sending, s.Metrics(ctx), s.started, now)
			cancel()
			if err == nil {
				sentSince = now
			} else if failure == "" {
				failure = err.Error()
			}
		}
		if ctx.Err() != nil {
			return
		}
		switch {
		case failure != "":
			if backoff == 0 {
				backoff = time.Second
			} else if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			note(failure)
		case tried:
			backoff = 0
			note("")
		}
	}
}
