package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/image/draw"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/whatsapp"
)

const (
	// defaultInlineMediaBudget is how many base64 characters one get_media
	// result may carry. It is about 1.5 MiB: large enough for a phone photo,
	// small enough that a tool result stays a tool result rather than a
	// download.
	defaultInlineMediaBudget = 1536 * 1024
	// defaultMediaDimension is the longest edge an inline image is allowed to
	// have when the caller does not say. 1568 is the long edge Claude's vision
	// path handles without further downscaling.
	defaultMediaDimension = 1568
	// minMediaDimension and maxMediaDimension bound max_dimension.
	minMediaDimension = 256
	maxMediaDimension = 4096
	// minInlineEdge is the smallest long edge the re-encoder will shrink to
	// before giving up and returning metadata only.
	minInlineEdge = 64
	// maxMediaUploadBytes is the largest payload send_media accepts.
	maxMediaUploadBytes = 50 << 20
)

// jpegQualities is the quality ladder the image re-encoder walks, highest
// first, before it starts shrinking the dimensions.
var jpegQualities = []int{85, 75, 65, 55, 45, 35}

// errMediaUploadTooLarge is returned for a send_media payload over
// maxMediaUploadBytes, before anything is uploaded.
var errMediaUploadTooLarge = errors.New("media is larger than the 50 MiB upload limit")

type getMediaInput struct {
	chatRef
	messageRef
	MaxDimension int `json:"max_dimension,omitempty" jsonschema:"the longest edge in pixels an image may have in the returned image block; default 1568, clamped to 256..4096. A larger image is downscaled and the metadata reports downscaled true"`
}

type getMediaResult struct {
	Mimetype   string `json:"mimetype" jsonschema:"the attachment's MIME type: what the message declares, else what the server served, else what the first bytes look like when the declared type is application/octet-stream"`
	Filename   string `json:"filename,omitempty" jsonschema:"the filename the sender set on the attachment (content.filename); absent when the sender set none"`
	SizeBytes  int64  `json:"size_bytes" jsonschema:"the size of the downloaded payload in bytes; for encrypted media this is the decrypted size"`
	Width      int    `json:"width,omitempty" jsonschema:"the width in pixels of the image this result describes: the returned image block after downscaling, or the source image when its payload did not fit; absent when the payload is not an image"`
	Height     int    `json:"height,omitempty" jsonschema:"the height in pixels of the image this result describes: the returned image block after downscaling, or the source image when its payload did not fit; absent when the payload is not an image"`
	DurationMS int64  `json:"duration_ms,omitempty" jsonschema:"the duration the sender declared for the media, in milliseconds; absent when the sender declared none"`
	MediaURI   string `json:"media_uri" jsonschema:"an opaque reference to the attachment's stored bytes; pass it back to the server's media API when you need the payload outside this tool"`
	Encrypted  bool   `json:"encrypted" jsonschema:"true when the attachment was end-to-end encrypted and was decrypted before being returned"`
	Downscaled bool   `json:"downscaled" jsonschema:"true when the returned image was resized or re-encoded smaller to respect max_dimension or the inline budget"`
	Included   bool   `json:"included" jsonschema:"whether the result also carries the payload as an image or audio content block. False means the text block is the whole answer and reason says why"`
	Reason     string `json:"reason,omitempty" jsonschema:"why the payload is not included as a content block, for example that only metadata is returned for a video or a file; absent when included is true"`
}

type sendMediaInput struct {
	chatRef
	Path           string `json:"path,omitempty" jsonschema:"absolute path of a local file to upload. Pass exactly one of path and data_base64. The file must exist, be readable, be a regular file and be at most 50 MiB; its bytes are read into memory and never modified"`
	DataBase64     string `json:"data_base64,omitempty" jsonschema:"the payload itself as standard base64. Pass exactly one of path and data_base64, and mimetype alongside it. The decoded payload must be at most 50 MiB"`
	Mimetype       string `json:"mimetype,omitempty" jsonschema:"the payload's MIME type, for example image/jpeg. Required with data_base64; with path it overrides the type detected from the file. It decides the reported type: image/* becomes image, video/* video, audio/* audio, anything else document"`
	Caption        string `json:"caption,omitempty" jsonschema:"text to send as the message text, which WhatsApp shows under the media. When empty the filename is used as the text"`
	Filename       string `json:"filename,omitempty" jsonschema:"the filename to record on the message and the upload; default the base name of path, or upload for data_base64"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"optional message ID this one answers, for example $abc123. It must be a message reference ($ followed by the ID) or the call is rejected before anything is uploaded. The replied-to message can be in any chat; only the resolved target chat must be allow-listed"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"optional key of at most 128 characters that makes a retry safe. A second call with the same key, chat and payload within 10 minutes returns the first message ID with deduplicated true and uploads nothing; the same key with a different payload is an error. Keys are held in memory only, so a restart forgets them"`
}

