package toolchain

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

//go:embed node-release-keys/*.asc
var nodeKeyFiles embed.FS

// nodeKeys reads the keys that come with moor, the first time they are needed.
var nodeKeys = sync.OnceValues(func() (openpgp.EntityList, error) {
	var all openpgp.EntityList
	names, err := fs.Glob(nodeKeyFiles, "node-release-keys/*.asc")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		body, err := nodeKeyFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		all = append(all, keys...)
	}
	return all, nil
})

// checkSignature checks that a release's checksums were signed by one of those who sign its releases. HTTPS
// says that they came from the provider's address; this says that they came from the provider.
func (m *Manager) checkSignature(ctx context.Context, f file, sums []byte, log io.Writer) error {
	keys := m.src.NodeKeys
	if keys == nil {
		var err error
		if keys, err = nodeKeys(); err != nil {
			return err
		}
	}
	unchecked := func(why string) error {
		if m.src.SignaturesOnlyWarn {
			fmt.Fprintf(log, "warning: %s: going on, since signatures = \"warn\"\n", why)
			return nil
		}
		return fmt.Errorf("%s. If that is expected, as with a mirror that leaves signatures out or a new releaser that this moor does not know of yet, upgrade moor, or set signatures = \"warn\" under [toolchains] in server.toml", why)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.sigURL, nil)
	if err != nil {
		return err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { // a mirror may say anything, for something it does not have
		return unchecked(fmt.Sprintf("%s has no signature at %s (%s)", f.signedAs, f.sigURL, resp.Status))
	}
	signature, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	entity, err := openpgp.CheckDetachedSignature(keys, bytes.NewReader(sums), bytes.NewReader(signature), nil)
	switch {
	case errors.Is(err, pgperrors.ErrUnknownIssuer):
		return unchecked(fmt.Sprintf("%s is signed by a key that this moor does not know", f.signedAs))
	case errors.Is(err, pgperrors.ErrKeyExpired) && entity != nil:
		// The signature is good, by a key that has expired since, as old releases' keys do. One that says it was
		// made after that is no release of theirs. Someone holding the key could date it earlier, so that this
		// catches a mistake or a careless forger, and is no defence against a careful one.
		if !madeBeforeExpiry(keys, signature) {
			return fmt.Errorf("%s failed verification: its signature was made with a key that had already expired", f.signedAs)
		}
	case err != nil:
		return fmt.Errorf("%s failed verification: the signature on its checksums is wrong: %w", f.signedAs, err)
	}
	who := "a known key"
	if identity := entity.PrimaryIdentity(); identity != nil && identity.UserId != nil {
		who = identity.UserId.Name
	}
	fmt.Fprintf(log, "verified the signature on the checksums of %s, by %s\n", f.signedAs, who)
	return nil
}

// madeBeforeExpiry says whether a signature was made while the key that made it was good.
func madeBeforeExpiry(keys openpgp.EntityList, signature []byte) bool {
	p, err := packet.Read(bytes.NewReader(signature))
	if err != nil {
		return false
	}
	sig, ok := p.(*packet.Signature)
	if !ok || sig.IssuerKeyId == nil {
		return false
	}
	for _, key := range keys.KeysById(*sig.IssuerKeyId) {
		if key.PublicKey != nil && key.SelfSignature != nil && !key.PublicKey.KeyExpired(key.SelfSignature, sig.CreationTime) {
			return true
		}
	}
	return false
}
