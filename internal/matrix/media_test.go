package matrix

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// pngBytes builds a tiny valid PNG, so a test can assert real dimensions
// without carrying a fixture file.
func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// mediaEventJSON is one m.room.message body pointing at mxc://test/media1.
func mediaEventJSON(msgtype, body string, extra string) string {
	return `{"type":"m.room.message","event_id":"$img","sender":"@alice:test","origin_server_ts":1000,` +
		`"room_id":"!r:test","content":{"msgtype":"` + msgtype + `","body":"` + body + `","url":"mxc://test/media1"` + extra + `}}`
}

func TestMessagesCarryMediaMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"chunk":[`+
			`{"type":"m.room.message","event_id":"$img","sender":"@alice:test","origin_server_ts":1000,"content":{"msgtype":"m.image","body":"photo.jpg","filename":"photo.jpg","url":"mxc://test/media1","info":{"mimetype":"image/jpeg","size":1234,"w":800,"h":600}}},`+
			`{"type":"m.room.message","event_id":"$audio","sender":"@alice:test","origin_server_ts":2000,"content":{"msgtype":"m.audio","body":"voice note","file":{"url":"mxc://test/media2","key":{"k":"aGFyZ2VseXNlY3JldGtleWZvcnRlc3RzMDAwMDAwMDAwMDAwMA","alg":"A256CTR","ext":true,"kty":"oct","key_ops":["encrypt","decrypt"]},"iv":"aXYtdGVzdC1pdi0xMjM0NTY3OA","hashes":{"sha256":"aGFzaGZvcnRlc3RzMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA"},"v":"v2"},"info":{"mimetype":"audio/ogg","size":9999,"duration":4200}}},`+
			`{"type":"m.room.message","event_id":"$text","sender":"@bob:test","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"just words"}}`+
			`]}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.Messages(context.Background(), "!r:test", 20, "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("Messages returned %d, want 3", len(page.Messages))
	}

	img := page.Messages[0]
	if img.MimeType != "image/jpeg" || img.SizeBytes != 1234 || img.Width != 800 || img.Height != 600 {
		t.Fatalf("image metadata = %+v, want mimetype image/jpeg, 1234 bytes, 800x600", img)
	}
	if img.Filename != "photo.jpg" || img.MXCURI != "mxc://test/media1" || img.DurationMS != 0 {
		t.Fatalf("image attachment = %+v, want filename photo.jpg and the plaintext mxc URI", img)
	}
	if img.Body != "photo.jpg" {
		t.Fatalf("image body = %q, want the caption untouched", img.Body)
	}

	audio := page.Messages[1]
	if audio.MimeType != "audio/ogg" || audio.SizeBytes != 9999 || audio.DurationMS != 4200 {
		t.Fatalf("audio metadata = %+v, want mimetype, size and duration", audio)
	}
	if audio.MXCURI != "mxc://test/media2" || audio.Width != 0 {
		t.Fatalf("audio attachment = %+v, want the file URL as the mxc URI and no width", audio)
	}

	text := page.Messages[2]
	if text.MimeType != "" || text.Filename != "" || text.SizeBytes != 0 || text.MXCURI != "" ||
		text.Width != 0 || text.Height != 0 || text.DurationMS != 0 {
		t.Fatalf("text message carries media metadata: %+v", text)
	}
}

func TestMediaDownloadsPlainMedia(t *testing.T) {
	payload := pngBytes(t, 8, 6)
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/rooms/!r:test/event/$img"):
			// No info.mimetype: the type must come from the download response.
			io.WriteString(w, `{"type":"m.room.message","event_id":"$img","sender":"@alice:test","origin_server_ts":1000,`+
				`"room_id":"!r:test","content":{"msgtype":"m.image","body":"photo.png","url":"mxc://test/media1","info":{"size":123}}}`)
		case strings.Contains(r.URL.Path, "/media/download/test/media1"):
			downloads.Add(1)
			w.Header().Set("Content-Type", "image/png")
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	att, err := c.Media(context.Background(), "!r:test", "$img")
	if err != nil {
		t.Fatalf("Media: %v", err)
	}
	if downloads.Load() != 1 {
		t.Fatalf("download requests = %d, want 1", downloads.Load())
	}
	if !bytes.Equal(att.Data, payload) {
		t.Fatalf("Media returned %d bytes, want the served payload", len(att.Data))
	}
	if att.MimeType != "image/png" {
		t.Fatalf("Media mimetype = %q, want image/png from the response header", att.MimeType)
	}
	if att.MXCURI != "mxc://test/media1" || att.Encrypted {
		t.Fatalf("Media attachment = %+v, want the plaintext mxc URI and encrypted false", att)
	}
}

func TestMediaRejectsNonMediaEvent(t *testing.T) {
	events := map[string]string{
		"text": `{"type":"m.room.message","event_id":"$t","sender":"@a:test","origin_server_ts":1,` +
			`"content":{"msgtype":"m.text","body":"hello"}}`,
		"membership": `{"type":"m.room.member","event_id":"$m","sender":"@a:test","origin_server_ts":1,` +
			`"state_key":"@b:test","content":{"membership":"join"}}`,
	}
	for name, body := range events {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/event/") {
					io.WriteString(w, body)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			c, err := New(testConfig(srv.URL), testLogger())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Media(context.Background(), "!r:test", "$e"); !errors.Is(err, ErrNotMedia) {
				t.Fatalf("Media(%s event) = %v, want ErrNotMedia", name, err)
			}
		})
	}
}

func TestMediaUnknownEventIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Event not found"}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Media(context.Background(), "!r:test", "$missing")
	if !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("Media(unknown event) = %v, want ErrEventNotFound", err)
	}
	if !strings.Contains(err.Error(), "$missing") {
		t.Fatalf("error = %q, want it to name the event", err)
	}
}

func TestMediaDownloadFailureIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/event/"):
			io.WriteString(w, mediaEventJSON("m.file", "doc.pdf", `,"info":{"mimetype":"application/pdf","size":10}`))
		case strings.Contains(r.URL.Path, "/media/download/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Media(context.Background(), "!r:test", "$img"); !errors.Is(err, ErrMediaDownload) {
		t.Fatalf("Media(download 500) = %v, want ErrMediaDownload", err)
	}
}

func TestMediaRefusesOversizedDownload(t *testing.T) {
	t.Run("declared length", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/event/"):
				io.WriteString(w, mediaEventJSON("m.file", "huge.bin", ""))
			case strings.Contains(r.URL.Path, "/media/download/"):
				w.Header().Set("Content-Length", "26214401")
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Media(context.Background(), "!r:test", "$img"); !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("Media(declared 25MiB+1) = %v, want ErrMediaTooLarge", err)
		}
	})

	t.Run("undeclared length", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/event/"):
				io.WriteString(w, mediaEventJSON("m.file", "huge.bin", ""))
			case strings.Contains(r.URL.Path, "/media/download/"):
				// A chunked response with no Content-Length: the limit has to
				// be enforced while reading.
				w.WriteHeader(http.StatusOK)
				chunk := bytes.Repeat([]byte{0x41}, 1<<20)
				for written := 0; written <= MaxMediaDownloadBytes; written += len(chunk) {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Media(context.Background(), "!r:test", "$img"); !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("Media(streamed 25MiB+) = %v, want ErrMediaTooLarge", err)
		}
	})
}

func TestMediaEncryptedWithoutE2EEIsRefused(t *testing.T) {
	t.Run("encrypted file content", func(t *testing.T) {
		var downloads atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/event/"):
				io.WriteString(w, `{"type":"m.room.message","event_id":"$img","sender":"@a:test","origin_server_ts":1,`+
					`"content":{"msgtype":"m.image","body":"enc.png","file":{"url":"mxc://test/media1","key":{"k":"a2V5","alg":"A256CTR","kty":"oct"},"iv":"aXY","hashes":{"sha256":"aGFzaA"},"v":"v2"},"info":{"mimetype":"image/png","size":9}}}`)
			case strings.Contains(r.URL.Path, "/media/download/"):
				downloads.Add(1)
				w.Write([]byte("ciphertext"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Media(context.Background(), "!r:test", "$img"); !errors.Is(err, ErrEncryptedMediaNoE2EE) {
			t.Fatalf("Media(encrypted file, E2EE off) = %v, want ErrEncryptedMediaNoE2EE", err)
		}
		if downloads.Load() != 0 {
			t.Fatalf("download requests = %d, want 0: ciphertext must never be fetched", downloads.Load())
		}
	})

	t.Run("encrypted event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/event/") {
				io.WriteString(w, `{"type":"m.room.encrypted","event_id":"$enc","sender":"@a:test","origin_server_ts":1,`+
					`"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"AwgAEnB","sender_key":"key","session_id":"sess","device_id":"DEV"}}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Media(context.Background(), "!r:test", "$enc"); !errors.Is(err, ErrEncryptedMediaNoE2EE) {
			t.Fatalf("Media(encrypted event, E2EE off) = %v, want ErrEncryptedMediaNoE2EE", err)
		}
	})
}

