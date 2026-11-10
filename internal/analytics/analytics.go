// Package analytics counts playback where delivery answers the viewer
// (D103): each counted response is an Event, and viewers are known only by
// a keyed hash of the token's vid.
package analytics

import (
	"crypto/hmac"
	"crypto/sha256"
)

// Kinds of counted response, named as the Worker's data points name them.
const (
	KindMaster        = "master"
	KindPlaylist      = "playlist"
	KindInit          = "init"
	KindSegment       = "segment"
	KindKey           = "key"
	KindPoster        = "poster"
	KindTrack         = "track"
	KindTrackPlaylist = "track_playlist"
)

// Event is one counted delivery response. Its numbers are additive.
type Event struct {
	ProjectID  string
	AppID      string
	AssetID    string
	PlaybackID string
	Kind       string
	// Bytes is the response body's length as sent.
	Bytes int64
	// Views is 1 for a master answered from its first byte.
	Views int64
	// WatchSeconds is media.SegmentSeconds for a segment of the watch-time
	// rendition (audio, or video when the output has none) answered from
	// its first byte.
	WatchSeconds int64
	// Vid is the token's viewer ID, or empty. It is never stored: the
	// recorder keeps only ViewerKey of it.
	Vid string
}

// ViewerKeySize is the length of a viewer key in bytes.
const ViewerKeySize = 16

// ViewerKey is how a viewer is stored: HMAC-SHA256 under the project's
// analytics key over the app ID, a zero byte and the vid, cut to 16 bytes.
// Without the key a stored viewer can't be matched to a vid, and the same
// vid under two apps is two viewers.
func ViewerKey(key []byte, appID, vid string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(appID))
	m.Write([]byte{0})
	m.Write([]byte(vid))
	return m.Sum(nil)[:ViewerKeySize]
}
