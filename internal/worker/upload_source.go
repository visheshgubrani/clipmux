package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// uploadSource is an upload_source attempt (push --local --upload-source,
// D059): it sends the local file to storage in the parts the server signs,
// straight from where it lies, then checks the file didn't change while it
// was read. It returns the stored parts for complete.
func (w *Worker) uploadSource(ctx context.Context, job *workerapi.Job, report media.Progress) ([]workerapi.SourcePart, string, error) {
	if w.Local == nil {
		return nil, codeSourceMissing, fmt.Errorf("%w: this worker keeps no local sources", ErrSourceMissing)
	}
	path, err := w.Local.Resolve(job.Source.AgentSourceID, job.Source.Size)
	if err != nil {
		return nil, localCode(err), err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, codeSourceMissing, fmt.Errorf("%w: %v", ErrSourceMissing, err)
	}
	defer f.Close()

	report("upload_source", 0)
	// The first call, with no parts, starts the upload and says its shape.
	first, err := w.signParts(ctx, job, nil)
	if err != nil {
		return nil, codeUpload, err
	}
	var parts []workerapi.SourcePart
	for next := 1; next <= first.PartCount; {
		batch := make([]int, 0, workerapi.MaxSourceParts)
		for n := next; n <= first.PartCount && len(batch) < workerapi.MaxSourceParts; n++ {
			batch = append(batch, n)
		}
		signed, err := w.signParts(ctx, job, batch)
		if err != nil {
			return nil, codeUpload, err
		}
		for _, n := range batch {
			off := int64(n-1) * signed.PartSize
			size := min(signed.PartSize, job.Source.Size-off)
			etag, err := w.putPart(ctx, signed.URLs[strconv.Itoa(n)], io.NewSectionReader(f, off, size), size)
			if err != nil {
				return nil, codeUpload, fmt.Errorf("part %d: %w", n, err)
			}
			parts = append(parts, workerapi.SourcePart{Number: n, ETag: etag})
			report("upload_source", float64(n)/float64(first.PartCount))
		}
		next = batch[len(batch)-1] + 1
	}
	// A file edited while it was read would store a mix of two files.
	if _, err := w.Local.Resolve(job.Source.AgentSourceID, job.Source.Size); err != nil {
		return nil, localCode(err), err
	}
	return parts, "", nil
}

func (w *Worker) signParts(ctx context.Context, job *workerapi.Job, parts []int) (workerapi.SourcePartsResponse, error) {
	if parts == nil {
		parts = []int{}
	}
	var out workerapi.SourcePartsResponse
	status, err := w.post(ctx, "/worker/v1/attempts/"+job.AttemptID+"/source-parts", job.AttemptToken, map[string]any{"parts": parts}, &out)
	switch {
	case err != nil:
		return out, err
	case status != http.StatusOK:
		return out, fmt.Errorf("source-parts: HTTP %d", status)
	case out.Action == "stop":
		return out, errStopped
	case out.PartSize <= 0 || out.PartCount <= 0:
		return out, errors.New("source-parts: no part size")
	}
	return out, nil
}

// putPart PUTs one part, retrying network errors and 5xx, and returns
// its ETag. Part URLs aren't create-once: completing the upload names the
// parts, so a part written twice is the same bytes.
func (w *Worker) putPart(ctx context.Context, url string, body *io.SectionReader, size int64) (string, error) {
	if url == "" {
		return "", errors.New("no signed URL")
	}
	var last error
	for try := range 3 {
		if try > 0 {
			if err := sleep(ctx, time.Duration(try)*time.Second); err != nil {
				return "", err
			}
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		reqCtx, watch := idle(ctx, w.idleTimeout())
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodPut, w.storageURL(url), watch.reader(io.NopCloser(body)))
		req.ContentLength = size
		res, err := w.httpClient().Do(req)
		watch.stop()
		if err != nil {
			last = redact(err)
			continue
		}
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusOK && res.Header.Get("ETag") != "":
			return res.Header.Get("ETag"), nil
		case res.StatusCode >= 500:
			last = fmt.Errorf("PUT part: HTTP %d", res.StatusCode)
		default:
			return "", fmt.Errorf("PUT part: HTTP %d", res.StatusCode)
		}
	}
	return "", last
}
