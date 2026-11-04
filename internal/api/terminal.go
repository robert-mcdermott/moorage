package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/oneoff"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const (
	// terminalPing is how often a terminal's connection is shown to be alive to whatever is in the way.
	terminalPing = 30 * time.Second
	// terminalWrite is as long as someone may leave what they have been sent unread.
	terminalWrite = 5 * time.Minute
)

var terminalKind = regexp.MustCompile(`^[a-zA-Z0-9._+-]{1,64}$`)

func terminalDimension(text string, otherwise int) uint16 {
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > 1000 {
		return uint16(otherwise)
	}
	return uint16(n)
}

// terminal runs a command with someone at the other end of it: binary messages are what is typed and what it shows, and
// text messages say anything else. Whoever is at the other end goes, and the command goes with them.
//
// A browser sends its cookies with a WebSocket to anywhere, from any page, and could be made to run things: so this takes a
// token, which a page elsewhere does not have.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	p := who(r)
	if p.SessionID != "" {
		problem(w, http.StatusForbidden, "token_only", "A terminal takes an API token, and not a session in a browser: moor run -it has one.")
		return
	}
	q := r.URL.Query()
	var limit time.Duration
	if text := q.Get("timeout"); text != "" {
		var d manifest.Duration
		if err := d.UnmarshalText([]byte(text)); err != nil {
			problem(w, http.StatusUnprocessableEntity, "invalid_timeout", sentenceOf(err))
			return
		}
		limit = d.Std()
	}
	kind := q.Get("term")
	if !terminalKind.MatchString(kind) {
		kind = "xterm-256color"
	}
	// A browser's fetch of this address would otherwise start a command, and find out only then that it is not a WebSocket.
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		problem(w, http.StatusUpgradeRequired, "websocket_only", "This is a WebSocket.")
		return
	}
	typedFrom, typed := io.Pipe()
	defer typedFrom.Close()
	shownFrom, shown := io.Pipe()
	resize := make(chan platform.TerminalSize, 1)
	session := &oneoff.Session{TTY: q.Get("tty") == "1", Size: platform.TerminalSize{Rows: terminalDimension(q.Get("rows"), 24), Cols: terminalDimension(q.Get("cols"), 80)},
		Term: kind, In: typedFrom, Out: shown, Resize: resize}
	instance, trigger := q.Get("instance"), "terminal"
	if instance != "" {
		trigger = "exec"
	}
	run, err := s.deployer.StartRun(lasting(r), deploy.RunRequest{App: app, Env: env, By: p.Name, Command: q.Get("command"), Timeout: limit, Trigger: trigger, Session: session, Instance: instance})
	if s.runFailed(w, err) {
		return
	}
	// Opening one is a GET, which is not otherwise on record as doing anything.
	s.auth.Audit(r.Context(), store.AuditEntry{Actor: p.Name, IP: s.auth.ClientIP(r), Action: "run." + trigger, Target: app + "/" + env, Result: "ok", Data: map[string]any{"run": run.ID, "instance": instance}})
	stopRun := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := s.deployer.StopRun(ctx, app, run.ID); err != nil {
			s.log.Warn("stop a run that no one is at", "run", run.ID, "err", err)
		}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		stopRun()
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Everything shown goes to whoever is there. Once they have gone, it goes nowhere, and the command is not held up. They
	// may be reading slowly, through a pager, and they get a while; anything could be happening at the other end of one
	// that stays stopped, and it is holding on to a command.
	var sending atomic.Bool
	shownAll := make(chan struct{})
	go func() {
		defer close(shownAll)
		buf := make([]byte, 32<<10)
		for {
			n, err := shownFrom.Read(buf)
			if n > 0 {
				sending.Store(true)
				writing, stopWriting := context.WithTimeout(ctx, terminalWrite)
				werr := conn.Write(writing, websocket.MessageBinary, buf[:n])
				stopWriting()
				sending.Store(false)
				if werr != nil {
					cancel()
					io.Copy(io.Discard, shownFrom)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// What is typed waits its turn apart from reading the connection, which is how someone leaving is noticed, as a command
	// need not be reading what is typed at it. There is a limit: past that it stops reading, which is what slows down
	// whatever is sending a large file.
	typing := make(chan []byte, 64)
	var endTyping sync.Once
	stopTyping := func() { endTyping.Do(func() { close(typing) }) }
	go func() {
		defer typed.Close()
		for data := range typing {
			if _, err := typed.Write(data); err != nil {
				for range typing { // none of it is wanted now, and whoever is sending it should not stop for that
				}
				return
			}
		}
	}()
	go func() {
		defer cancel()
		defer stopTyping()
		over := false
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				if over {
					continue
				}
				select {
				case typing <- data:
				case <-ctx.Done():
					return
				}
				continue
			}
			var said apitypes.TerminalMessage
			if json.Unmarshal(data, &said) != nil {
				continue
			}
			if said.EOF {
				over = true
				stopTyping()
			}
			if said.Rows > 0 && said.Cols > 0 {
				size := platform.TerminalSize{Rows: terminalDimension(strconv.Itoa(said.Rows), 24), Cols: terminalDimension(strconv.Itoa(said.Cols), 80)}
				// Only the last one matters, and nothing need be listening.
				select {
				case <-resize:
				default:
				}
				resize <- size
			}
		}
	}()
	// Something written shows whether the connection is still there, without needing anyone to answer: while it is not
	// being read, for as long as what is typed waits, nothing else would. It is also what keeps it open past a proxy.
	go func() {
		t := time.NewTicker(terminalPing)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if sending.Load() {
					continue
				}
				writing, stopWriting := context.WithTimeout(ctx, terminalWrite)
				err := conn.Write(writing, websocket.MessageText, []byte("{}"))
				stopWriting()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	// It runs until it ends, or no one is there, or they may no longer do this.
	follow := make(chan struct{})
	go func() {
		defer close(follow)
		s.waitForRun(context.WithoutCancel(r.Context()), app, run.ID)
	}()
	send := func(m apitypes.TerminalMessage) {
		data, err := json.Marshal(m)
		if err != nil {
			return
		}
		writing, stopWriting := context.WithTimeout(ctx, terminalWrite)
		defer stopWriting()
		conn.Write(writing, websocket.MessageText, data)
	}
	send(apitypes.TerminalMessage{Run: run.ID})
	select {
	case <-follow:
	case <-ctx.Done():
		stopRun()
		<-follow
	}
	shown.Close()
	<-shownAll
	final, err := s.db.Run(context.WithoutCancel(r.Context()), app, run.ID)
	switch {
	case err != nil:
		stopRun()
		send(apitypes.TerminalMessage{Ended: true, Error: sentenceOf(err)})
	case !final.Finished():
		stopRun()
		send(apitypes.TerminalMessage{Ended: true, Error: "It could not be followed, so it has been stopped."})
	default:
		doc := runDoc(final)
		send(apitypes.TerminalMessage{Ended: true, ExitCode: doc.ExitCode, Error: doc.Error})
	}
	if ctx.Err() == nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

func (s *Server) waitForRun(ctx context.Context, app, id string) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		run, err := s.db.Run(ctx, app, id)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("read a run", "run", id, "err", err)
			return
		}
		if err == nil && run.Finished() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
