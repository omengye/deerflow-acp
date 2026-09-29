package harness

// EventPage is a page of durable domain events, not ACP session/update frames.
// NextCursor pins the read to the first page's last committed event. A new
// empty-cursor request includes later events. Asset bytes are never embedded.
type EventPage struct {
	Events     []RunEvent `json:"events"`
	NextCursor string     `json:"nextCursor,omitempty"`
}