type sendMediaResult struct {
	MessageID    string `json:"message_id" jsonschema:"the message ID of the sent media message, or of the original send when the call was deduplicated"`
	Deduplicated bool   `json:"deduplicated" jsonschema:"true when an earlier send with the same idempotency_key was returned instead of uploading and sending again"`
	Type         string `json:"type" jsonschema:"the WhatsApp message type the media was sent as: image, video, audio or document"`
	MediaURI     string `json:"media_uri,omitempty" jsonschema:"the opaque reference the payload was uploaded to; absent on a deduplicated replay, which uploads nothing"`
}

// handleGetMedia answers one attachment as metadata plus, when it qualifies, an
// image or audio content block. The text block is always the metadata JSON, so
// a caller that cannot use a payload still learns what the attachment is and
// why the payload is missing.
func (s *Server) handleGetMedia(ctx context.Context, _ *mcp.CallToolRequest, in getMediaInput) (*mcp.CallToolResult, getMediaResult, error) {
	if !matrix.ValidEventID(in.messageID()) {
		return nil, getMediaResult{}, fmt.Errorf("%w: message_id must be a message reference, a $ sigil followed by the ID", matrix.ErrInvalidEventID)
	}
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, getMediaResult{}, err
	}
	att, err := s.matrix.Media(ctx, chatID, in.messageID())
	if err != nil {
		return nil, getMediaResult{}, err
	}
	mimeType := normalizeMimeType(att.MimeType)
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(att.Data)
	}
	out := getMediaResult{
		Mimetype:   mimeType,
		Filename:   att.Filename,
		SizeBytes:  int64(len(att.Data)),
		DurationMS: att.DurationMS,
		MediaURI:   att.MXCURI,
		Encrypted:  att.Encrypted,
	}
	block, width, height, downscaled, reason, err := s.inlineMedia(att.Data, mimeType, clampMediaDimension(in.MaxDimension))
	if err != nil {
		return nil, getMediaResult{}, err
	}
	out.Width, out.Height = width, height
	out.Downscaled = downscaled
	out.Included = block != nil
	out.Reason = reason

	text, err := json.Marshal(out)
	if err != nil {
		return nil, getMediaResult{}, fmt.Errorf("encode media metadata: %w", err)
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}
	if block != nil {
		res.Content = append(res.Content, block)
	}
	return res, out, nil
}

// inlineMedia turns one payload into the content block the tool returns, or
// explains why it returns none. Images are downscaled and re-encoded within the
// budget; audio is passed through when it fits; anything else is metadata only,
// because turning a video or a document into base64 would spend the whole
// result on something no client renders.
func (s *Server) inlineMedia(data []byte, mimeType string, maxDim int) (mcp.Content, int, int, bool, string, error) {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		encoded, encodedType, width, height, downscaled, err := encodeInlineImage(data, maxDim, s.inlineBudget)
		if err != nil {
			return nil, 0, 0, false, "", err
		}
		if encoded == nil {
			// The dimensions of the source image are still worth reporting:
			// they describe the attachment even though its bytes were dropped.
			return nil, width, height, false, fmt.Sprintf("the image is still larger than the %d-byte inline budget after downscaling and re-encoding, so only its metadata is returned", s.inlineBudget), nil
		}
		return &mcp.ImageContent{Data: encoded, MIMEType: encodedType}, width, height, downscaled, "", nil
	case strings.HasPrefix(mimeType, "audio/"):
		if !inlineFits(len(data), s.inlineBudget) {
			return nil, 0, 0, false, fmt.Sprintf("the audio payload is larger than the %d-byte inline budget, so only its metadata is returned", s.inlineBudget), nil
		}
		return &mcp.AudioContent{Data: data, MIMEType: mimeType}, 0, 0, false, "", nil
	default:
		return nil, 0, 0, false, fmt.Sprintf("%s is neither an image nor audio, so only its metadata is returned", mimeType), nil
	}
}

