package media

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MediaPlaylist is what reading an output's media playlist needs: its init
// segment, and each segment with its IV and duration. Shaka writes one
// EXT-X-KEY for the whole playlist.
type MediaPlaylist struct {
	Init     string
	KeyURI   string
	Segments []PlaylistSegment
}

// PlaylistSegment is one media segment.
type PlaylistSegment struct {
	URI string
	// IV is the segment's AES-128 IV: the key line's IV, or its media
	// sequence number when the line names none.
	IV [16]byte
	// Duration is its EXTINF, in seconds.
	Duration float64
}

// ParseMediaPlaylist reads an encrypted fMP4 media playlist as Shaka writes
// them: EXT-X-MAP, one EXT-X-KEY (METHOD=AES-128), EXTINF and URIs.
func ParseMediaPlaylist(body string) (MediaPlaylist, error) {
	var p MediaPlaylist
	var iv []byte
	seq, duration := int64(0), -1.0
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			n, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				return MediaPlaylist{}, fmt.Errorf("media sequence: %w", err)
			}
			seq = n
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			p.Init = attr(line, "URI")
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			if m := attr(line, "METHOD"); m != "AES-128" {
				return MediaPlaylist{}, fmt.Errorf("key method %q, want AES-128", m)
			}
			p.KeyURI = attr(line, "URI")
			if v := attr(line, "IV"); v != "" {
				b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X"))
				if err != nil || len(b) != 16 {
					return MediaPlaylist{}, fmt.Errorf("key IV %q", v)
				}
				iv = b
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			v, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			d, err := strconv.ParseFloat(v, 64)
			if err != nil || d < 0 {
				return MediaPlaylist{}, fmt.Errorf("EXTINF %q", v)
			}
			duration = d
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if duration < 0 {
				return MediaPlaylist{}, fmt.Errorf("segment %q has no EXTINF", line)
			}
			s := PlaylistSegment{URI: line, Duration: duration}
			if iv != nil {
				copy(s.IV[:], iv)
			} else {
				// RFC 8216 5.2: the media sequence number, big-endian.
				for i, n := 15, seq+int64(len(p.Segments)); i >= 8; i-- {
					s.IV[i] = byte(n)
					n >>= 8
				}
			}
			p.Segments = append(p.Segments, s)
			duration = -1
		}
	}
	if p.Init == "" || p.KeyURI == "" || len(p.Segments) == 0 {
		return MediaPlaylist{}, errors.New("not an encrypted fMP4 media playlist")
	}
	return p, sc.Err()
}

// attr reads a playlist tag attribute, quoted or not.
func attr(line, name string) string {
	_, list, _ := strings.Cut(line, ":")
	for list != "" {
		k, rest, ok := strings.Cut(list, "=")
		if !ok {
			return ""
		}
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return ""
			}
			v, rest = rest[1:end+1], rest[end+2:]
		} else {
			v, rest, _ = strings.Cut(rest, ",")
			rest = "," + rest
		}
		if strings.TrimSpace(k) == name {
			return v
		}
		list = strings.TrimPrefix(rest, ",")
	}
	return ""
}

// DecryptSegment reverses Shaka's HLS AES-128: CBC over the whole segment
// with PKCS#7 padding.
func DecryptSegment(key [16]byte, iv [16]byte, seg []byte) ([]byte, error) {
	if len(seg) == 0 || len(seg)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("a %d-byte segment is not whole AES blocks", len(seg))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(seg))
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(plain, seg)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, errors.New("bad padding: wrong key, or not AES-128")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("bad padding: wrong key, or not AES-128")
		}
	}
	return plain[:len(plain)-pad], nil
}
