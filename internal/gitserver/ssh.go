package gitserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/robert-mcdermott/moorage/internal/names"
)

// SSH serves git over SSH: exec requests for git-receive-pack and
// git-upload-pack, and nothing else — no shell, no terminal, no
// forwarding.
type SSH struct {
	Git     *Server
	HostKey ssh.Signer
	// Key returns who holds key, once the client has proved it does, or
	// false for a key that may not push.
	Key func(ctx context.Context, key ssh.PublicKey) (who string, ok bool)
	// Used, if set, is told the fingerprint of each key a client signed in
	// with (not of the keys it only offered).
	Used func(fingerprint string)
	// Attempts, if set, slows addresses whose handshakes keep failing.
	Attempts Attempts
	// Authorize, if set, says whether the key with this fingerprint may reach an app,
	// to push or to read. It returns ErrForbidden if not.
	Authorize func(ctx context.Context, fingerprint, app string, push bool) error

	mu    sync.Mutex
	ln    net.Listener
	conns map[*ssh.ServerConn]bool
	done  bool
}

// Attempts counts failures per key, here an address.
type Attempts interface {
	Wait(key string) time.Duration
	Fail(key string)
	Clear(key string)
}

const (
	userExtension = "moorage-user"
	keyExtension  = "moorage-key"
)

// LoadHostKey reads the server's SSH host key from path, creating an
// Ed25519 key there if there is none.
func LoadHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(key, "moorage host key")
		if err != nil {
			return nil, err
		}
		data = pem.EncodeToMemory(block)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

func (x *SSH) config() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-Moorage",
		MaxAuthTries:  6,
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			who, ok := x.Key(context.Background(), key)
			if !ok {
				return nil, errors.New("this key may not push to Moorage: add it with moor keys add")
			}
			// The library keeps the permissions of the key the client
			// signed with, not of the last key it offered: who comes from
			// there, never from a variable set here.
			return &ssh.Permissions{Extensions: map[string]string{userExtension: who, keyExtension: ssh.FingerprintSHA256(key)}}, nil
		},
	}
	cfg.AddHostKey(x.HostKey)
	return cfg
}

// Serve accepts connections on l until Close.
func (x *SSH) Serve(l net.Listener) error {
	x.mu.Lock()
	x.ln, x.conns = l, map[*ssh.ServerConn]bool{}
	x.mu.Unlock()
	cfg := x.config()
	for {
		nc, err := l.Accept()
		if err != nil {
			x.mu.Lock()
			done := x.done
			x.mu.Unlock()
			if done {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		go x.handle(nc, cfg)
	}
}

// Close stops accepting connections and closes those open: a push under
// way fails, and its deployment goes on.
func (x *SSH) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.done = true
	for c := range x.conns {
		c.Close()
	}
	if x.ln == nil {
		return nil
	}
	return x.ln.Close()
}

func (x *SSH) handle(nc net.Conn, cfg *ssh.ServerConfig) {
	ip, _, _ := net.SplitHostPort(nc.RemoteAddr().String())
	key := "ssh-ip:" + ip
	if x.Attempts != nil && x.Attempts.Wait(key) > 0 {
		x.Git.logger().Warn("security: an SSH connection refused by the rate limit", "ip", ip)
		nc.Close()
		return
	}
	nc.SetDeadline(time.Now().Add(30 * time.Second)) // for the handshake
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		if x.Attempts != nil {
			x.Attempts.Fail(key)
		}
		x.Git.logger().Warn("security: an SSH sign-in failed", "ip", ip, "err", err)
		nc.Close()
		return
	}
	if x.Attempts != nil {
		x.Attempts.Clear(key)
	}
	nc.SetDeadline(time.Time{})
	x.mu.Lock()
	if x.done {
		x.mu.Unlock()
		conn.Close()
		return
	}
	x.conns[conn] = true
	x.mu.Unlock()
	defer func() {
		x.mu.Lock()
		delete(x.conns, conn)
		x.mu.Unlock()
		conn.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { conn.Wait(); cancel() }()
	go ssh.DiscardRequests(reqs) // global requests, such as forwarding: refused
	who := conn.Permissions.Extensions[userExtension]
	if x.Used != nil {
		x.Used(conn.Permissions.Extensions[keyExtension])
	}
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "Moorage serves git only")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go x.session(ctx, who, conn.Permissions.Extensions[keyExtension], channel, requests)
	}
}

