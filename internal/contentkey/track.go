package contentkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Tracks (subtitles, chapters) are stored encrypted like segments, so
// storage holds only ciphertext (docs/plans/P5-production.md, "Transcripts
// and WebVTT"). They use AES-128-GCM rather than HLS's CBC: delivery
// decrypts them (players never see a key), and GCM's tag with the storage
// key as associated data means a tampered or moved object can't inject cues.

// trackFormat is the sealed track file's first byte.
const trackFormat byte = 0x01

// trackHeader is the format byte, the secret's version and the nonce.
const trackHeader = 1 + 4 + 12

// ErrTrackSealed means a sealed track file couldn't be opened: corrupt,
// moved to another key, or sealed under a version this ring lacks.
var ErrTrackSealed = errors.New("contentkey: the track file doesn't open")

// DeriveTrack returns a track's key: the first 16 bytes of HMAC-SHA256 over
// the asset and track IDs, keyed with the secret. Each track has its own.
func (s Secret) DeriveTrack(assetID, trackID string) [16]byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("clipmux-track-key-v1\n" + assetID + "\n" + trackID))
	var k [16]byte
	copy(k[:], m.Sum(nil))
	return k
}

// SealTrack encrypts a track's file for storage at storageKey:
// 0x01 | version (u32, big-endian) | 12-byte nonce | ciphertext and tag,
// with the storage key as associated data.
func SealTrack(s Secret, assetID, trackID, storageKey string, plain []byte) ([]byte, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return sealTrack(s, assetID, trackID, storageKey, plain, nonce)
}

func sealTrack(s Secret, assetID, trackID, storageKey string, plain []byte, nonce [12]byte) ([]byte, error) {
	gcm, err := trackAEAD(s.DeriveTrack(assetID, trackID))
	if err != nil {
		return nil, err
	}
	out := make([]byte, trackHeader, trackHeader+len(plain)+gcm.Overhead())
	out[0] = trackFormat
	binary.BigEndian.PutUint32(out[1:5], uint32(s.Version))
	copy(out[5:trackHeader], nonce[:])
	return gcm.Seal(out, nonce[:], plain, []byte(storageKey)), nil
}

// TrackVersion reads the secret version a sealed track file names.
func TrackVersion(sealed []byte) (int32, error) {
	if len(sealed) < trackHeader+16 || sealed[0] != trackFormat {
		return 0, ErrTrackSealed
	}
	return int32(binary.BigEndian.Uint32(sealed[1:5])), nil
}

// OpenTrack decrypts a sealed track file stored at storageKey, with the
// version of the secret it names.
func (r Ring) OpenTrack(assetID, trackID, storageKey string, sealed []byte) ([]byte, error) {
	v, err := TrackVersion(sealed)
	if err != nil {
		return nil, err
	}
	s, ok := r.Get(v)
	if !ok {
		return nil, fmt.Errorf("%w: no secret version %d", ErrTrackSealed, v)
	}
	gcm, err := trackAEAD(s.DeriveTrack(assetID, trackID))
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, sealed[5:trackHeader], sealed[trackHeader:], []byte(storageKey))
	if err != nil {
		return nil, ErrTrackSealed
	}
	return plain, nil
}

func trackAEAD(key [16]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
