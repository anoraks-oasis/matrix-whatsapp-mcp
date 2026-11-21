package matrix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

// MaxMediaDownloadBytes is the largest attachment one get_media call will
// download. It bounds both the memory a call can cost and what a hostile or
// merely enormous file can do to the process: the payload is held in memory
// and never written to disk.
const MaxMediaDownloadBytes = 25 << 20

var (
	// ErrNotMedia is returned when an event exists but carries no attachment,
	// such as a text message or a state event. It is deliberately distinct from
	// ErrNotAMessage: a membership event is not a message at all, while a text
	// message is a message that simply has no media.
	ErrNotMedia = errors.New("this message has no attachment")
	// ErrMediaTooLarge is returned when the homeserver reports or serves more
	// than MaxMediaDownloadBytes.
	ErrMediaTooLarge = errors.New("attachment is too large to download")
	// ErrMediaDownload is returned when the homeserver refuses or fails the
	// media download, or serves an attachment that cannot be interpreted.
	ErrMediaDownload = errors.New("could not download the attachment")
	// ErrEncryptedMediaNoE2EE is returned for encrypted media when E2EE is off:
	// the bytes would be ciphertext, and returning them would be garbage.
	ErrEncryptedMediaNoE2EE = errors.New("this message is encrypted and end-to-end encryption is switched off on the server; set MATRIX_E2EE_ENABLED=true")
	// ErrMediaUndecryptable is returned when E2EE is on but the attachment's key
	// material is missing or does not decrypt its own payload.
	ErrMediaUndecryptable = errors.New("this attachment could not be decrypted")
	// ErrUnsupportedCodec is returned for an attachment this server cannot
	// interpret: an encryption algorithm it does not implement, or an image
	// format the Go standard library cannot decode.
	ErrUnsupportedCodec = errors.New("unsupported attachment format")
	// ErrEmptyMedia is returned when a send_media payload has no bytes.
	ErrEmptyMedia = errors.New("the attachment is empty")
)

// MediaAttachment is one downloaded attachment: what the message says about it
// plus the bytes themselves, which for encrypted media are the decrypted bytes.
type MediaAttachment struct {
	MimeType   string
	Filename   string
	DurationMS int64
	MXCURI     string
	Encrypted  bool
	Data       []byte
}

// MediaUpload is one attachment to upload and send.
type MediaUpload struct {
	Data     []byte
	MimeType string
	Filename string
}

// MediaSendResult is what sending one media message produced.
type MediaSendResult struct {
	EventID string
	MsgType string
	MXCURI  string
}

// mediaAttachment is the attachment a message event describes, read from either the
// plaintext content.url or the encrypted content.file shape.
type mediaAttachment struct {
	MXCURI     string
	MimeType   string
	Filename   string
	SizeBytes  int64
	Width      int
	Height     int
	DurationMS int64
	Encrypted  bool
	File       *event.EncryptedFileInfo
}

// contentAttachment reads the attachment off a media message. A message with
// neither a url nor a file carries none, which is how a text message is told
// apart from an image without trusting the msgtype alone.
func contentAttachment(msg *event.MessageEventContent) (mediaAttachment, bool) {
	if msg == nil {
		return mediaAttachment{}, false
	}
	att := mediaAttachment{Filename: msg.FileName}
	if msg.Info != nil {
		att.MimeType = msg.Info.MimeType
		att.SizeBytes = int64(msg.Info.Size)
		att.Width = msg.Info.Width
		att.Height = msg.Info.Height
		att.DurationMS = int64(msg.Info.Duration)
	}
	switch {
	case msg.File != nil && msg.File.URL != "":
		att.MXCURI = string(msg.File.URL)
		att.Encrypted = true
		att.File = msg.File
	case msg.URL != "":
		att.MXCURI = string(msg.URL)
	default:
		return mediaAttachment{}, false
	}
	return att, true
}

