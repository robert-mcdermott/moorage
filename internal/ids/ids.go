// Package ids creates typed, sortable identifiers: a short lowercase prefix
// naming the kind of entity, an underscore and a ULID — a 48-bit millisecond
// timestamp and 80 random bits in Crockford base32 — such as
// app_01JBXK3M8QZ5V4RRFFQ69G5FAV. Identifiers sort by creation time, and
// those created by one process are strictly increasing.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Prefixes of the entities in the data model (docs/10-data-model.md).
const (
	App        = "app"
	Env        = "env"
	Build      = "bld"
	Release    = "rel"
	Deployment = "dep"
	Instance   = "ins"
	User       = "usr"
	Token      = "tok"
	Var        = "var"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidLen is the length of the part after the underscore.
const ulidLen = 26

var (
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
)

// New returns a new identifier with the given prefix, which must be two to
// four lowercase ASCII letters.
func New(prefix string) string {
	if !validPrefix(prefix) {
		panic(fmt.Sprintf("ids: invalid prefix %q", prefix))
	}
	mu.Lock()
	defer mu.Unlock()
	ms := uint64(time.Now().UnixMilli())
	if ms <= lastMS {
		// The same millisecond as the last identifier (or an earlier one, if
		// the clock stepped back): keep the last timestamp and increment the
		// random part, so identifiers keep increasing.
		ms = lastMS
		if !increment(&lastRand) {
			ms++
			_, _ = rand.Read(lastRand[:])
		}
	} else {
		_, _ = rand.Read(lastRand[:])
	}
	lastMS = ms
	return prefix + "_" + encode(ms, lastRand)
}

// Parse returns an identifier's prefix and creation time, or an error if it
// is not well formed. Lowercase ULIDs are accepted.
func Parse(id string) (prefix string, created time.Time, err error) {
	prefix, ulid, ok := strings.Cut(id, "_")
	if !ok {
		return "", time.Time{}, fmt.Errorf("ids: %q has no prefix", id)
	}
	if !validPrefix(prefix) {
		return "", time.Time{}, fmt.Errorf("ids: %q has an invalid prefix", id)
	}
	if len(ulid) != ulidLen {
		return "", time.Time{}, fmt.Errorf("ids: %q is not %d characters after the prefix", id, ulidLen)
	}
	var ms uint64
	for i := 0; i < ulidLen; i++ {
		v := strings.IndexByte(alphabet, upper(ulid[i]))
		if v < 0 {
			return "", time.Time{}, fmt.Errorf("ids: %q contains %q, which is not Crockford base32", id, ulid[i])
		}
		if i == 0 && v > 7 {
			return "", time.Time{}, fmt.Errorf("ids: %q has a timestamp out of range", id)
		}
		if i < 10 {
			ms = ms<<5 | uint64(v)
		}
	}
	return prefix, time.UnixMilli(int64(ms)).UTC(), nil
}

// encode writes the 48-bit timestamp as 10 characters and the 80 random
// bits as 16 characters.
func encode(ms uint64, r [10]byte) string {
	var b [ulidLen]byte
	for i := 9; i >= 0; i-- {
		b[i] = alphabet[ms&31]
		ms >>= 5
	}
	hi := uint64(r[0])<<8 | uint64(r[1])
	lo := binary.BigEndian.Uint64(r[2:])
	for i := ulidLen - 1; i >= 10; i-- {
		b[i] = alphabet[lo&31]
		lo = lo>>5 | (hi&31)<<59
		hi >>= 5
	}
	return string(b[:])
}

// increment adds one to r as an 80-bit big-endian number and reports false
// if it overflowed.
func increment(r *[10]byte) bool {
	for i := len(r) - 1; i >= 0; i-- {
		r[i]++
		if r[i] != 0 {
			return true
		}
	}
	return false
}

func validPrefix(p string) bool {
	if len(p) < 2 || len(p) > 4 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 'a' || p[i] > 'z' {
			return false
		}
	}
	return true
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}
