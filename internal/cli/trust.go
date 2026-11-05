package cli

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/server"
	"github.com/robert-mcdermott/moorage/internal/truststore"
)

// trustSystem is the machine moor trust changes; tests give it a fake one.
var trustSystem = truststore.Local

func newTrustCmd() *cobra.Command {
	var (
		conn              connection
		file, fingerprint string
		remove, print     bool
	)
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Trust the server's internal CA on this machine",
		Long: `Trust the certificate authority of a server on this machine, so that browsers
and command-line tools accept the certificates it issues. The CA goes into the
operating system's list and, on Linux, into the browsers' own lists.

A CA is powerful: whoever holds its key can pretend to be any site it may sign
for. Moorage's own CA may sign only for the server's domain, and moor trust says
what the one it finds may sign for. It comes from the server's own files on the
server's machine, and otherwise from the server, over a connection nothing can
vouch for yet: compare its fingerprint with what moor server status shows on the
server. --fingerprint does that without asking.

On macOS and Windows, moor trust changes your own list, and asks you to confirm;
as root or an Administrator, the machine's. On Linux it uses sudo.`,
		Example: `  moor trust
  moor trust --server https://moorage.apps.example.com
  moor trust --server https://moorage.apps.example.com --fingerprint AB:CD:…
  moor trust --file corporate-root.pem
  moor trust --print > moorage-ca.crt
  moor trust --remove`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if remove && print {
				return usageError{errors.New("pass --remove or --print, not both")}
			}
			ca, err := findCA(cmd, &conn, file)
			if err != nil {
				return err
			}
			errOut := cmd.ErrOrStderr()
			if fingerprint != "" && !truststore.SameFingerprint(fingerprint, ca.cert) {
				return fmt.Errorf("the CA from %s has the fingerprint %s, not the one you gave: do not trust it until you know why", ca.from, truststore.Fingerprint(ca.cert))
			}
			describeCA(errOut, ca)
			if print {
				return pem.Encode(cmd.OutOrStdout(), &pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
			}
			system := trustSystem()
			if remove {
				res, err := system.Remove(ca.cert)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed it from %s.\n", strings.Join(res.Places, ", "))
				for _, note := range res.Notes {
					fmt.Fprintln(cmd.OutOrStdout(), note)
				}
				return nil
			}
			if truststore.Trusted(ca.cert) {
				fmt.Fprintln(cmd.OutOrStdout(), "This machine already trusts it.")
				return nil
			}
			if !ca.vouched && fingerprint == "" {
				if err := confirmCA(cmd, ca); err != nil {
					return err
				}
			}
			res, err := system.Install(ca.cert)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Added it to %s.\n", strings.Join(res.Places, ", "))
			for _, note := range res.Notes {
				fmt.Fprintln(out, note)
			}
			fmt.Fprintln(out, "A browser that is open may need restarting. moor trust --remove undoes this.")
			return nil
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&file, "file", "", "trust this PEM file, instead of asking a server")
	f.StringVar(&fingerprint, "fingerprint", "", "the SHA-256 fingerprint the CA must have; asks nothing if it does")
	f.BoolVar(&remove, "remove", false, "stop trusting the CA")
	f.BoolVar(&print, "print", false, "print the CA certificate, and change nothing")
	return cmd
}

// foundCA is a CA, and how much its origin says for it.
type foundCA struct {
	cert *x509.Certificate
	from string
	// vouched says it comes from somewhere that needs no second opinion: a file
	// the user named, or the server's own files on this machine.
	vouched bool
}

func parseCA(data []byte, from string) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s holds no PEM certificate", from)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("%s is not a certificate authority: it is the certificate of %s", from, cert.Subject)
	}
	return cert, nil
}