// encodeInlineImage decodes an image, fits it inside maxDim and re-encodes it
// until the base64 payload fits the budget, lowering quality first and then
// dimensions. It returns nil data and no error when the image cannot be made to
// fit, so the caller can report a reason rather than silently truncating.
//
// A payload already inside both budgets is returned unchanged: re-encoding a
// small JPEG would only lose quality, and re-encoding a GIF would drop its
// animation.
func encodeInlineImage(payload []byte, maxDim, budget int) ([]byte, string, int, int, bool, error) {
	img, format, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		return nil, "", 0, 0, false, fmt.Errorf("%w: the image could not be decoded: %v", matrix.ErrUnsupportedCodec, err)
	}
	bounds := img.Bounds()
	origW, origH := bounds.Dx(), bounds.Dy()
	if origW <= 0 || origH <= 0 {
		return nil, "", 0, 0, false, fmt.Errorf("%w: the image carries no pixels", matrix.ErrUnsupportedCodec)
	}
	width, height := fitWithin(origW, origH, maxDim)
	if width == origW && height == origH && inlineFits(len(payload), budget) {
		return payload, imageMime(format), origW, origH, false, nil
	}
	for {
		var source image.Image = img
		if width != origW || height != origH {
			source = scaleTo(img, width, height)
		}
		if format == "png" {
			if encoded, err := encodePNG(source); err == nil && inlineFits(len(encoded), budget) {
				return encoded, "image/png", width, height, true, nil
			}
		}
		for _, quality := range jpegQualities {
			encoded, err := encodeJPEG(source, quality)
			if err != nil {
				continue
			}
			if inlineFits(len(encoded), budget) {
				return encoded, "image/jpeg", width, height, true, nil
			}
		}
		if max(width, height) <= minInlineEdge {
			return nil, "", origW, origH, false, nil
		}
		width, height = width*3/4, height*3/4
		if width < 1 {
			width = 1
		}
		if height < 1 {
			height = 1
		}
	}
}

// fitWithin scales width and height down so the longer edge is at most
// maxEdge, keeping the aspect ratio. An image already inside the bound is
// returned unchanged.
func fitWithin(width, height, maxEdge int) (int, int) {
	longest := max(width, height)
	if longest <= maxEdge {
		return width, height
	}
	scale := float64(maxEdge) / float64(longest)
	scaledWidth := max(int(math.Round(float64(width)*scale)), 1)
	scaledHeight := max(int(math.Round(float64(height)*scale)), 1)
	return scaledWidth, scaledHeight
}

// scaleTo resizes src with Catmull-Rom interpolation, which keeps a
// downscaled photo legible instead of producing the aliased mess a nearest
// neighbour scale would.
func scaleTo(src image.Image, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeJPEG flattens transparency onto white first: JPEG has no alpha
// channel, and encoding an RGBA image directly would turn every transparent
// pixel black.
func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func flatten(img image.Image) image.Image {
	if opaque, ok := img.(interface{ Opaque() bool }); ok && opaque.Opaque() {
		return img
	}
	bounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Over)
	return dst
}

// inlineFits reports whether size bytes of binary encode to a base64 payload
// inside the budget.
func inlineFits(size, budget int) bool {
	return base64.StdEncoding.EncodedLen(size) <= budget
}

func imageMime(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	default:
		return "image/" + format
	}
}

// normalizeMimeType lowercases a MIME type and drops any parameters, so
// "image/JPEG; charset=binary" and "image/jpeg" classify the same.
func normalizeMimeType(value string) string {
	if parsed, _, err := mime.ParseMediaType(value); err == nil {
		return strings.ToLower(parsed)
	}
	return strings.ToLower(strings.TrimSpace(value))
}

// clampMediaDimension applies get_media's default and clamp. Zero and negative
// values mean the caller did not choose a size, not that it wants a 0-pixel
// image.
func clampMediaDimension(value int) int {
	switch {
	case value <= 0:
		return defaultMediaDimension
	case value < minMediaDimension:
		return minMediaDimension
	case value > maxMediaDimension:
		return maxMediaDimension
	default:
		return value
	}
}

// outgoingMedia is a payload that passed validation and is ready for the gate.
type outgoingMedia struct {
	data     []byte
	filename string
	mimeType string
}

