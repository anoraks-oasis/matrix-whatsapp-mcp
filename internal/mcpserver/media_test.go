package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
)

// makePNG builds a valid PNG of the given size with a cheap gradient, so a test
// can exercise real decoding and scaling without a fixture file.
func makePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for x := 0; x < width; x++ {
			row[x*4] = uint8(x % 256)
			row[x*4+1] = uint8(y % 256)
			row[x*4+2] = 0x40
			row[x*4+3] = 0xff
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// mediaTextBlock parses the metadata text block get_media returns first.
func mediaTextBlock(t *testing.T, res *mcp.CallToolResult) getMediaResult {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("get_media returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content block is %T, want *mcp.TextContent", res.Content[0])
	}
	var meta getMediaResult
	if err := json.Unmarshal([]byte(text.Text), &meta); err != nil {
		t.Fatalf("metadata text block is not JSON: %v (%s)", err, text.Text)
	}
	return meta
}

// imageBlockData returns the bytes an image content block carries on the wire,
// which is where the base64 requirement lives: the block marshals its payload
// as base64 and a client decodes it back.
func imageBlockData(t *testing.T, content mcp.Content) ([]byte, string) {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal image block: %v", err)
	}
	var wire struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MIMEType string `json:"mimeType"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal image block: %v", err)
	}
	if wire.Type != "image" {
		t.Fatalf("content block type = %q, want image", wire.Type)
	}
	if wire.Data == "" {
		t.Fatal("image block carries no data")
	}
	data, err := base64.StdEncoding.DecodeString(wire.Data)
	if err != nil {
		t.Fatalf("image block data is not base64: %v", err)
	}
	return data, wire.MIMEType
}

func TestGetMediaReturnsImageBlock(t *testing.T) {
	payload := makePNG(t, 40, 30)
	svc := &fakeMatrix{mediaAtt: matrix.MediaAttachment{
		MimeType: "image/png",
		Filename: "pic.png",
		MXCURI:   "mxc://test/pic",
		Data:     payload,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_media",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$img"},
	})
	if err != nil {
		t.Fatalf("get_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_media returned error: %s", resultErrorText(t, res))
	}
	if len(res.Content) != 2 {
		t.Fatalf("get_media returned %d content blocks, want text plus image", len(res.Content))
	}
	meta := mediaTextBlock(t, res)
	if !meta.Included || meta.Reason != "" {
		t.Fatalf("metadata = %+v, want included true and no reason", meta)
	}
	if meta.Mimetype != "image/png" || meta.Filename != "pic.png" || meta.MediaURI != "mxc://test/pic" {
		t.Fatalf("metadata = %+v, want the attachment's type, name and media URI", meta)
	}
	if meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("metadata size_bytes = %d, want %d", meta.SizeBytes, len(payload))
	}
	if meta.Width != 40 || meta.Height != 30 || meta.Downscaled {
		t.Fatalf("metadata = %+v, want 40x30 and downscaled false", meta)
	}

	data, mimeType := imageBlockData(t, res.Content[1])
	if mimeType != "image/png" {
		t.Fatalf("image block mimeType = %q, want image/png", mimeType)
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image block payload does not decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("image block payload format = %q, want png", format)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 40 || bounds.Dy() != 30 {
		t.Fatalf("image block is %dx%d, want 40x30", bounds.Dx(), bounds.Dy())
	}

	structured := decodeStructured[getMediaResult](t, res)
	if structured.Mimetype != "image/png" || !structured.Included {
		t.Fatalf("structured content = %+v, want the same metadata as the text block", structured)
	}
}

func TestGetMediaDownscalesToMaxDimension(t *testing.T) {
	payload := makePNG(t, 4000, 3000)
	svc := &fakeMatrix{mediaAtt: matrix.MediaAttachment{
		MimeType: "image/png",
		MXCURI:   "mxc://test/big",
		Data:     payload,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_media",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$img", "max_dimension": 512},
	})
	if err != nil {
		t.Fatalf("get_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_media returned error: %s", resultErrorText(t, res))
	}
	if len(res.Content) != 2 {
		t.Fatalf("get_media returned %d content blocks, want text plus image", len(res.Content))
	}
	meta := mediaTextBlock(t, res)
	if !meta.Downscaled {
		t.Fatalf("metadata = %+v, want downscaled true", meta)
	}
	if meta.Width != 512 || meta.Height != 384 {
		t.Fatalf("metadata dimensions = %dx%d, want 512x384", meta.Width, meta.Height)
	}
	data, _ := imageBlockData(t, res.Content[1])
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("downscaled payload does not decode: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != 512 || bounds.Dy() != 384 {
		t.Fatalf("downscaled image is %dx%d, want 512x384", bounds.Dx(), bounds.Dy())
	}
	if longEdge := max(bounds.Dx(), bounds.Dy()); longEdge > 512 {
		t.Fatalf("downscaled long edge = %d, want at most 512", longEdge)
	}
}

func TestGetMediaOverBudgetReturnsMetadataOnly(t *testing.T) {
	payload := makePNG(t, 40, 30)
	svc := &fakeMatrix{mediaAtt: matrix.MediaAttachment{
		MimeType: "image/png",
		MXCURI:   "mxc://test/pic",
		Data:     payload,
	}}
	// A budget no real image can fit, so the re-encoder runs out of dimensions
	// instead of the test needing a megabyte fixture.
	srv := newServerWithBudget(svc, &fakeProber{}, &fakeRecent{}, nil, newIdempotencyCache(), 64)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_media",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$img"},
	})
	if err != nil {
		t.Fatalf("get_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_media returned error: %s", resultErrorText(t, res))
	}
	if len(res.Content) != 1 {
		t.Fatalf("get_media returned %d content blocks, want text only", len(res.Content))
	}
	meta := mediaTextBlock(t, res)
	if meta.Included {
		t.Fatalf("metadata = %+v, want included false", meta)
	}
	if !strings.Contains(meta.Reason, "budget") {
		t.Fatalf("reason = %q, want it to explain the inline budget", meta.Reason)
	}
	if meta.Mimetype != "image/png" || meta.MediaURI != "mxc://test/pic" || meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("metadata = %+v, want the attachment's metadata even when the payload is dropped", meta)
	}
	if meta.Width != 40 || meta.Height != 30 || meta.Downscaled {
		t.Fatalf("metadata = %+v, want the source dimensions when no image block is returned", meta)
	}
}

func TestGetMediaAudioBecomesAudioBlock(t *testing.T) {
	payload := []byte("OggS-fake-audio")
	svc := &fakeMatrix{mediaAtt: matrix.MediaAttachment{
		MimeType:   "audio/ogg",
		DurationMS: 4200,
		MXCURI:     "mxc://test/voice",
		Data:       payload,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_media",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$audio"},
	})
	if err != nil {
		t.Fatalf("get_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_media returned error: %s", resultErrorText(t, res))
	}
	if len(res.Content) != 2 {
		t.Fatalf("get_media returned %d content blocks, want text plus audio", len(res.Content))
	}
	audio, ok := res.Content[1].(*mcp.AudioContent)
	if !ok {
		t.Fatalf("second content block is %T, want *mcp.AudioContent", res.Content[1])
	}
	if audio.MIMEType != "audio/ogg" || !bytes.Equal(audio.Data, payload) {
		t.Fatalf("audio block = mime %q, %d bytes; want audio/ogg and the payload", audio.MIMEType, len(audio.Data))
	}
	meta := mediaTextBlock(t, res)
	if !meta.Included || meta.DurationMS != 4200 {
		t.Fatalf("metadata = %+v, want included true and the duration", meta)
	}
}

func TestGetMediaNonImageReturnsMetadataOnly(t *testing.T) {
	svc := &fakeMatrix{mediaAtt: matrix.MediaAttachment{
		MimeType: "application/pdf",
		Filename: "report.pdf",
		MXCURI:   "mxc://test/doc",
		Data:     []byte("%PDF-1.7 ok"),
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_media",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$doc"},
	})
	if err != nil {
		t.Fatalf("get_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_media returned error: %s", resultErrorText(t, res))
	}
	if len(res.Content) != 1 {
		t.Fatalf("get_media returned %d content blocks, want text only", len(res.Content))
	}
	meta := mediaTextBlock(t, res)
	if meta.Included || meta.Reason == "" {
		t.Fatalf("metadata = %+v, want included false with a reason", meta)
	}
	if !strings.Contains(meta.Reason, "metadata") {
		t.Fatalf("reason = %q, want it to say only metadata is returned", meta.Reason)
	}
}

func TestGetMediaTypedErrors(t *testing.T) {
	cases := []struct {
		name    string
		att     matrix.MediaAttachment
		err     error
		wantMsg string
	}{
		{
			name:    "not media",
			err:     matrix.ErrNotMedia,
			wantMsg: "no media attachment",
		},
		{
			name:    "unknown event",
			err:     matrix.ErrEventNotFound,
			wantMsg: "event not found",
		},
		{
			name:    "download failure",
			err:     matrix.ErrMediaDownload,
			wantMsg: "download failed",
		},
		{
			name:    "too large",
			err:     matrix.ErrMediaTooLarge,
			wantMsg: "larger than the download limit",
		},
		{
			name:    "encrypted without e2ee",
			err:     matrix.ErrEncryptedMediaNoE2EE,
			wantMsg: "MATRIX_E2EE_ENABLED",
		},
		{
			name:    "unsupported codec",
			att:     matrix.MediaAttachment{MimeType: "image/webp", MXCURI: "mxc://test/webp", Data: []byte("not a webp")},
			wantMsg: "unsupported media codec",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeMatrix{mediaAtt: tc.att, mediaErr: tc.err}
			srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
			session, cleanup := connect(t, srv)
			defer cleanup()

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "get_media",
				Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$img"},
			})
			if err != nil {
				t.Fatalf("get_media: %v", err)
			}
			text := resultErrorText(t, res)
			if !strings.Contains(text, tc.wantMsg) {
				t.Fatalf("error = %q, want it to contain %q", text, tc.wantMsg)
			}
		})
	}
}

func TestGetMediaRejectsMalformedEventIDBeforeFetching(t *testing.T) {
	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_media",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "not-an-event"},
	})
	if err != nil {
		t.Fatalf("get_media: %v", err)
	}
	if text := resultErrorText(t, res); !strings.Contains(text, "invalid event ID") {
		t.Fatalf("error = %q, want invalid event ID", text)
	}
	rooms, _ := svc.mediaCalls()
	if len(rooms) != 0 {
		t.Fatalf("malformed event ID reached the Matrix layer %d times", len(rooms))
	}
}

func TestGetMediaClampsMaxDimension(t *testing.T) {
	payload := makePNG(t, 2000, 1000)
	svc := &fakeMatrix{mediaAtt: matrix.MediaAttachment{
		MimeType: "image/png", MXCURI: "mxc://test/wide", Data: payload,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	call := func(maxDimension any) getMediaResult {
		t.Helper()
		args := map[string]any{"chat_id": "!r:test", "message_id": "$img"}
		if maxDimension != nil {
			args["max_dimension"] = maxDimension
		}
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_media", Arguments: args})
		if err != nil {
			t.Fatalf("get_media: %v", err)
		}
		if res.IsError {
			t.Fatalf("get_media returned error: %s", resultErrorText(t, res))
		}
		return mediaTextBlock(t, res)
	}

	if meta := call(nil); meta.Width != 1568 || meta.Height != 784 {
		t.Fatalf("default max_dimension returned %dx%d, want 1568x784", meta.Width, meta.Height)
	}
	if meta := call(99999); meta.Width != 2000 || meta.Downscaled {
		t.Fatalf("max_dimension 99999 returned %+v, want the image untouched (the clamp is 4096)", meta)
	}
	if meta := call(1); meta.Width != 256 || meta.Height != 128 {
		t.Fatalf("max_dimension 1 returned %dx%d, want the 256 floor", meta.Width, meta.Height)
	}
}

// writeTempFile writes payload to a fresh file with the given name and returns
// its absolute path.
func writeTempFile(t *testing.T, name string, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	return path
}

func TestSendMediaFromPath(t *testing.T) {
	payload := makePNG(t, 5, 4)
	path := writeTempFile(t, "photo.png", payload)
	svc := &fakeMatrix{mediaSendID: "$mediaSent"}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "send_media",
		Arguments: map[string]any{
			"chat_id":  "!ok:test",
			"path":     path,
			"caption":  "look",
			"reply_to": "$parent",
		},
	})
	if err != nil {
		t.Fatalf("send_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("send_media returned error: %s", resultErrorText(t, res))
	}
	got := decodeStructured[sendMediaResult](t, res)
	if got.MessageID != "$mediaSent" || got.Deduplicated || got.Type != "image" || got.MediaURI != "mxc://test/uploaded" {
		t.Fatalf("send_media result = %+v, want $mediaSent as image at the uploaded media URI", got)
	}

	sends := svc.mediaSent()
	if len(sends) != 1 {
		t.Fatalf("Matrix layer saw %d media sends, want 1", len(sends))
	}
	call := sends[0]
	if call.roomID != "!ok:test" || call.caption != "look" || call.replyTo != "$parent" {
		t.Fatalf("send arguments = %+v, want the room, caption and reply target", call)
	}
	if !bytes.Equal(call.media.Data, payload) {
		t.Fatalf("send payload is %d bytes, want the file's %d", len(call.media.Data), len(payload))
	}
	if call.media.MimeType != "image/png" {
		t.Fatalf("send mimetype = %q, want image/png detected from the extension", call.media.MimeType)
	}
	if call.media.Filename != "photo.png" {
		t.Fatalf("send filename = %q, want the file's base name", call.media.Filename)
	}
}

func TestSendMediaFromBase64(t *testing.T) {
	payload := []byte("RIFF-fake-wav")
	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "send_media",
		Arguments: map[string]any{
			"chat_id":     "!ok:test",
			"data_base64": base64.StdEncoding.EncodeToString(payload),
			"mimetype":    "audio/wav",
			"filename":    "note.wav",
		},
	})
	if err != nil {
		t.Fatalf("send_media: %v", err)
	}
	if res.IsError {
		t.Fatalf("send_media returned error: %s", resultErrorText(t, res))
	}
	got := decodeStructured[sendMediaResult](t, res)
	if got.Type != "audio" {
		t.Fatalf("type = %q, want audio", got.Type)
	}
	sends := svc.mediaSent()
	if len(sends) != 1 || !bytes.Equal(sends[0].media.Data, payload) {
		t.Fatalf("Matrix layer saw %+v, want the decoded base64 payload", sends)
	}
	if sends[0].media.Filename != "note.wav" || sends[0].media.MimeType != "audio/wav" {
		t.Fatalf("upload = %+v, want the filename and mimetype", sends[0].media)
	}
}

func TestSendMediaRejectsBadPayloadSources(t *testing.T) {
	dir := t.TempDir()
	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	cases := []struct {
		name    string
		args    map[string]any
		wantMsg string
	}{
		{
			name:    "neither source",
			args:    map[string]any{"chat_id": "!ok:test"},
			wantMsg: "pass one of path or data_base64",
		},
		{
			name:    "both sources",
			args:    map[string]any{"chat_id": "!ok:test", "path": "/tmp/x", "data_base64": "aGk=", "mimetype": "image/png"},
			wantMsg: "exactly one of path and data_base64",
		},
		{
			name:    "relative path",
			args:    map[string]any{"chat_id": "!ok:test", "path": "photo.png"},
			wantMsg: "path must be absolute",
		},
		{
			name:    "missing file",
			args:    map[string]any{"chat_id": "!ok:test", "path": filepath.Join(dir, "nope.png")},
			wantMsg: "path is not readable",
		},
		{
			name:    "directory",
			args:    map[string]any{"chat_id": "!ok:test", "path": dir},
			wantMsg: "is a directory",
		},
		{
			name:    "base64 without mimetype",
			args:    map[string]any{"chat_id": "!ok:test", "data_base64": "aGk="},
			wantMsg: "mimetype is required",
		},
		{
			name:    "invalid base64",
			args:    map[string]any{"chat_id": "!ok:test", "data_base64": "not base64!", "mimetype": "image/png"},
			wantMsg: "not valid standard base64",
		},
		{
			name:    "malformed reply",
			args:    map[string]any{"chat_id": "!ok:test", "data_base64": "aGk=", "mimetype": "image/png", "reply_to": "nope"},
			wantMsg: "invalid event ID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_media", Arguments: tc.args})
			if err != nil {
				t.Fatalf("send_media: %v", err)
			}
			if text := resultErrorText(t, res); !strings.Contains(text, tc.wantMsg) {
				t.Fatalf("error = %q, want it to contain %q", text, tc.wantMsg)
			}
		})
	}
	if sends := svc.mediaSent(); len(sends) != 0 {
		t.Fatalf("invalid payloads reached the Matrix layer %d times", len(sends))
	}
}

func TestSendMediaRefusesOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	// A sparse file: the size check must reject it without reading 50 MiB.
	if err := file.Truncate(maxMediaUploadBytes + 1); err != nil {
		file.Close()
		t.Fatalf("truncate fixture: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_media",
		Arguments: map[string]any{"chat_id": "!ok:test", "path": path},
	})
	if err != nil {
		t.Fatalf("send_media: %v", err)
	}
	if text := resultErrorText(t, res); !strings.Contains(text, "50 MiB") {
		t.Fatalf("error = %q, want the 50 MiB upload cap", text)
	}
	if sends := svc.mediaSent(); len(sends) != 0 {
		t.Fatalf("oversized file reached the Matrix layer %d times", len(sends))
	}
}

func TestSendMediaAllowlistRejectsWithZeroUploads(t *testing.T) {
	for name, allowed := range map[string][]string{"empty allowlist": nil, "other room": {"!elsewhere:test"}} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeMatrix{}
			srv := New(svc, &fakeProber{}, &fakeRecent{}, allowed)
			session, cleanup := connect(t, srv)
			defer cleanup()

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "send_media",
				Arguments: map[string]any{"chat_id": "!ok:test", "data_base64": "aGk=", "mimetype": "image/png"},
			})
			if err != nil {
				t.Fatalf("send_media: %v", err)
			}
			text := resultErrorText(t, res)
			want := "not allowed"
			if allowed == nil {
				want = "read-only"
			}
			if !strings.Contains(text, want) {
				t.Fatalf("error = %q, want %q", text, want)
			}
			if sends := svc.mediaSent(); len(sends) != 0 {
				t.Fatalf("rejected send reached the Matrix layer %d times", len(sends))
			}
		})
	}
}

func TestSendMediaRateLimited(t *testing.T) {
	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	args := map[string]any{"chat_id": "!ok:test", "data_base64": "aGk=", "mimetype": "image/png"}
	first, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_media", Arguments: args})
	if err != nil {
		t.Fatalf("first send_media: %v", err)
	}
	if first.IsError {
		t.Fatalf("first send_media rejected: %s", resultErrorText(t, first))
	}
	second, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_media", Arguments: args})
	if err != nil {
		t.Fatalf("second send_media: %v", err)
	}
	if text := resultErrorText(t, second); !strings.Contains(text, "rate limited") {
		t.Fatalf("second send_media error = %q, want rate limited", text)
	}
	if sends := svc.mediaSent(); len(sends) != 1 {
		t.Fatalf("Matrix layer saw %d sends, want exactly 1", len(sends))
	}
}

func TestSendMediaIdempotentReplaySkipsUpload(t *testing.T) {
	svc := &fakeMatrix{mediaSendID: "$once"}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	args := map[string]any{
		"chat_id":         "!ok:test",
		"data_base64":     "aGVsbG8=",
		"mimetype":        "text/plain",
		"caption":         "hi",
		"idempotency_key": "media-1",
	}
	first, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_media", Arguments: args})
	if err != nil {
		t.Fatalf("first send_media: %v", err)
	}
	if first.IsError {
		t.Fatalf("first send_media rejected: %s", resultErrorText(t, first))
	}
	second, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_media", Arguments: args})
	if err != nil {
		t.Fatalf("replay send_media: %v", err)
	}
	if second.IsError {
		t.Fatalf("replay rejected: %s", resultErrorText(t, second))
	}
	replay := decodeStructured[sendMediaResult](t, second)
	if replay.MessageID != "$once" || !replay.Deduplicated {
		t.Fatalf("replay = %+v, want $once with deduplicated true", replay)
	}
	if sends := svc.mediaSent(); len(sends) != 1 {
		t.Fatalf("Matrix layer saw %d sends, want 1: a replay must not upload", len(sends))
	}

	// The same key with different bytes is a conflict, not a second send.
	changed := map[string]any{
		"chat_id":         "!ok:test",
		"data_base64":     "ZGlmZmVyZW50",
		"mimetype":        "text/plain",
		"caption":         "hi",
		"idempotency_key": "media-1",
	}
	conflict, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_media", Arguments: changed})
	if err != nil {
		t.Fatalf("conflicting send_media: %v", err)
	}
	if text := resultErrorText(t, conflict); !strings.Contains(text, "different message") {
		t.Fatalf("conflict error = %q, want the idempotency conflict", text)
	}
	if sends := svc.mediaSent(); len(sends) != 1 {
		t.Fatalf("conflict reached the Matrix layer: %d sends", len(sends))
	}
}

func TestSendMediaSurfacesUpstreamError(t *testing.T) {
	svc := &fakeMatrix{mediaSendErr: matrix.ErrNotBridged}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_media",
		Arguments: map[string]any{"chat_id": "!ok:test", "data_base64": "aGk=", "mimetype": "image/png"},
	})
	if err != nil {
		t.Fatalf("send_media: %v", err)
	}
	if text := resultErrorText(t, res); !strings.Contains(text, "bridge did not bridge") {
		t.Fatalf("error = %q, want the bridge rejection to surface", text)
	}
}