func TestMediaValidatesIDsBeforeRequesting(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Media(context.Background(), "not-a-room", "$img"); !errors.Is(err, ErrInvalidRoomID) {
		t.Fatalf("Media(bad room) = %v, want ErrInvalidRoomID", err)
	}
	if _, err := c.Media(context.Background(), "!r:test", "not-an-event"); !errors.Is(err, ErrInvalidEventID) {
		t.Fatalf("Media(bad event) = %v, want ErrInvalidEventID", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid IDs reached the homeserver %d times, want 0", hits.Load())
	}
}

// fakeSendServer answers the three endpoints a media send touches and records
// what it saw.
type fakeSendServer struct {
	*httptest.Server
	mu          sync.Mutex
	uploadPath  string
	uploadType  string
	uploadBody  []byte
	sendPath    string
	sendBody    string
	uploadCount int
	encrypted   bool
	eventID     string
}

func newFakeSendServer(t *testing.T) *fakeSendServer {
	t.Helper()
	fs := &fakeSendServer{eventID: "$sentmedia"}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			if fs.encrypted {
				io.WriteString(w, `{"algorithm":"m.megolm.v1.aes-sha2"}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		case strings.Contains(r.URL.Path, "/media/") && strings.HasSuffix(r.URL.Path, "/upload"):
			body, _ := io.ReadAll(r.Body)
			fs.mu.Lock()
			fs.uploadCount++
			fs.uploadPath = r.URL.Path
			fs.uploadType = r.Header.Get("Content-Type")
			fs.uploadBody = body
			fs.mu.Unlock()
			io.WriteString(w, `{"content_uri":"mxc://test/uploaded"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			body, _ := io.ReadAll(r.Body)
			fs.mu.Lock()
			fs.sendPath = r.URL.Path
			fs.sendBody = string(body)
			fs.mu.Unlock()
			io.WriteString(w, `{"event_id":"`+fs.eventID+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fs.Close)
	return fs
}

func TestSendMediaUploadsThenSends(t *testing.T) {
	fs := newFakeSendServer(t)
	c, err := New(testConfig(fs.URL, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	payload := pngBytes(t, 3, 2)
	sent, err := c.SendMedia(context.Background(), "!ok:test", MediaUpload{
		Data:     payload,
		MimeType: "image/png",
		Filename: "photo.png",
	}, "look at this", "$parent")
	if err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	if sent.EventID != "$sentmedia" || sent.MsgType != "m.image" || sent.MXCURI != "mxc://test/uploaded" {
		t.Fatalf("SendMedia result = %+v, want $sentmedia as m.image at the uploaded mxc URI", sent)
	}

	fs.mu.Lock()
	uploadPath, uploadType, uploadBody := fs.uploadPath, fs.uploadType, append([]byte(nil), fs.uploadBody...)
	sendPath, sendBody := fs.sendPath, fs.sendBody
	fs.mu.Unlock()

	if uploadPath != "/_matrix/media/v3/upload" {
		t.Fatalf("upload path = %q, want /_matrix/media/v3/upload", uploadPath)
	}
	if uploadType != "image/png" {
		t.Fatalf("upload content type = %q, want image/png", uploadType)
	}
	if !bytes.Equal(uploadBody, payload) {
		t.Fatalf("upload body is %d bytes, want the payload as-is", len(uploadBody))
	}
	if !strings.Contains(sendPath, "/rooms/!ok:test/send/m.room.message/") {
		t.Fatalf("send path = %q, want the room's m.room.message endpoint", sendPath)
	}
	for _, want := range []string{
		`"msgtype":"m.image"`,
		`"body":"look at this"`,
		`"url":"mxc://test/uploaded"`,
		`"filename":"photo.png"`,
		`"mimetype":"image/png"`,
		`"w":3`,
		`"h":2`,
		`"m.in_reply_to":{"event_id":"$parent"}`,
	} {
		if !strings.Contains(sendBody, want) {
			t.Fatalf("send body = %s, want it to contain %s", sendBody, want)
		}
	}
}

func TestSendMediaMsgTypePerMimetype(t *testing.T) {
	cases := []struct {
		mime    string
		msgType string
	}{
		{"image/jpeg", "m.image"},
		{"video/mp4", "m.video"},
		{"audio/ogg", "m.audio"},
		{"application/pdf", "m.file"},
		{"text/csv", "m.file"},
	}
	fs := newFakeSendServer(t)
	c, err := New(testConfig(fs.URL, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		fs.eventID = "$sent" + string(rune('a'+i))
		sent, err := c.SendMedia(context.Background(), "!ok:test", MediaUpload{
			Data:     []byte("payload-" + tc.mime),
			MimeType: tc.mime,
			Filename: "attachment.bin",
		}, "", "")
		if err != nil {
			t.Fatalf("SendMedia(%s): %v", tc.mime, err)
		}
		if sent.MsgType != tc.msgType {
			t.Fatalf("SendMedia(%s) msgtype = %q, want %q", tc.mime, sent.MsgType, tc.msgType)
		}
		fs.mu.Lock()
		body := fs.sendBody
		fs.mu.Unlock()
		if !strings.Contains(body, `"msgtype":"`+tc.msgType+`"`) {
			t.Fatalf("send body for %s = %s, want msgtype %s", tc.mime, body, tc.msgType)
		}
		if !strings.Contains(body, `"body":"attachment.bin"`) {
			t.Fatalf("send body for %s = %s, want the filename as the body when there is no caption", tc.mime, body)
		}
	}
}

func TestSendMediaRefusedBeforeAnyRequest(t *testing.T) {
	t.Run("not allowlisted", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			http.NotFound(w, r)
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL, "!ok:test"), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.SendMedia(context.Background(), "!other:test", MediaUpload{Data: []byte("x"), MimeType: "image/png", Filename: "x.png"}, "", "")
		if !errors.Is(err, ErrSendNotAllowed) {
			t.Fatalf("SendMedia(non-allowlisted) = %v, want ErrSendNotAllowed", err)
		}
		if hits.Load() != 0 {
			t.Fatalf("non-allowlisted send made %d requests, want 0", hits.Load())
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		fs := newFakeSendServer(t)
		c, err := New(testConfig(fs.URL, "!ok:test"), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.SendMedia(context.Background(), "!ok:test", MediaUpload{MimeType: "image/png"}, "", ""); !errors.Is(err, ErrEmptyMedia) {
			t.Fatalf("SendMedia(empty) = %v, want ErrEmptyMedia", err)
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if fs.uploadCount != 0 {
			t.Fatalf("empty payload still uploaded %d times", fs.uploadCount)
		}
	})

	t.Run("malformed reply", func(t *testing.T) {
		fs := newFakeSendServer(t)
		c, err := New(testConfig(fs.URL, "!ok:test"), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.SendMedia(context.Background(), "!ok:test", MediaUpload{Data: []byte("x"), MimeType: "image/png"}, "", "not-an-event")
		if !errors.Is(err, ErrInvalidEventID) {
			t.Fatalf("SendMedia(bad reply) = %v, want ErrInvalidEventID", err)
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if fs.uploadCount != 0 {
			t.Fatalf("bad reply still uploaded %d times", fs.uploadCount)
		}
	})
}

func TestSendMediaRefusesEncryptedRoomWithoutE2EE(t *testing.T) {
	fs := newFakeSendServer(t)
	fs.encrypted = true
	c, err := New(testConfig(fs.URL, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SendMedia(context.Background(), "!ok:test", MediaUpload{Data: []byte("x"), MimeType: "image/png"}, "", "")
	if !errors.Is(err, ErrRoomEncrypted) {
		t.Fatalf("SendMedia(encrypted room, E2EE off) = %v, want ErrRoomEncrypted", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.uploadCount != 0 {
		t.Fatalf("refused send still uploaded %d times", fs.uploadCount)
	}
}
