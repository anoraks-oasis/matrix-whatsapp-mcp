package matrix

import "errors"

// toolError is an error whose text is written for the agent calling a tool,
// while its cause stays in the chain for errors.Is, errors.As and the log. The
// cause is never appended to the text: a transport error naming a homeserver,
// a room or an event must not reach a client that only knows chats and
// messages.
type toolError struct {
	text  string
	cause error
}

func (e *toolError) Error() string { return e.text }

func (e *toolError) Unwrap() error { return e.cause }

// failedText logs the raw cause and returns the tool-facing text. Callers keep
// classifying the result with errors.Is(err, context.Canceled) or a transport
// sentinel, because the cause is still unwrapped.
func (c *Client) failedText(text string, cause error) error {
	c.log.Printf("%s: %v", text, cause)
	return &toolError{text: text, cause: cause}
}

// failedAs is failedText for a sentinel error: the agent reads the sentinel's
// own wording, and the cause stays classified together with the sentinel.
func (c *Client) failedAs(sentinel, cause error) error {
	c.log.Printf("%s: %v", sentinel, cause)
	return &toolError{text: sentinel.Error(), cause: errors.Join(sentinel, cause)}
}