// Media implements MatrixService. It fetches one event, reads its attachment
// descriptor, downloads the payload and decrypts it when it is encrypted. The
// payload is returned in memory: nothing is cached and nothing touches disk.
//
// Encrypted media is refused with ErrEncryptedMediaNoE2EE when E2EE is off,
// because the ciphertext on the media repository is not a payload a caller can
// use. An event that is not in the room is ErrEventNotFound, and an event that
// carries no attachment is ErrNotMedia; neither is reported as empty media.
func (c *Client) Media(ctx context.Context, roomID, eventID string) (MediaAttachment, error) {
	if !config.ValidRoomID(roomID) {
		return MediaAttachment{}, ErrInvalidRoomID
	}
	if !ValidEventID(eventID) {
		return MediaAttachment{}, ErrInvalidEventID
	}
	evt, err := c.cli.GetEvent(ctx, id.RoomID(roomID), id.EventID(eventID))
	if err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return MediaAttachment{}, c.failedAs(ErrEventNotFound, err)
		}
		return MediaAttachment{}, c.failedText("could not read the message", err)
	}
	if evt.Type == event.EventEncrypted {
		if !c.cfg.E2EEEnabled || c.crypto.Load() == nil {
			return MediaAttachment{}, ErrEncryptedMediaNoE2EE
		}
		decrypted := c.decrypt(ctx, evt)
		if decrypted == nil {
			return MediaAttachment{}, ErrMediaUndecryptable
		}
		evt = decrypted
	}
	if evt.Type != event.EventMessage {
		return MediaAttachment{}, ErrNotMedia
	}
	if evt.Content.Parsed == nil {
		if err := evt.Content.ParseRaw(evt.Type); err != nil {
			return MediaAttachment{}, ErrNotMedia
		}
	}
	msg := evt.Content.AsMessage()
	att, ok := contentAttachment(msg)
	if !ok {
		return MediaAttachment{}, ErrNotMedia
	}
	if att.Encrypted && (!c.cfg.E2EEEnabled || c.crypto.Load() == nil) {
		return MediaAttachment{}, ErrEncryptedMediaNoE2EE
	}

	uri, err := id.ParseContentURI(att.MXCURI)
	if err != nil {
		return MediaAttachment{}, c.failedAs(ErrMediaDownload, err)
	}
	data, contentType, err := c.downloadMedia(ctx, uri)
	if err != nil {
		return MediaAttachment{}, err
	}
	if att.Encrypted {
		if att.File == nil {
			return MediaAttachment{}, ErrMediaUndecryptable
		}
		data, err = c.decryptMedia(att.File, data)
		if err != nil {
			return MediaAttachment{}, err
		}
	}
	if att.MimeType == "" {
		att.MimeType = contentType
	}
	return MediaAttachment{
		MimeType:   att.MimeType,
		Filename:   att.Filename,
		DurationMS: att.DurationMS,
		MXCURI:     att.MXCURI,
		Encrypted:  att.Encrypted,
		Data:       data,
	}, nil
}

// downloadMedia fetches one content URI, refusing anything the homeserver
// declares or turns out to be larger than MaxMediaDownloadBytes. The declared
// Content-Length is checked before a byte is read, and a chunked response is
// cut off one byte past the limit, so neither a header nor a missing one can
// make the download unbounded.
func (c *Client) downloadMedia(ctx context.Context, uri id.ContentURI) ([]byte, string, error) {
	resp, err := c.cli.Download(ctx, uri)
	if err != nil {
		return nil, "", c.failedAs(ErrMediaDownload, err)
	}
	defer resp.Body.Close()
	if resp.ContentLength > MaxMediaDownloadBytes {
		return nil, "", c.failedAs(ErrMediaTooLarge, fmt.Errorf("server reports %d bytes, over the %d-byte limit", resp.ContentLength, MaxMediaDownloadBytes))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxMediaDownloadBytes+1))
	if err != nil {
		return nil, "", c.failedAs(ErrMediaDownload, err)
	}
	if len(data) > MaxMediaDownloadBytes {
		return nil, "", c.failedAs(ErrMediaTooLarge, fmt.Errorf("payload over the %d-byte limit", MaxMediaDownloadBytes))
	}
	contentType := resp.Header.Get("Content-Type")
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = parsed
	}
	return data, contentType, nil
}

// decryptMedia decrypts an attachment with the key material its own content
// carries. The hash check is part of the decryption: a payload that does not
// match the declared SHA-256 is treated as corrupt rather than returned.
func (c *Client) decryptMedia(file *event.EncryptedFileInfo, data []byte) ([]byte, error) {
	encrypted := file.EncryptedFile
	plaintext, err := encrypted.Decrypt(data)
	if err != nil {
		switch {
		case errors.Is(err, attachment.ErrUnsupportedVersion), errors.Is(err, attachment.ErrUnsupportedAlgorithm):
			return nil, c.failedAs(ErrUnsupportedCodec, err)
		default:
			return nil, c.failedAs(ErrMediaUndecryptable, err)
		}
	}
	return plaintext, nil
}