// handleSendMedia applies the send gate to one media message. Every rejection
// that does not need the homeserver — a bad reply target, an unusable payload
// source, a path that is not an absolute readable file — happens before the
// idempotency key is claimed and before the rate-limit token is spent.
func (s *Server) handleSendMedia(ctx context.Context, _ *mcp.CallToolRequest, in sendMediaInput) (*mcp.CallToolResult, sendMediaResult, error) {
	if in.ReplyTo != "" && !matrix.ValidEventID(in.ReplyTo) {
		return nil, sendMediaResult{}, fmt.Errorf("%w: reply_to must be a message ID, a $ sigil followed by the ID", matrix.ErrInvalidEventID)
	}
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, sendMediaResult{}, err
	}
	if utf8.RuneCountInString(in.IdempotencyKey) > maxIdempotencyKeyLen {
		return nil, sendMediaResult{}, fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLen)
	}
	media, err := loadOutgoingMedia(in)
	if err != nil {
		return nil, sendMediaResult{}, err
	}

	// The key is claimed before the rate limiter, exactly as send_message does,
	// so a retry of a send that already went through returns the original event
	// ID instead of "rate limited". The fingerprint covers the payload, so the
	// same key with different bytes is a conflict rather than a replay.
	var claim *sendClaim
	if in.IdempotencyKey != "" {
		c, err := s.sends.claim(ctx, in.IdempotencyKey, chatID, mediaFingerprint(media, in))
		if err != nil {
			return nil, sendMediaResult{}, err
		}
		if c.replayed {
			return nil, sendMediaResult{
				MessageID:    c.eventID,
				Deduplicated: true,
				Type:         whatsapp.Type(string(matrix.MediaMessageType(media.mimeType))),
			}, nil
		}
		claim = c
		defer s.sends.release(claim)
	}

	if err := s.gate.check(chatID); err != nil {
		return nil, sendMediaResult{}, err
	}
	sent, err := s.matrix.SendMedia(ctx, chatID, matrix.MediaUpload{
		Data:     media.data,
		MimeType: media.mimeType,
		Filename: media.filename,
	}, in.Caption, in.ReplyTo)
	if err != nil {
		return nil, sendMediaResult{}, err
	}
	if claim != nil {
		s.sends.settle(claim, sent.EventID)
	}
	return nil, sendMediaResult{MessageID: sent.EventID, Type: whatsapp.Type(sent.MsgType), MediaURI: sent.MXCURI}, nil
}

// loadOutgoingMedia validates the payload source and reads it into memory. It
// never writes, and it never echoes payload bytes in an error.
func loadOutgoingMedia(in sendMediaInput) (outgoingMedia, error) {
	hasPath := strings.TrimSpace(in.Path) != ""
	hasData := strings.TrimSpace(in.DataBase64) != ""
	if hasPath == hasData {
		if hasPath {
			return outgoingMedia{}, errors.New("pass exactly one of path and data_base64, not both")
		}
		return outgoingMedia{}, errors.New("pass one of path or data_base64")
	}

	if hasData {
		if strings.TrimSpace(in.Mimetype) == "" {
			return outgoingMedia{}, errors.New("mimetype is required with data_base64")
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.DataBase64))
		if err != nil {
			return outgoingMedia{}, errors.New("data_base64 is not valid standard base64")
		}
		if len(data) == 0 {
			return outgoingMedia{}, errors.New("data_base64 decodes to no bytes")
		}
		if len(data) > maxMediaUploadBytes {
			return outgoingMedia{}, errMediaUploadTooLarge
		}
		filename := strings.TrimSpace(in.Filename)
		if filename == "" {
			filename = "upload"
		}
		return outgoingMedia{data: data, filename: filename, mimeType: normalizeMimeType(in.Mimetype)}, nil
	}

	path := strings.TrimSpace(in.Path)
	if !filepath.IsAbs(path) {
		return outgoingMedia{}, fmt.Errorf("path must be absolute: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return outgoingMedia{}, fmt.Errorf("path is not readable: %v", err)
	}
	if info.IsDir() {
		return outgoingMedia{}, fmt.Errorf("path is a directory, not a file: %s", path)
	}
	if !info.Mode().IsRegular() {
		return outgoingMedia{}, fmt.Errorf("path is not a regular file: %s", path)
	}
	if info.Size() > maxMediaUploadBytes {
		return outgoingMedia{}, errMediaUploadTooLarge
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return outgoingMedia{}, fmt.Errorf("path is not readable: %v", err)
	}
	if len(data) > maxMediaUploadBytes {
		return outgoingMedia{}, errMediaUploadTooLarge
	}
	if len(data) == 0 {
		return outgoingMedia{}, errors.New("path holds no bytes")
	}

	mimeType := normalizeMimeType(in.Mimetype)
	if mimeType == "" {
		mimeType = normalizeMimeType(mime.TypeByExtension(strings.ToLower(filepath.Ext(path))))
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	filename := strings.TrimSpace(in.Filename)
	if filename == "" {
		filename = filepath.Base(path)
	}
	return outgoingMedia{data: data, filename: filename, mimeType: mimeType}, nil
}

// mediaFingerprint identifies a payload for the idempotency cache: the same key
// and the same bytes, caption and reply target replay, while a different file
// under the same key is the conflict the cache already reports for text.
func mediaFingerprint(media outgoingMedia, in sendMediaInput) string {
	hash := sha256.New()
	hash.Write(media.data)
	for _, field := range []string{media.filename, media.mimeType, in.Caption, in.ReplyTo} {
		hash.Write([]byte{0})
		hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
