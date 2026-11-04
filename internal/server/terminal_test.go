package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// watchedTerminal keeps what a terminal shows, to wait for.
type watchedTerminal struct {
	t    *testing.T
	term *client.Terminal
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func openTerminal(t *testing.T, c *client.Client, app string, o client.TerminalOptions) *watchedTerminal {
	t.Helper()
	term, err := c.Terminal(context.Background(), app, "production", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close() })
	w := &watchedTerminal{t: t, term: term, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			w.mu.Lock()
			w.out.Write(buf[:n])
			w.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return w
}

func (w *watchedTerminal) shown() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.String()
}

func (w *watchedTerminal) waitFor(want string) {
	w.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(w.shown(), want) {
		if time.Now().After(deadline) {
			w.t.Fatalf("it never showed %q, only %q", want, w.shown())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *watchedTerminal) type_(text string) {
	w.t.Helper()
	if _, err := io.WriteString(w.term, text); err != nil {
		w.t.Fatal(err)
	}
}

func (w *watchedTerminal) wait() {
	w.t.Helper()
	select {
	case <-w.done:
	case <-time.After(30 * time.Second):
		w.t.Fatalf("it never ended, having shown %q", w.shown())
	}
}

func TestTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this uses sh: TestTerminalOnWindows is the same thing for there")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "API_KEY", Value: "a-secret-value", Secret: true}}}); err != nil {
		t.Fatal(err)
	}

	// A terminal of its own, of the size asked for, with what a command has.
	w := openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 33, Cols: 101, Term: "xterm-test", Command: `sh -c 'echo size $(stty size); [ -t 0 ] && echo it-is-a-terminal; echo kind $TERM app $MOORAGE_APP key $API_KEY; read line; echo got:$line; exit 7'`})
	w.waitFor("size 33 101")
	w.waitFor("it-is-a-terminal")
	w.waitFor("kind xterm-test app shop key a-secret-value")
	w.type_("hello\r")
	w.waitFor("got:hello")
	w.wait()
	code, problem, over := w.term.Ended()
	if !over || code == nil || *code != 7 {
		t.Errorf("%v %q %v", code, problem, over)
	}

	// It is on record, and what was said in it is not.
	if w.term.Run == "" {
		t.Fatal("it does not say which run it is")
	}
	run, err := c.Run(ctx, "shop", w.term.Run)
	if err != nil || run.Trigger != "terminal" || run.CreatedBy == "" || run.Status != "failed" {
		t.Errorf("%+v %v", run, err)
	}
	_, said, _ := followedRun(t, c, "shop", w.term.Run)
	if strings.Contains(said, "a-secret-value") || strings.Contains(said, "got:hello") || !strings.Contains(said, "not kept") {
		t.Errorf("what is kept: %q", said)
	}

	// It hears of being another size.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `sh -c 'stty size; echo first; read a; stty size; echo second; read b'`})
	w.waitFor("24 80")
	w.waitFor("first")
	if err := w.term.Resize(50, 150); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	w.type_("\r")
	w.waitFor("50 150")
	w.waitFor("second")
	w.type_("\r")
	w.wait()
	if code, _, _ := w.term.Ended(); code == nil || *code != 0 {
		t.Errorf("%v", code)
	}

	// Ctrl-C is for what is running there, as it is in any terminal.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `sh -c 'echo going; sleep 100'`})
	w.waitFor("going")
	w.type_("\x03")
	w.wait()

	// Without one, it has pipes, and standard input ends when it is told.
	w = openTerminal(t, c, "shop", client.TerminalOptions{Command: `sh -c '[ -t 0 ] || echo no-terminal; cat; echo the-end'`})
	w.waitFor("no-terminal")
	w.type_("one line\n")
	w.waitFor("one line")
	if err := w.term.CloseInput(); err != nil {
		t.Fatal(err)
	}
	w.waitFor("the-end")
	w.wait()
	if code, _, _ := w.term.Ended(); code == nil || *code != 0 {
		t.Errorf("%v", code)
	}

	// Going away stops it, and everything it started.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `sh -c 'echo $$; sleep 1000 & wait'`})
	w.waitFor("\n")
	runs, err := c.Runs(ctx, "shop", "production", "")
	if err != nil {
		t.Fatal(err)
	}
	var going string
	for _, r := range runs {
		if r.Status == "running" {
			going = r.ID
		}
	}
	if going == "" {
		t.Fatalf("%+v", runs)
	}
	w.term.Close()
	waitUntil(t, "it to stop", func() bool {
		r, err := c.Run(ctx, "shop", going)
		return err == nil && r.Status == store.RunCanceled
	})

	// That goes for what a shell has put in a process group of its own, as it does with each job.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `sh -c 'set -m; sleep 1000 & echo background-is-$!; echo going; wait'`})
	w.waitFor("going")
	found := regexp.MustCompile(`background-is-(\d+)`).FindStringSubmatch(w.shown())
	if found == nil {
		t.Fatalf("%q", w.shown())
	}
	background, _ := strconv.Atoi(found[1])
	if !processAlive(background) {
		t.Fatalf("it is not running: %d", background)
	}
	w.term.Close()
	waitUntil(t, "what it put in the background to go", func() bool { return !processAlive(background) })

	// So it does if whoever led the session has gone already, which is what the end of a shell looks like.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `sh -c 'set -m; sleep 1000 & echo background-is-$!; echo going; exit 0'`})
	w.waitFor("going")
	found = regexp.MustCompile(`background-is-(\d+)`).FindStringSubmatch(w.shown())
	if found == nil {
		t.Fatalf("%q", w.shown())
	}
	background, _ = strconv.Atoi(found[1])
	w.wait()
	waitUntil(t, "what it left behind to go", func() bool { return !processAlive(background) })

	// Without a terminal, a command that has ended has ended, whether anyone has finished typing or not.
	w = openTerminal(t, c, "shop", client.TerminalOptions{Command: `sh -c 'echo done-already; exit 3'`})
	w.waitFor("done-already")
	w.wait()
	if code, _, _ := w.term.Ended(); code == nil || *code != 3 {
		t.Errorf("%v", code)
	}

	// And it takes as long as it may, with someone there or not, though they have typed nothing.
	w = openTerminal(t, c, "shop", client.TerminalOptions{Timeout: "1s", Command: `sleep 100`})
	w.wait()
	if _, problem, over := w.term.Ended(); !over || !strings.Contains(problem, "out of time") {
		t.Errorf("%q %v", problem, over)
	}

	// All that it says gets to someone who is reading slowly, however long that takes after it has ended.
	slow, err := c.Terminal(ctx, "shop", "production", client.TerminalOptions{Command: `sh -c 'i=0; while [ $i -lt 2000 ]; do echo line-$i-of-output-that-goes-on-for-a-while-so-as-to-fill-the-pipes; i=$((i+1)); done'`})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	time.Sleep(3500 * time.Millisecond)
	shown, err := io.ReadAll(slow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shown), "line-1999-") {
		t.Errorf("it lost what it said: %d bytes", len(shown))
	}

	// Someone who has gone is noticed though the command is reading nothing of what they typed, and more than a pipe holds.
	w = openTerminal(t, c, "shop", client.TerminalOptions{Command: `sh -c 'echo going; sleep 1000'`})
	w.waitFor("going")
	runs, err = c.Runs(ctx, "shop", "production", "")
	if err != nil {
		t.Fatal(err)
	}
	going = ""
	for _, r := range runs {
		if r.Status == "running" {
			going = r.ID
		}
	}
	if going == "" {
		t.Fatalf("%+v", runs)
	}
	lots := strings.Repeat("x", 256<<10)
	w.type_(lots)
	w.term.Resize(30, 90)
	w.term.Resize(31, 91)
	w.term.Close()
	waitUntil(t, "it to notice", func() bool {
		r, err := c.Run(ctx, "shop", going)
		return err == nil && r.Status == store.RunCanceled
	})

	// It takes as long as it may.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Timeout: "1s", Command: `sleep 100`})
	w.wait()
	if _, problem, over := w.term.Ended(); !over || !strings.Contains(problem, "out of time") {
		t.Errorf("%q %v", problem, over)
	}
}

