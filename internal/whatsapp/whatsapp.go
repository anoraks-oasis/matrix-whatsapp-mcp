// Package whatsapp holds the WhatsApp vocabulary the MCP surface and the
// webhook speak. Matrix message types stop at this boundary: every consumer
// that reports a message type or a placeholder body goes through Type and
// Body, so a tool description, a message field and a bracketed marker cannot
// describe the same message differently.
//
// Only the words change. A chat ID is still a Matrix room ID and a message ID
// is still a Matrix event ID; nothing here touches an ID.
package whatsapp

// The message types the tools report.
const (
	Text     = "text"
	Image    = "image"
	Video    = "video"
	Audio    = "audio"
	Document = "document"
	Sticker  = "sticker"
	Location = "location"
	Other    = "other"
)

// Type maps a Matrix message type to the WhatsApp type the tools report.
// Notice and emote are text, m.file is a document, and anything unrecognized
// is other.
func Type(matrixType string) string {
	switch matrixType {
	case "m.text", "m.notice", "m.emote":
		return Text
	case "m.image":
		return Image
	case "m.video":
		return Video
	case "m.audio":
		return Audio
	case "m.file":
		return Document
	case "m.sticker":
		return Sticker
	case "m.location":
		return Location
	default:
		return Other
	}
}

// Body returns body unchanged when it carries text, and otherwise a bracketed
// marker naming the WhatsApp type, such as [image] or [document], so a caller
// never sees a silently blank message. A text message (or an untyped event)
// with no body stays empty.
func Body(matrixType, body string) string {
	if body != "" {
		return body
	}
	typ := Type(matrixType)
	if typ == Text || matrixType == "" {
		return ""
	}
	return "[" + typ + "]"
}
