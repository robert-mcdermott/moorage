package gitserver

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrPackTooLarge is returned when a push sends more than the limit.
var ErrPackTooLarge = errors.New("the push is larger than the server accepts")

// requestReader hands a receive-pack request on to the git library and ends
// it where the request ends: after the commands, the push options and the
// pack's trailing checksum. The library reads until end of file, which the
// git command line gives it by closing its side, but other clients keep
// the connection open to read the answer (spike S1).
//
// It reads the client's stream in a goroutine, parsing it as it goes, and
// passes every byte it consumed through a pipe that it closes at the end of
// the request.
func requestReader(client io.Reader, limit int64) io.ReadCloser {
	pr, pw := io.Pipe()
	t := &tee{src: bufio.NewReaderSize(client, 64<<10), limit: limit}
	t.dst = bufio.NewWriterSize(pw, 64<<10)
	go func() {
		err := t.request()
		if ferr := t.dst.Flush(); err == nil {
			err = ferr
		}
		pw.CloseWithError(err) // nil closes with io.EOF
	}()
	return pr
}

// tee reads the client's stream and writes everything it reads to dst. It
// flushes dst before it waits for the client, so that what the library
// needs is never held back.
type tee struct {
	src   *bufio.Reader
	dst   *bufio.Writer
	n     int64
	limit int64
}

func (t *tee) wait() error {
	if t.src.Buffered() == 0 {
		return t.dst.Flush()
	}
	return nil
}

func (t *tee) count(n int) error {
	t.n += int64(n)
	if t.limit > 0 && t.n > t.limit {
		return ErrPackTooLarge
	}
	return nil
}

func (t *tee) ReadByte() (byte, error) {
	if err := t.wait(); err != nil {
		return 0, err
	}
	b, err := t.src.ReadByte()
	if err != nil {
		return 0, err
	}
	if err := t.count(1); err != nil {
		return 0, err
	}
	return b, t.dst.WriteByte(b)
}

func (t *tee) Read(p []byte) (int, error) {
	if err := t.wait(); err != nil {
		return 0, err
	}
	n, err := t.src.Read(p)
	if n > 0 {
		if cerr := t.count(n); cerr != nil {
			return 0, cerr
		}
		if _, werr := t.dst.Write(p[:n]); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

// full reads exactly len(p) bytes.
func (t *tee) full(p []byte) error {
	_, err := io.ReadFull(t, p)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// request reads a receive-pack update request: pkt-lines up to a flush
// (the commands, after any shallow lines), the push options up to a flush
// when they were negotiated, then the pack when a command needs one.
func (t *tee) request() error {
	lines, err := t.pktLines()
	if err != nil || len(lines) == 0 {
		return err // a flush alone: nothing to update
	}
	var caps []string
	needPack := false
	for _, l := range lines {
		cmd, c, hasCaps := strings.Cut(l, "\x00")
		if hasCaps && caps == nil {
			caps = strings.Fields(c)
		}
		f := strings.Fields(cmd)
		if len(f) == 3 && strings.Trim(f[1], "0") != "" {
			needPack = true // an update or a creation, not a deletion
		}
	}
	hashSize := 20
	for _, c := range caps {
		if c == "push-options" {
			if _, err := t.pktLines(); err != nil {
				return err
			}
		}
		if c == "object-format=sha256" {
			hashSize = 32
		}
	}
	if !needPack {
		return nil
	}
	return t.pack(hashSize)
}

// pktLines reads pkt-lines up to a flush and returns their payloads.
func (t *tee) pktLines() ([]string, error) {
	var lines []string
	hdr := make([]byte, 4)
	for {
		if err := t.full(hdr); err != nil {
			return nil, fmt.Errorf("reading the request: %w", err)
		}
		n, err := strconv.ParseUint(string(hdr), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("reading the request: bad pkt-line length %q", hdr)
		}
		switch {
		case n == 0:
			return lines, nil
		case n < 4:
			return nil, fmt.Errorf("reading the request: bad pkt-line length %d", n)
		}
		payload := make([]byte, n-4)
		if err := t.full(payload); err != nil {
			return nil, fmt.Errorf("reading the request: %w", err)
		}
		lines = append(lines, string(bytes.TrimSuffix(payload, []byte("\n"))))
	}
}

// pack reads a pack: its header, each object (a type-and-size header, a
// delta's base, then a zlib stream, which ends itself), and the checksum.
func (t *tee) pack(hashSize int) error {
	hdr := make([]byte, 12)
	if err := t.full(hdr); err != nil {
		return fmt.Errorf("reading the pack: %w", err)
	}
	if !bytes.Equal(hdr[:4], []byte("PACK")) {
		return errors.New("reading the pack: it does not start with PACK")
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return fmt.Errorf("reading the pack: version %d", v)
	}
	objects := binary.BigEndian.Uint32(hdr[8:12])
	base := make([]byte, hashSize)
	for i := uint32(0); i < objects; i++ {
		b, err := t.ReadByte()
		if err != nil {
			return fmt.Errorf("reading the pack: %w", unexpected(err))
		}
		kind := (b >> 4) & 7
		for b&0x80 != 0 { // the rest of the size
			if b, err = t.ReadByte(); err != nil {
				return fmt.Errorf("reading the pack: %w", unexpected(err))
			}
		}
		switch kind {
		case 6: // OFS_DELTA: the base's offset, in its own varint
			for {
				if b, err = t.ReadByte(); err != nil {
					return fmt.Errorf("reading the pack: %w", unexpected(err))
				}
				if b&0x80 == 0 {
					break
				}
			}
		case 7: // REF_DELTA: the base's hash
			if err := t.full(base); err != nil {
				return fmt.Errorf("reading the pack: %w", err)
			}
		}
		// zlib reads byte by byte from an io.ByteReader, so it stops where
		// its stream ends.
		z, err := zlib.NewReader(t)
		if err != nil {
			return fmt.Errorf("reading the pack: object %d: %w", i, unexpected(err))
		}
		if _, err := io.Copy(io.Discard, z); err != nil {
			return fmt.Errorf("reading the pack: object %d: %w", i, unexpected(err))
		}
		if err := z.Close(); err != nil {
			return fmt.Errorf("reading the pack: object %d: %w", i, err)
		}
	}
	if err := t.full(base); err != nil { // the trailing checksum
		return fmt.Errorf("reading the pack's checksum: %w", err)
	}
	return nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
