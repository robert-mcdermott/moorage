package localdns

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/platform"
)

// The lines between which the hosts file is this server's to change, and nothing outside them.
const (
	hostsBegin = "# BEGIN Moorage: this block is written by moor server resolver"
	hostsEnd   = "# END Moorage"
)

// HasHostsBlock says whether a hosts file has the block.
func HasHostsBlock(path string) bool {
	body, err := os.ReadFile(path)
	return err == nil && bytes.Contains(body, []byte(hostsBegin))
}

// WriteHostsBlock has the block say that these names are on this computer. It says whether that changed anything.
func WriteHostsBlock(path string, names []string) (bool, error) {
	before, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	var block strings.Builder
	block.WriteString(hostsBegin + eol(before))
	for _, name := range sorted {
		block.WriteString("127.0.0.1 " + name + eol(before))
		block.WriteString("::1 " + name + eol(before))
	}
	block.WriteString(hostsEnd + eol(before))
	rest := withoutBlock(before)
	after := append(bytes.TrimRight(rest, "\r\n"), []byte(eol(before)+eol(before))...)
	if len(bytes.TrimSpace(rest)) == 0 {
		after = nil
	}
	after = append(after, block.String()...)
	if bytes.Equal(before, after) {
		return false, nil
	}
	return true, writeOver(path, after)
}

// RemoveHostsBlock takes the block out, leaving what is around it.
func RemoveHostsBlock(path string) error {
	before, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(before, []byte(hostsBegin)) {
		return nil
	}
	after := append(bytes.TrimRight(withoutBlock(before), "\r\n"), []byte(eol(before))...)
	return writeOver(path, after)
}

func eol(body []byte) string {
	if bytes.Contains(body, []byte("\r\n")) || (len(body) == 0 && filepath.Separator == '\\') {
		return "\r\n"
	}
	return "\n"
}

func withoutBlock(body []byte) []byte {
	start := bytes.Index(body, []byte(hostsBegin))
	if start < 0 {
		return body
	}
	end := bytes.Index(body[start:], []byte(hostsEnd))
	if end < 0 {
		// Nothing says where it ended, and what follows may be someone's own. So only the line that began it goes.
		rest := body[start:]
		if newline := bytes.IndexByte(rest, '\n'); newline >= 0 {
			return append(append([]byte{}, body[:start]...), rest[newline+1:]...)
		}
		return body[:start]
	}
	rest := body[start+end+len(hostsEnd):]
	return append(append([]byte{}, body[:start]...), bytes.TrimLeft(rest, "\r\n")...)
}

// writeOver changes the file in one step, as a program reading it at that moment should see all of one or the other,
// keeping who may do what with it.
func writeOver(path string, body []byte) error {
	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hosts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(body)
	if closed := tmp.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		if err := platform.CopyAccess(path, tmp.Name()); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}
