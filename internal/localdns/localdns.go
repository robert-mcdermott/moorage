// Package localdns is a small DNS responder for a server on one's own computer:
// it answers for the server's domain with the loopback addresses, and refuses
// everything else. The macOS resolver can be pointed at it for *.localhost, which
// it does not resolve by itself (spec/02-architecture.md, "Configuration").
package localdns

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// ttl is short: nothing is gained by caching a name that always means the same.
const ttl = 60

// Serve answers queries on conn until ctx ends.
func Serve(ctx context.Context, conn net.PacketConn, domain string, log *slog.Logger) error {
	go func() {
		<-ctx.Done()
		conn.SetDeadline(time.Now())
	}()
	buf := make([]byte, 1500)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		reply, err := Answer(buf[:n], domain)
		if err != nil {
			log.Debug("a DNS query that could not be read", "from", addr, "err", err)
			continue
		}
		if _, err := conn.WriteTo(reply, addr); err != nil && ctx.Err() == nil {
			log.Debug("a DNS reply that could not be sent", "to", addr, "err", err)
		}
	}
}

// Answer returns the reply to a query.
func Answer(query []byte, domain string) ([]byte, error) {
	var p dnsmessage.Parser
	head, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	if head.Response {
		return nil, errors.New("not a query")
	}
	questions, err := p.AllQuestions()
	if err != nil {
		return nil, err
	}
	reply := dnsmessage.Message{
		Header: dnsmessage.Header{ID: head.ID, Response: true, Authoritative: true, RecursionDesired: head.RecursionDesired},
	}
	if head.OpCode != 0 || len(questions) != 1 {
		reply.RCode = dnsmessage.RCodeNotImplemented
		return reply.Pack()
	}
	q := questions[0]
	reply.Questions = questions
	if !under(q.Name.String(), domain) {
		reply.RCode = dnsmessage.RCodeRefused
		return reply.Pack()
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: ttl}
	switch q.Type {
	case dnsmessage.TypeA:
		rh.Type = dnsmessage.TypeA
		reply.Answers = []dnsmessage.Resource{{Header: rh, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}}}
	case dnsmessage.TypeAAAA:
		rh.Type = dnsmessage.TypeAAAA
		reply.Answers = []dnsmessage.Resource{{Header: rh, Body: &dnsmessage.AAAAResource{AAAA: [16]byte{15: 1}}}}
	}
	return reply.Pack()
}

// under reports whether a name (with its final dot) is domain or below it.
func under(name, domain string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	domain = strings.ToLower(domain)
	return name == domain || strings.HasSuffix(name, "."+domain)
}
