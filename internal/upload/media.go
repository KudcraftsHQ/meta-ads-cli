package upload

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
)

// Image uploads an image to an ad account's image library.
//
// Meta takes ad images as base64 in a form field rather than as multipart, and
// keys the response by filename, so the whole file has to be in memory. That is
// fine -- ad images are small, and Meta rejects anything over 30 MB anyway.
func Image(ctx context.Context, c *graph.Client, accountID string, src *Source) (json.RawMessage, error) {
	data, err := Buffer(src)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", src.Name, err)
	}
	return c.Do(ctx, graph.Request{
		Method: http.MethodPost,
		Path:   accountID + "/adimages",
		Params: map[string]any{
			"bytes":    base64.StdEncoding.EncodeToString(data),
			"filename": src.Name,
		},
	})
}

// VideoOptions tune a video upload.
type VideoOptions struct {
	// Name is the video's title in the ad account.
	Name string
	// ChunkSize caps how much is sent per transfer call. Meta dictates the
	// window through start/end offsets; this only bounds our buffer.
	ChunkSize int64
	// Wait polls until Meta finishes encoding before returning.
	Wait bool
	// WaitTimeout bounds that poll.
	WaitTimeout time.Duration
	// Progress, if set, is called after each transferred chunk.
	Progress func(sent, total int64)
}

const defaultChunkSize = 8 << 20 // 8 MiB

type videoSession struct {
	UploadSessionID string `json:"upload_session_id"`
	VideoID         string `json:"video_id"`
	StartOffset     string `json:"start_offset"`
	EndOffset       string `json:"end_offset"`
}

// Video performs Meta's three-phase chunked upload against graph-video: start
// declares the size and receives a session, transfer sends the byte window Meta
// asks for until the offsets converge, finish commits the session.
func Video(ctx context.Context, c *graph.Client, accountID string, src *Source, opts VideoOptions) (json.RawMessage, error) {
	defer src.Close()

	if opts.ChunkSize <= 0 {
		opts.ChunkSize = defaultChunkSize
	}
	if src.Size <= 0 {
		return nil, fmt.Errorf("video upload needs a known file size; %s did not report one", src.Name)
	}
	path := accountID + "/advideos"

	// Phase 1: start.
	raw, err := c.Do(ctx, graph.Request{
		Method: http.MethodPost,
		Path:   path,
		Video:  true,
		Params: map[string]any{
			"upload_phase": "start",
			"file_size":    src.Size,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("start upload session: %w", err)
	}
	if c.DryRun {
		return raw, nil
	}
	var session videoSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("parse upload session: %w", err)
	}

	// Phase 2: transfer. Meta names the byte range it wants next; we read that
	// much from the source and post it, then it names the next one.
	buf := make([]byte, 0, opts.ChunkSize)
	var sent int64
	for {
		start, err := strconv.ParseInt(session.StartOffset, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad start_offset %q: %w", session.StartOffset, err)
		}
		end, err := strconv.ParseInt(session.EndOffset, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad end_offset %q: %w", session.EndOffset, err)
		}
		if start >= end {
			break
		}

		want := end - start
		if want > opts.ChunkSize {
			want = opts.ChunkSize
		}
		if int64(cap(buf)) < want {
			buf = make([]byte, want)
		}
		chunk := buf[:want]
		if _, err := io.ReadFull(src, chunk); err != nil {
			return nil, fmt.Errorf("read chunk at offset %d: %w", start, err)
		}

		raw, err := c.Send(ctx, func() (*http.Request, error) {
			return multipartRequest(ctx, c, path, true, map[string]string{
				"upload_phase":      "transfer",
				"upload_session_id": session.UploadSessionID,
				"start_offset":      session.StartOffset,
			}, "video_file_chunk", src.Name, bytes.NewReader(chunk))
		})
		if err != nil {
			return nil, fmt.Errorf("transfer chunk at offset %d: %w", start, err)
		}
		var next videoSession
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, fmt.Errorf("parse transfer response: %w", err)
		}
		if next.StartOffset == session.StartOffset {
			return nil, fmt.Errorf("upload stalled: Meta returned the same offset %s twice", next.StartOffset)
		}
		session.StartOffset, session.EndOffset = next.StartOffset, next.EndOffset

		sent += want
		if opts.Progress != nil {
			opts.Progress(sent, src.Size)
		}
	}

	// Phase 3: finish.
	finishParams := map[string]any{
		"upload_phase":      "finish",
		"upload_session_id": session.UploadSessionID,
	}
	if opts.Name != "" {
		finishParams["title"] = opts.Name
		finishParams["name"] = opts.Name
	}
	if _, err := c.Do(ctx, graph.Request{
		Method: http.MethodPost,
		Path:   path,
		Video:  true,
		Params: finishParams,
	}); err != nil {
		return nil, fmt.Errorf("finish upload session: %w", err)
	}

	result := map[string]any{
		"id":                session.VideoID,
		"upload_session_id": session.UploadSessionID,
		"bytes":             src.Size,
	}

	if opts.Wait {
		status, err := WaitForVideo(ctx, c, session.VideoID, opts.WaitTimeout)
		if err != nil {
			return nil, err
		}
		result["status"] = status
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WaitForVideo polls a video until Meta finishes encoding it. Ads cannot be
// created against a video that is still processing, so --wait is what makes
// upload-then-create work as one script.
func WaitForVideo(ctx context.Context, c *graph.Client, videoID string, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for {
		raw, err := c.Do(ctx, graph.Request{
			Method: http.MethodGet,
			Path:   videoID,
			Params: map[string]any{"fields": "status"},
		})
		if err != nil {
			return nil, fmt.Errorf("poll video %s: %w", videoID, err)
		}
		var probe struct {
			Status map[string]any `json:"status"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("parse video status: %w", err)
		}

		phase, _ := probe.Status["video_status"].(string)
		switch strings.ToLower(phase) {
		case "ready":
			return probe.Status, nil
		case "error":
			return probe.Status, fmt.Errorf("video %s failed to process", videoID)
		}

		if time.Now().After(deadline) {
			return probe.Status, fmt.Errorf("video %s still %q after %s", videoID, phase, timeout)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if interval < 15*time.Second {
			interval += 2 * time.Second
		}
	}
}

// multipartRequest is defined here rather than in graph because it is only
// video upload that needs one.
func multipartRequest(ctx context.Context, c *graph.Client, path string, video bool,
	fields map[string]string, fileField, fileName string, body io.Reader) (*http.Request, error) {

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	values, err := c.Values(nil)
	if err != nil {
		return nil, err
	}
	for k := range values {
		if err := w.WriteField(k, values.Get(k)); err != nil {
			return nil, err
		}
	}
	part, err := w.CreateFormFile(fileField, fileName)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(path, video), bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, nil
}
