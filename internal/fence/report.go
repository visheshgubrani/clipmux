package fence

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// Report is what storage holds that the database doesn't know: the videos
// and re-encodes made after the backup that was restored. None of it is
// deleted while the fence is up.
type Report struct {
	// Routes are route records for playback IDs the database has no row for.
	Routes  []RouteFinding
	Outputs []OutputFinding
	Sources []SourceFinding
}

// RouteFinding is a _routes/{playback_id}.json no playback ID row matches.
type RouteFinding struct {
	Store      string
	PlaybackID string
	AssetID    string
	OutputID   string
}

// OutputFinding is a media/{asset}/{output}/ prefix no output row matches.
type OutputFinding struct {
	Store    string
	AssetID  string
	OutputID string
	// AssetKnown: the asset has a row, so this is a re-encode made after the
	// backup; otherwise the whole asset is newer.
	AssetKnown bool
	// HasManifest: the output was committed (its manifest.json is there).
	HasManifest bool
	Files       int
	Bytes       int64
}

// SourceFinding is an uploads/{asset}/source no asset row matches.
type SourceFinding struct {
	Store   string
	AssetID string
	Bytes   int64
}

// Empty reports whether storage holds nothing the database doesn't know.
func (r Report) Empty() bool { return len(r.Routes)+len(r.Outputs)+len(r.Sources) == 0 }

// Report lists what every store of the project holds that the database
// doesn't know, from the stores' listings and their route records. A store it
// can't read is an error, returned with what the others showed.
func (e *Epoch) Report(ctx context.Context) (Report, error) {
	q := db.New(e.Pool)
	known := map[string]map[string]bool{}
	for name, list := range map[string]func(context.Context, string) ([]string, error){
		"assets": q.ListAssetIDs, "outputs": q.ListOutputIDs, "playback": q.ListPlaybackIDStrings,
	} {
		ids, err := list(ctx, e.ProjectID)
		if err != nil {
			return Report{}, fmt.Errorf("list %s: %w", name, err)
		}
		known[name] = map[string]bool{}
		for _, id := range ids {
			known[name][id] = true
		}
	}
	states, errs := e.read(ctx)
	var rep Report
	for _, s := range states {
		var err error
		if rep.Routes, err = routeFindings(ctx, s, known["playback"], rep.Routes); err != nil {
			errs = append(errs, fmt.Errorf("store %s: routes: %w", s.id, err))
		}
		if rep.Outputs, err = outputFindings(ctx, s, known["assets"], known["outputs"], rep.Outputs); err != nil {
			errs = append(errs, fmt.Errorf("store %s: outputs: %w", s.id, err))
		}
		if rep.Sources, err = sourceFindings(ctx, s, known["assets"], rep.Sources); err != nil {
			errs = append(errs, fmt.Errorf("store %s: sources: %w", s.id, err))
		}
	}
	slices.SortFunc(rep.Routes, func(a, b RouteFinding) int { return strings.Compare(a.Store+a.PlaybackID, b.Store+b.PlaybackID) })
	slices.SortFunc(rep.Outputs, func(a, b OutputFinding) int {
		return strings.Compare(a.Store+a.AssetID+a.OutputID, b.Store+b.AssetID+b.OutputID)
	})
	slices.SortFunc(rep.Sources, func(a, b SourceFinding) int { return strings.Compare(a.Store+a.AssetID, b.Store+b.AssetID) })
	return rep, errors.Join(errs...)
}

// walk calls fn with every object under prefix.
func walk(ctx context.Context, st storage.Storage, prefix string, fn func(storage.Object) error) error {
	for cursor := ""; ; {
		objs, next, err := st.List(ctx, prefix, cursor)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range objs {
			if err := fn(o); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

func routeFindings(ctx context.Context, s state, playback map[string]bool, out []RouteFinding) ([]RouteFinding, error) {
	err := walk(ctx, s.store, "_routes/", func(o storage.Object) error {
		pid, ok := routeID(o.Key)
		if !ok || playback[pid] {
			return nil
		}
		f := RouteFinding{Store: s.id, PlaybackID: pid}
		// The record says which asset it was for; one that can't be read is
		// still listed.
		if rc, err := s.store.Get(ctx, o.Key); err == nil {
			if r, err := decodeRecord(rc); err == nil {
				f.AssetID, f.OutputID = r.AssetID, r.OutputID
			}
			rc.Close()
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

func outputFindings(ctx context.Context, s state, assets, outputs map[string]bool, out []OutputFinding) ([]OutputFinding, error) {
	found := map[string]*OutputFinding{}
	err := walk(ctx, s.store, "media/", func(o storage.Object) error {
		// media/{asset}/{output}/{file}; tracks and other files sit beside
		// the outputs.
		parts := strings.SplitN(o.Key, "/", 4)
		if len(parts) < 4 || !strings.HasPrefix(parts[2], "out_") || outputs[parts[2]] {
			return nil
		}
		key := parts[1] + "/" + parts[2]
		f := found[key]
		if f == nil {
			f = &OutputFinding{Store: s.id, AssetID: parts[1], OutputID: parts[2], AssetKnown: assets[parts[1]]}
			found[key] = f
		}
		f.Files++
		f.Bytes += o.Size
		if parts[3] == "manifest.json" {
			f.HasManifest = true
		}
		return nil
	})
	for _, f := range found {
		out = append(out, *f)
	}
	return out, err
}

func sourceFindings(ctx context.Context, s state, assets map[string]bool, out []SourceFinding) ([]SourceFinding, error) {
	err := walk(ctx, s.store, "uploads/", func(o storage.Object) error {
		asset, file, ok := strings.Cut(strings.TrimPrefix(o.Key, "uploads/"), "/")
		if ok && file == "source" && !assets[asset] {
			out = append(out, SourceFinding{Store: s.id, AssetID: asset, Bytes: o.Size})
		}
		return nil
	})
	return out, err
}
