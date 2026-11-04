package toolchain

import (
	"bytes"
	"context"
	"crypto"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func newSigner(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	entity, err := openpgp.NewEntity(name, "", name+"@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	return entity
}

var (
	signerOnce sync.Once
	signer     *openpgp.Entity
)

// testSigner is who the fake provider's releases are signed by.
func testSigner(t *testing.T) *openpgp.Entity {
	t.Helper()
	signerOnce.Do(func() { signer = newSigner(t, "A Releaser") })
	return signer
}

func signedBy(t *testing.T, who *openpgp.Entity, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := openpgp.DetachSign(&out, who, bytes.NewReader(body), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// aSignedFile is a checksum file that the fake provider serves, as a release of Node.js would have it.
func aSignedFile(t *testing.T) (*fakeProvider, *Manager, file, []byte) {
	t.Helper()
	f, m := newFake(t)
	sums := []byte(strings.Repeat("a", 64) + "  node.tar.gz\n")
	f.files["/node/v1.2.3/SHASUMS256.txt.sig"] = signedBy(t, testSigner(t), sums)
	return f, m, file{name: "node.tar.gz", sigURL: m.src.Node + "v1.2.3/SHASUMS256.txt.sig", signedAs: "Node.js 1.2.3"}, sums
}

func TestSignatureGood(t *testing.T) {
	_, m, f, sums := aSignedFile(t)
	var log bytes.Buffer
	if err := m.checkSignature(context.Background(), f, sums, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "by A Releaser") {
		t.Errorf("%q", log.String())
	}
}

func TestSignatureRefused(t *testing.T) {
	ctx := context.Background()
	stranger := newSigner(t, "A Stranger")

	check := func(t *testing.T, change func(*fakeProvider), sums []byte, evenIfWarning bool, saying string) {
		t.Helper()
		provider, m, f, good := aSignedFile(t)
		if sums == nil {
			sums = good
		}
		if change != nil {
			change(provider)
		}
		err := m.checkSignature(ctx, f, sums, io.Discard)
		if err == nil || !strings.Contains(err.Error(), saying) {
			t.Errorf("wanted it refused for %q: %v", saying, err)
		}
		m.src.SignaturesOnlyWarn = true
		var log bytes.Buffer
		err = m.checkSignature(ctx, f, sums, &log)
		switch {
		case evenIfWarning && (err == nil || !strings.Contains(err.Error(), saying)):
			t.Errorf("with signatures = warn: %v", err)
		case !evenIfWarning && err != nil:
			t.Errorf("with signatures = warn: %v", err)
		case !evenIfWarning && !strings.Contains(log.String(), "warning: ") && !strings.Contains(log.String(), saying):
			t.Errorf("with signatures = warn, it says nothing: %q", log.String())
		}
	}
	t.Run("nobody we know", func(t *testing.T) {
		_, _, _, sums := aSignedFile(t)
		check(t, func(p *fakeProvider) {
			p.files["/node/v1.2.3/SHASUMS256.txt.sig"] = signedBy(t, stranger, sums)
		}, nil, false, "does not know")
	})
	t.Run("no signature", func(t *testing.T) {
		check(t, func(p *fakeProvider) { delete(p.files, "/node/v1.2.3/SHASUMS256.txt.sig") }, nil, false, "has no signature")
	})
	t.Run("checksums that were changed", func(t *testing.T) {
		check(t, nil, []byte(strings.Repeat("b", 64)+"  node.tar.gz\n"), true, "is wrong")
	})
	t.Run("not a signature", func(t *testing.T) {
		check(t, func(p *fakeProvider) { p.files["/node/v1.2.3/SHASUMS256.txt.sig"] = []byte("this is no signature") }, nil, true, "failed verification")
	})
}

func serveOnce(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestNodeKeysThatComeWithMoor checks real releases' real signatures against the real keys, by a releaser from
// long ago and by one of late.
func TestNodeKeysThatComeWithMoor(t *testing.T) {
	keys, err := nodeKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) < 20 {
		t.Fatalf("%d keys", len(keys))
	}
	_, m := newFake(t)
	m.src.NodeKeys = nil
	for _, version := range []string{"12.22.12", "24.11.1"} {
		sums, err := os.ReadFile("testdata/node-" + version + "-SHASUMS256.txt")
		if err != nil {
			t.Fatal(err)
		}
		signature, err := os.ReadFile("testdata/node-" + version + "-SHASUMS256.txt.sig")
		if err != nil {
			t.Fatal(err)
		}
		srvURL := serveOnce(t, signature)
		var log bytes.Buffer
		if err := m.checkSignature(context.Background(), file{sigURL: srvURL, signedAs: "Node.js " + version}, sums, &log); err != nil {
			t.Errorf("%s: %v", version, err)
		}
		if !strings.Contains(log.String(), "verified") {
			t.Errorf("%s: %q", version, log.String())
		}
	}
}

// TestSignatureByAKeyThatHadExpired: an old key is the likeliest to have gone astray, and a signature it makes now is
// not one of those it made while it was good.
func TestSignatureByAKeyThatHadExpired(t *testing.T) {
	old := time.Now().AddDate(-4, 0, 0)
	lifetime := uint32(365 * 24 * 3600)
	config := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, KeyLifetimeSecs: lifetime, Time: func() time.Time { return old }}
	key, err := openpgp.NewEntity("An Old Releaser", "", "old@example.com", config)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte(strings.Repeat("a", 64) + "  node.tar.gz\n")
	// Made by hand, as whoever holds an old key could: the library would not sign with one that has expired.
	signed := func(at time.Time) []byte {
		sig := &packet.Signature{Version: key.PrivateKey.Version, SigType: packet.SigTypeBinary, PubKeyAlgo: key.PrivateKey.PubKeyAlgo, Hash: crypto.SHA256, CreationTime: at, IssuerKeyId: &key.PrivateKey.KeyId}
		h, err := sig.PrepareSign(&packet.Config{DefaultHash: crypto.SHA256})
		if err != nil {
			t.Fatal(err)
		}
		h.Write(sums)
		if err := sig.Sign(h, key.PrivateKey, &packet.Config{DefaultHash: crypto.SHA256}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := sig.Serialize(&out); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	_, m := newFake(t)
	m.src.NodeKeys = openpgp.EntityList{key}
	check := func(signature []byte) error {
		return m.checkSignature(context.Background(), file{sigURL: serveOnce(t, signature), signedAs: "Node.js 1.2.3"}, sums, io.Discard)
	}
	if err := check(signed(old.AddDate(0, 1, 0))); err != nil {
		t.Errorf("made while it was good: %v", err)
	}
	if err := check(signed(time.Now())); err == nil || !strings.Contains(err.Error(), "already expired") {
		t.Errorf("made since: %v", err)
	}
}
