package realtime

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"
)

// DefaultMaxMessageSize bounds one WebSocket message in either direction. A
// realtime event carries at most one audio chunk (base64 inside JSON), so one
// mebibyte is generous; the limit is enforced by the transport and again by the
// bridge so a non-WebSocket provider cannot bypass it.
const DefaultMaxMessageSize = 1 << 20

// Accept upgrades the browser HTTP request to a WebSocket. Origin verification
// stays at the library default (same host only): the bundled web UI is served
// from this origin and a cross-origin page must not be able to open a voice
// session with the user's cookie. On any failure Accept has already written the
// HTTP response.
func Accept(w http.ResponseWriter, r *http.Request) (Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(DefaultMaxMessageSize)
	return &wsConn{conn: c}, nil
}

// Dial opens the provider leg of the bridge. The caller passes the headers the
// provider needs (authorization, beta flags); the bridge never sees the key.
func Dial(ctx context.Context, endpoint string, header http.Header) (Conn, error) {
	c, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(DefaultMaxMessageSize)
	return &wsConn{conn: c}, nil
}

// wsConn adapts *websocket.Conn to Conn and normalizes peer closes into
// CloseError so the bridge can propagate the code without depending on the
// library.
type wsConn struct {
	conn *websocket.Conn
}

// Read implements Conn.
func (c *wsConn) Read(ctx context.Context) (Message, error) {
	typ, data, err := c.conn.Read(ctx)
	if err != nil {
		if code := websocket.CloseStatus(err); code != -1 {
			var ce websocket.CloseError
			reason := ""
			if errors.As(err, &ce) {
				reason = ce.Reason
			}
			return Message{}, &CloseError{Code: int(code), Reason: reason}
		}
		return Message{}, err
	}
	mt := MessageText
	if typ == websocket.MessageBinary {
		mt = MessageBinary
	}
	return Message{Type: mt, Data: data}, nil
}

// Write implements Conn.
func (c *wsConn) Write(ctx context.Context, msg Message) error {
	typ := websocket.MessageText
	if msg.Type == MessageBinary {
		typ = websocket.MessageBinary
	}
	return c.conn.Write(ctx, typ, msg.Data)
}

// Close implements Conn. The library makes repeated calls no-ops.
func (c *wsConn) Close(code int, reason string) error {
	return c.conn.Close(websocket.StatusCode(code), truncateCloseReason(reason))
}

// truncateCloseReason keeps the control-frame reason inside the 123-byte budget
// (the library rejects longer reasons).
func truncateCloseReason(reason string) string {
	if len(reason) <= 123 {
		return reason
	}
	cut := 123
	for cut > 0 && (reason[cut]&0xC0) == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return reason[:cut]
}
