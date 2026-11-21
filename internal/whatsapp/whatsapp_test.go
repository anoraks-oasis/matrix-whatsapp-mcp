package whatsapp

import "testing"

// TestType covers the whole mapping table: every Matrix message type the tools
// can receive has a WhatsApp type, and the unrecognized ones are other rather
// than a leak of the Matrix name.
func TestType(t *testing.T) {
	tests := []struct {
		matrixType string
		want       string
	}{
		{"m.text", Text},
		{"m.notice", Text},
		{"m.emote", Text},
		{"m.image", Image},
		{"m.video", Video},
		{"m.audio", Audio},
		{"m.file", Document},
		{"m.sticker", Sticker},
		{"m.location", Location},
		{"m.room.message", Other},
		{"", Other},
	}
	for _, tc := range tests {
		if got := Type(tc.matrixType); got != tc.want {
			t.Errorf("Type(%q) = %q, want %q", tc.matrixType, got, tc.want)
		}
	}
}

// TestBody pins the placeholder text to the WhatsApp type, so a file is
// [document] and an unknown type is [other] rather than the Matrix name.
func TestBody(t *testing.T) {
	tests := []struct {
		matrixType string
		body       string
		want       string
	}{
		{"m.text", "", ""},
		{"m.notice", "", ""},
		{"", "", ""},
		{"m.text", "hello", "hello"},
		{"m.image", "", "[image]"},
		{"m.video", "", "[video]"},
		{"m.audio", "", "[audio]"},
		{"m.file", "", "[document]"},
		{"m.sticker", "", "[sticker]"},
		{"m.location", "", "[location]"},
		{"m.room.message", "", "[other]"},
		{"m.image", "a caption", "a caption"},
	}
	for _, tc := range tests {
		if got := Body(tc.matrixType, tc.body); got != tc.want {
			t.Errorf("Body(%q, %q) = %q, want %q", tc.matrixType, tc.body, got, tc.want)
		}
	}
}