// findCA finds the CA: a file, the server's own on this machine, or the server's
// answer.
func findCA(cmd *cobra.Command, conn *connection, file string) (foundCA, error) {
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return foundCA{}, err
		}
		cert, err := parseCA(data, file)
		return foundCA{cert: cert, from: file, vouched: true}, err
	}
	serverURL, _, _, err := conn.resolve()
	if err != nil {
		return foundCA{}, err
	}
	if localURL, _, err := localServer(); err == nil && localURL == serverURL {
		path, err := configPath("")
		if err != nil {
			return foundCA{}, err
		}
		cfg, err := config.Load(path, os.Getenv)
		if err != nil {
			return foundCA{}, err
		}
		switch cfg.TLS.Mode {
		case "off":
			return foundCA{}, errors.New("this server serves plain HTTP, so there is no certificate of its to trust. Behind a TLS proxy, the proxy's certificate is the one that matters")
		case "acme":
			return foundCA{}, errors.New("this server's certificates come from a CA: a public CA's are trusted already, and for an internal one, trust its root with moor trust --file")
		case "files":
			return foundCA{}, errors.New("this server uses the certificates you gave it: trust their issuer with moor trust --file ISSUER.pem")
		}
		caFile := cfg.TLS.CACert
		if caFile == "" {
			caFile = filepath.Join(server.NewLayout(cfg.DataDir).Certs, "ca.crt")
		}
		data, err := os.ReadFile(caFile)
		if errors.Is(err, os.ErrNotExist) {
			return foundCA{}, errors.New("this server has no CA yet: it makes one the first time it starts")
		}
		if err == nil {
			cert, err := parseCA(data, caFile)
			return foundCA{cert: cert, from: caFile, vouched: true}, err
		}
		// Not this user's to read: it is served to anyone, and this is loopback.
		cert, err := fetchCA(cmd, serverURL)
		return foundCA{cert: cert, from: serverURL, vouched: true}, err
	}
	cert, err := fetchCA(cmd, serverURL)
	return foundCA{cert: cert, from: serverURL}, err
}

// fetchCA asks a server for its CA. Nothing vouches for the connection, since
// the point is to trust what it says for the first time. What it can check, it
// does: that the server's own certificate comes from that CA.
func fetchCA(cmd *cobra.Command, serverURL string) (*x509.Certificate, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Get(strings.TrimRight(serverURL, "/") + "/.moorage/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("ask %s for its CA: %w", u.Host, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s has no internal CA to trust (it answered %s). A server behind a TLS proxy, or using your own certificates, relies on their issuer: moor trust --file ISSUER.pem", u.Host, res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	cert, err := parseCA(data, "the answer of "+u.Host)
	if err != nil {
		return nil, err
	}
	if res.TLS != nil && len(res.TLS.PeerCertificates) > 0 {
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		inter := x509.NewCertPool()
		for _, c := range res.TLS.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		if _, err := res.TLS.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: u.Hostname()}); err != nil {
			return nil, fmt.Errorf("the certificate %s presents does not come from the CA it offers (%v): something may stand between you and the server, so Moorage will not trust it", u.Host, err)
		}
	}
	return cert, nil
}

func describeCA(w io.Writer, ca foundCA) {
	fmt.Fprintf(w, "%s\n", ca.cert.Subject.CommonName)
	fmt.Fprintf(w, "  Fingerprint  SHA-256 %s\n", truststore.Fingerprint(ca.cert))
	if names := ca.cert.PermittedDNSDomains; len(names) > 0 {
		fmt.Fprintf(w, "  May sign for %s, and nothing else\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintf(w, "  May sign for any site: it has no name constraints\n")
	}
	fmt.Fprintf(w, "  Expires      %s\n", ca.cert.NotAfter.Local().Format("2 Jan 2006"))
	fmt.Fprintf(w, "  From         %s\n", ca.from)
}

// confirmCA asks, of a CA nothing vouches for.
func confirmCA(cmd *cobra.Command, ca foundCA) error {
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return errors.New("nothing here can confirm this CA: compare its fingerprint with moor server status on the server, and pass it with --fingerprint")
	}
	errOut := cmd.ErrOrStderr()
	fmt.Fprint(errOut, "\nNothing vouches for this answer yet: compare the fingerprint with what\nmoor server status shows on the server. Trust this CA on this computer? [y/N] ")
	line, _ := bufio.NewReader(file).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("not trusted: nothing changed")
	}
	return nil
}
