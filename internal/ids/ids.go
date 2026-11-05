// Package ids generates the text IDs used for every object: a type prefix,
// an underscore, then 26 Crockford base32 characters (48-bit millisecond
// timestamp + 80 random bits), so IDs sort by creation time.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// Prefixes for each object type.
const (
	App        = "app"
	APIKey     = "key"
	Asset      = "ast"
	Output     = "out"
	Job        = "job"
	Attempt    = "att"
	PlaybackID = "pid"
	Upload     = "upl"
	Agent      = "agt"
	Source     = "src"
	Event      = "evt"
	Webhook    = "whk"
	Storage    = "sto"
	SigningKey = "kid"
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz" // Crockford, lowercase

// New returns a new ID with the given prefix.
func New(prefix string) string { return newAt(prefix, time.Now()) }

func newAt(prefix string, t time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(t.UnixMilli())<<16)
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never returns an error.
	return prefix + "_" + encode(b)
}

// encode writes 128 bits as 26 base32 characters, most significant first.
func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
