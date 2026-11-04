// Command releasekey makes and uses the key that releases are signed with. It is run by the release
// workflow and to make a key, and is no part of moor.
//
//	go run ./internal/upgrade/releasekey new PRIVATE_FILE     writes a new key, and prints its public half
//	go run ./internal/upgrade/releasekey sign FILE            prints a signature, with the key in $MOORAGE_RELEASE_KEY
//	go run ./internal/upgrade/releasekey verify FILE SIGFILE  checks one against the keys in the code
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/upgrade"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasekey:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "new":
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(f, base64.StdEncoding.EncodeToString(private.Seed()))
		if closed := f.Close(); err == nil {
			err = closed
		}
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(public))
		return nil
	case len(args) == 2 && args[0] == "sign":
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("MOORAGE_RELEASE_KEY")))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("$MOORAGE_RELEASE_KEY is not a key")
		}
		body, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		fmt.Print(upgrade.Sign(ed25519.NewKeyFromSeed(seed), body))
		return nil
	case len(args) == 3 && args[0] == "verify":
		keys, err := upgrade.ReleaseKeys()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		signature, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		if !upgrade.VerifySignature(keys, body, signature) {
			return errors.New("the signature is not from any of the keys in the code")
		}
		return nil
	}
	return errors.New("usage: releasekey new PRIVATE_FILE | sign FILE | verify FILE SIGFILE")
}