// MediaMessageType maps a MIME type to the Matrix message type that carries it,
// which is also what the bridge looks at to decide how to render it in
// WhatsApp: images as photos, videos as videos, audio as voice notes and
// everything else as a document.
func MediaMessageType(mimeType string) event.MessageType {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(mimeType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// SendMedia implements MatrixService. It uploads data to the media repository
// and sends it as one message of the type MediaMessageType picks, with caption
// as the body so WhatsApp shows it under the media and with the reply relation
// when replyTo is set. The upload, the send and the bridge-ack guard are the
// same ones SendText uses: the allowlist is checked here too, and an encrypted
// room is refused when E2EE is off rather than receiving plaintext media.
//
// Media for an encrypted room is itself encrypted before upload, with the key
// parameter in the event content, so an E2EE room never gains an unencrypted
// copy of an attachment in its media repository.
func (c *Client) SendMedia(ctx context.Context, roomID string, media MediaUpload, caption, replyTo string) (MediaSendResult, error) {
	if !config.ValidRoomID(roomID) {
		return MediaSendResult{}, ErrInvalidRoomID
	}
	if !c.allow.Allowed(roomID) {
		return MediaSendResult{}, ErrSendNotAllowed
	}
	if len(media.Data) == 0 {
		return MediaSendResult{}, ErrEmptyMedia
	}
	if replyTo != "" && !ValidEventID(replyTo) {
		return MediaSendResult{}, ErrInvalidEventID
	}
	encryptedRoom, err := c.prepareEncryption(ctx, id.RoomID(roomID))
	if err != nil {
		return MediaSendResult{}, err
	}

	msgType := MediaMessageType(media.MimeType)
	body := caption
	if strings.TrimSpace(body) == "" {
		body = media.Filename
	}
	if body == "" {
		body = "upload"
	}
	content := &event.MessageEventContent{
		MsgType:  msgType,
		Body:     body,
		FileName: media.Filename,
		Info: &event.FileInfo{
			MimeType: media.MimeType,
			Size:     len(media.Data),
		},
	}
	if width, height, ok := imageDimensions(media.Data, media.MimeType); ok {
		content.Info.Width = width
		content.Info.Height = height
	}

	upload := mautrix.ReqUploadMedia{ContentType: media.MimeType, FileName: media.Filename}
	if encryptedRoom {
		file := attachment.NewEncryptedFile()
		upload.ContentBytes = file.Encrypt(media.Data)
		upload.ContentType = "application/octet-stream"
		uploaded, err := c.cli.UploadMedia(ctx, upload)
		if err != nil {
			return MediaSendResult{}, c.failedText("could not upload the attachment", err)
		}
		content.File = &event.EncryptedFileInfo{EncryptedFile: *file, URL: id.ContentURIString(uploaded.ContentURI.String())}
		content.URL = ""
	} else {
		upload.ContentBytes = media.Data
		uploaded, err := c.cli.UploadMedia(ctx, upload)
		if err != nil {
			return MediaSendResult{}, c.failedText("could not upload the attachment", err)
		}
		content.URL = id.ContentURIString(uploaded.ContentURI.String())
	}

	if replyTo != "" {
		content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: id.EventID(replyTo)}}
	}
	resp, err := c.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return MediaSendResult{}, c.failedText("could not send the attachment", err)
	}
	eventID := resp.EventID.String()
	if c.cfg.BridgeAckTimeout > 0 {
		if notice, ok := c.awaitBridgeNotice(ctx, roomID, resp.EventID); ok {
			return MediaSendResult{}, &BridgeNoticeError{EventID: eventID, Notice: notice}
		}
	}
	return MediaSendResult{EventID: eventID, MsgType: string(msgType), MXCURI: mediaURI(content)}, nil
}

// mediaURI returns the content URI a sent media message points at, whichever
// shape it carries.
func mediaURI(content *event.MessageEventContent) string {
	if content.File != nil {
		return string(content.File.URL)
	}
	return string(content.URL)
}

// imageDimensions reads an image's pixel size from its header, so an outgoing
// image carries the width and height WhatsApp uses to lay out the preview. A
// non-image, or bytes whose header does not decode, reports false and the
// fields stay out of the event.
func imageDimensions(data []byte, mimeType string) (int, int, bool) {
	if !strings.HasPrefix(mimeType, "image/") {
		return 0, 0, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}