func TestTerminalIsForThoseWhoMay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("what this runs uses sh")
	}
	cfg := testConfig(t)
	addrs := startWith(t, cfg, func(o *Options) { o.GateRecheck = 50 * time.Millisecond })
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()

	if _, err := c.Terminal(ctx, "nothing", "production", client.TerminalOptions{Command: "true"}); status(err) != http.StatusConflict {
		t.Errorf("an app that is not there: %v", err)
	}
	if _, err := c.CreateApp(ctx, "empty", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Terminal(ctx, "empty", "production", client.TerminalOptions{Command: "true"}); status(err) != http.StatusConflict && status(err) != http.StatusNotFound {
		t.Errorf("nothing deployed: %v", err)
	}
	deployedSource(t, c, "shop", appManifest(""))
	if _, err := c.Terminal(ctx, "shop", "production", client.TerminalOptions{}); status(err) != http.StatusConflict {
		t.Errorf("no command: %v", err)
	}
	deployedSource(t, c, "site", map[string]string{"index.html": "a site"})
	if _, err := c.Terminal(ctx, "site", "production", client.TerminalOptions{Command: "true"}); status(err) != http.StatusConflict {
		t.Errorf("a static site: %v", err)
	}

	// A browser's session cannot start one, or a page elsewhere could have it do so.
	res, body := owner.get("/api/v1/apps/shop/envs/production/terminal?command=true")
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "token") {
		t.Errorf("%d %s", res.StatusCode, body)
	}

	// A token that may only read, or someone who may only look, cannot.
	if _, err := tokenClient(t, c, tokenOf(t, owner, auth.ScopeRead)).Terminal(ctx, "shop", "production", client.TerminalOptions{Command: "true"}); status(err) != http.StatusForbidden {
		t.Errorf("a read-only token: %v", err)
	}
	viewer := addTeammate(t, cfg, addrs, owner, "someone@example.com")
	if _, err := c.SetMember(ctx, "default", "someone@example.com", store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	viewerClient := tokenClient(t, c, tokenOf(t, viewer, auth.ScopeDeploy))
	if _, err := viewerClient.Terminal(ctx, "shop", "production", client.TerminalOptions{Command: "true"}); status(err) != http.StatusForbidden {
		t.Errorf("a viewer: %v", err)
	}

	// There is nothing in a process to go into, and it is told what to use instead.
	instanceID := instances(t, c, "shop")[0].ID
	if _, err := c.Terminal(ctx, "shop", "production", client.TerminalOptions{Instance: instanceID, Command: "true"}); status(err) != http.StatusConflict || !strings.Contains(err.Error(), "moor run") {
		t.Errorf("exec in a process: %v", err)
	}
	if _, err := c.Terminal(ctx, "shop", "production", client.TerminalOptions{Instance: "ins_nothing", Command: "true"}); status(err) != http.StatusConflict {
		t.Errorf("exec in nothing: %v", err)
	}

	// Who no longer may does not go on being at it.
	if _, err := c.SetMember(ctx, "default", "someone@example.com", store.RoleDeployer); err != nil {
		t.Fatal(err)
	}
	w := openTerminal(t, viewerClient, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `sh -c 'echo going; sleep 1000'`})
	w.waitFor("going")
	if _, err := c.SetMember(ctx, "default", "someone@example.com", store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	w.wait()
}

func TestTerminalOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("this is for Windows")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	deployedSource(t, c, "shop", appManifest(""))

	w := openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `cmd /c "echo size-shown && exit 7"`})
	w.waitFor("size-shown")
	w.wait()
	if code, problem, over := w.term.Ended(); !over || code == nil || *code != 7 {
		t.Errorf("%v %q %v", code, problem, over)
	}

	// A prompt, which answers what is typed.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `cmd`})
	w.type_("echo typed-at-it\r\n")
	w.waitFor("typed-at-it")
	if err := w.term.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	w.type_("exit 3\r\n")
	w.wait()
	if code, problem, over := w.term.Ended(); !over || code == nil || *code != 3 {
		t.Errorf("%v %q %v, having shown %q", code, problem, over, w.shown())
	}

	// With pipes.
	w = openTerminal(t, c, "shop", client.TerminalOptions{Command: `cmd /c "echo piped"`})
	w.waitFor("piped")
	w.term.CloseInput()
	w.wait()
	if code, _, _ := w.term.Ended(); code == nil || *code != 0 {
		t.Errorf("%v", code)
	}

	// Going away stops it.
	w = openTerminal(t, c, "shop", client.TerminalOptions{TTY: true, Rows: 24, Cols: 80, Command: `cmd`})
	w.type_("echo going\r\n")
	w.waitFor("going")
	w.term.Close()
	waitUntil(t, "it to stop", func() bool {
		runs, err := c.Runs(context.Background(), "shop", "production", "")
		if err != nil {
			return false
		}
		for _, r := range runs {
			if r.Status == "running" {
				return false
			}
		}
		return true
	})
}