// session waits for a session's exec request and runs it. Environment
// requests set GIT_PROTOCOL; every other request is refused.
func (x *SSH) session(ctx context.Context, who, fingerprint string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	protocol := ""
	for req := range reqs {
		switch req.Type {
		case "env":
			var kv struct{ Name, Value string }
			if ssh.Unmarshal(req.Payload, &kv) == nil && kv.Name == "GIT_PROTOCOL" {
				protocol = kv.Value
				req.Reply(true, nil)
				continue
			}
			req.Reply(false, nil)
		case "exec":
			var cmd struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &cmd); err != nil {
				req.Reply(false, nil)
				return
			}
			req.Reply(true, nil)
			go ssh.DiscardRequests(reqs)
			status := x.exec(ctx, who, fingerprint, cmd.Command, protocol, ch)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			return
		default: // shell, pty-req, subsystem, x11-req…
			if req.Type == "shell" {
				fmt.Fprintln(ch.Stderr(), "Hi! Moorage serves git here, not a shell: push with git push.")
				req.Reply(true, nil)
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
				return
			}
			req.Reply(false, nil)
		}
	}
}

// exec runs a git command and returns its exit status.
func (x *SSH) exec(ctx context.Context, who, fingerprint, command, protocol string, ch ssh.Channel) uint32 {
	service, app, err := parseCommand(command)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "moorage: %v\n", err)
		return 1
	}
	if x.Authorize != nil {
		if err := x.Authorize(ctx, fingerprint, app, service == "git-receive-pack"); err != nil {
			fmt.Fprintf(ch.Stderr(), "moorage: you may not do that with %s: pushing takes being a deployer of its project\n", app)
			return 1
		}
	}
	out := channelWriter{ch}
	switch service {
	case "git-receive-pack":
		err = x.Git.receivePack(ctx, app, who, ch, out, false, false, protocol)
	case "git-upload-pack":
		err = x.Git.uploadPack(ctx, app, io.NopCloser(ch), out, false, false, protocol)
	}
	if errors.Is(err, ErrNoRepository) {
		fmt.Fprintf(ch.Stderr(), "moorage: there is no app named %s\n", app)
		return 1
	}
	if err != nil {
		x.Git.logger().Info("git over SSH", "service", service, "app", app, "by", who, "err", err)
		return 1
	}
	return 0
}

// channelWriter ends the command's output, without closing the channel
// before its exit status is sent.
type channelWriter struct{ ch ssh.Channel }

func (w channelWriter) Write(p []byte) (int, error) { return w.ch.Write(p) }
func (w channelWriter) Close() error                { return w.ch.CloseWrite() }

// parseCommand parses an exec request such as git-receive-pack '/app.git'
// into the service and the app.
func parseCommand(command string) (service, app string, err error) {
	service, arg, ok := strings.Cut(strings.TrimSpace(command), " ")
	if service == "git" { // the older form: git receive-pack '…'
		var sub string
		sub, arg, ok = strings.Cut(strings.TrimSpace(arg), " ")
		service = "git-" + sub
	}
	if !ok || (service != "git-receive-pack" && service != "git-upload-pack") {
		return "", "", fmt.Errorf("Moorage serves git push and git fetch only, not %q", command)
	}
	path := strings.Trim(strings.TrimSpace(arg), "'\"")
	app = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if err := names.ValidateApp(app); err != nil {
		return "", "", fmt.Errorf("%q is not an app's repository: use ssh://git@<server>/<app>.git", path)
	}
	return service, app, nil
}
