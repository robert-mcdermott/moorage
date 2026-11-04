package localdns

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 77, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}},
	}
	data, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		typ   dnsmessage.Type
		rcode dnsmessage.RCode
		body  dnsmessage.ResourceBody
	}{
		{"shop.localhost.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}},
		{"Shop.LocalHost.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}},
		{"a.b.localhost.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, &dnsmessage.AAAAResource{AAAA: [16]byte{15: 1}}},
		{"localhost.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}},
		{"shop.localhost.", dnsmessage.TypeMX, dnsmessage.RCodeSuccess, nil},
		{"example.com.", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
		{"notlocalhost.", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
		{"localhost.example.com.", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
	} {
		data, err := Answer(query(t, c.name, c.typ), "localhost")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var m dnsmessage.Message
		if err := m.Unpack(data); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if m.ID != 77 || !m.Response || m.RCode != c.rcode || !m.RecursionDesired {
			t.Errorf("%s: %+v", c.name, m.Header)
		}
		if len(m.Questions) != 1 || m.Questions[0].Name.String() != c.name {
			t.Errorf("%s: questions %+v", c.name, m.Questions)
		}
		if c.body == nil {
			if len(m.Answers) != 0 {
				t.Errorf("%s: answers %+v", c.name, m.Answers)
			}
			continue
		}
		if len(m.Answers) != 1 || m.Answers[0].Header.Name.String() != c.name {
			t.Fatalf("%s: answers %+v", c.name, m.Answers)
		}
		switch want := c.body.(type) {
		case *dnsmessage.AResource:
			if got, ok := m.Answers[0].Body.(*dnsmessage.AResource); !ok || got.A != want.A {
				t.Errorf("%s: %+v", c.name, m.Answers[0].Body)
			}
		case *dnsmessage.AAAAResource:
			if got, ok := m.Answers[0].Body.(*dnsmessage.AAAAResource); !ok || got.AAAA != want.AAAA {
				t.Errorf("%s: %+v", c.name, m.Answers[0].Body)
			}
		}
	}
	if _, err := Answer([]byte("nonsense"), "localhost"); err == nil {
		t.Error("answered nonsense")
	}
}

// TestServe asks over the network, with Go's own resolver as the client.
func TestServe(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, conn, "apps.test", slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
	lookup, cancelLookup := context.WithTimeout(ctx, 5*time.Second)
	defer cancelLookup()
	addrs, err := r.LookupHost(lookup, "shop.apps.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 {
		t.Errorf("%v", addrs)
	}
	if _, err := r.LookupHost(lookup, "shop.example.com"); err == nil {
		t.Error("it answered for another domain")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve did not stop")
	}
}

func TestHostsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := "# what someone wrote\n10.0.0.1 printer\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasHostsBlock(path) {
		t.Error("it has no block yet")
	}
	changed, err := WriteHostsBlock(path, []string{"shop.localhost", "moorage.localhost"})
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	body, _ := os.ReadFile(path)
	for _, want := range []string{"10.0.0.1 printer", "127.0.0.1 moorage.localhost", "::1 shop.localhost", hostsBegin, hostsEnd} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%q is not in:\n%s", want, body)
		}
	}
	if !HasHostsBlock(path) {
		t.Error("it has a block now")
	}
	// The same again changes nothing, and fewer names leave only those.
	if changed, err := WriteHostsBlock(path, []string{"moorage.localhost", "shop.localhost"}); err != nil || changed {
		t.Errorf("%v %v", changed, err)
	}
	if _, err := WriteHostsBlock(path, []string{"moorage.localhost"}); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if strings.Contains(string(body), "shop.localhost") || strings.Count(string(body), hostsBegin) != 1 {
		t.Errorf("%s", body)
	}
	if err := RemoveHostsBlock(path); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if strings.TrimSpace(string(body)) != strings.TrimSpace(original) {
		t.Errorf("what was around it changed: %q", body)
	}
}

func TestHostsBlockKeepsLineEndings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("10.0.0.1 printer\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteHostsBlock(path, []string{"shop.localhost"}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(strings.ReplaceAll(string(body), "\r\n", ""), "\n") {
		t.Errorf("%q", body)
	}
}
