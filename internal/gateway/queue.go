// The inbound message queue is where backpressure lives.
//
// Every transport reads from its source, decodes a frame and pushes the
// message into this queue. The queue is bounded by both message count and
// payload bytes; a push that would exceed either bound blocks until the
// consumer makes room. Blocking the reader stops reading the source, which
// stops draining the socket/pipe, which makes the provider's write block: the
// pressure travels the whole chain without an unbounded buffer anywhere
// (external-gateway spec: a slow consumer must not OOM the runtime).

package gateway

import (
	"context"
	"sync"
)

// queuedMessage is one message plus the wire bytes it occupies, so the byte
// bound can be enforced without re-encoding.
type queuedMessage struct {
	message Message
	size    int64
}

// messageQueue is a bounded FIFO with a two-sided interface: producers use the
// blocking push, one consumer uses pop plus the wake channel.
//
// Closing never drops what is already queued: the consumer drains, then sees
// the terminal error exactly once.
type messageQueue struct {
	mu       sync.Mutex
	room     *sync.Cond
	items    []queuedMessage
	head     int
	bytes    int64
	closed   bool
	terminal error
	wake     chan struct{}

	maxItems int
	maxBytes int64

	peakItems int
	peakBytes int64
}

// newMessageQueue returns a queue with the given bounds. Non-positive bounds
// fall back to the defaults.
func newMessageQueue(maxItems int, maxBytes int64) *messageQueue {
	if maxItems <= 0 {
		maxItems = DefaultMaxQueuedMessages
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxQueuedBytes
	}
	queue := &messageQueue{
		wake:     make(chan struct{}, 1),
		maxItems: maxItems,
		maxBytes: maxBytes,
	}
	queue.room = sync.NewCond(&queue.mu)
	return queue
}

// push enqueues message, blocking while either bound would be exceeded. It
// returns false when the queue is closed (the message was not enqueued).
func (q *messageQueue) push(message Message, size int) bool {
	q.mu.Lock()
	for !q.closed && q.full(size) {
		q.room.Wait()
	}
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, queuedMessage{message: message, size: int64(size)})
	q.bytes += int64(size)
	if items := len(q.items) - q.head; items > q.peakItems {
		q.peakItems = items
	}
	if q.bytes > q.peakBytes {
		q.peakBytes = q.bytes
	}
	q.mu.Unlock()
	q.signal()
	return true
}

// full reports whether one more message would exceed a bound.
func (q *messageQueue) full(size int) bool {
	if len(q.items)-q.head >= q.maxItems {
		return true
	}
	// A single message larger than the byte budget must still be deliverable,
	// or a valid frame could never be consumed; the byte bound applies to
	// everything queued behind it.
	return q.bytes > 0 && q.bytes+int64(size) > q.maxBytes
}

// pop returns the oldest message, or false when the queue is empty. It never
// blocks.
func (q *messageQueue) pop() (Message, bool) {
	q.mu.Lock()
	if q.head >= len(q.items) {
		q.mu.Unlock()
		return nil, false
	}
	item := q.items[q.head]
	q.items[q.head] = queuedMessage{}
	q.head++
	q.bytes -= item.size
	if q.head == len(q.items) {
		q.items = q.items[:0]
		q.head = 0
	} else if q.head >= 64 && q.head*2 >= len(q.items) {
		// Compact once the consumed prefix is worth fewer bytes than a copy.
		n := copy(q.items, q.items[q.head:])
		q.items = q.items[:n]
		q.head = 0
	}
	q.mu.Unlock()
	q.room.Signal()
	return item.message, true
}

// close marks the queue closed with a terminal error, waking both sides. The
// first close wins: a terminal set by the reader is not overwritten by Close.
func (q *messageQueue) close(terminal error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.terminal = terminal
	q.mu.Unlock()
	q.room.Broadcast()
	q.signal()
}

// state reports whether the queue is closed (no more pushes will succeed) and
// the terminal error that ended it.
func (q *messageQueue) state() (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed, q.terminal
}

// stats is a snapshot for TransportStats.
func (q *messageQueue) stats() (items int, bytes, peakItems, peakBytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) - q.head, q.bytes, int64(q.peakItems), q.peakBytes
}

// signal wakes a blocked consumer; the channel has capacity one and a stale
// token only costs one extra loop iteration.
func (q *messageQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// outboundQueue is the writer-side FIFO: frames waiting to be written to a
// stdio peer.
//
// It exists because a write to a provider that never reads its stdin blocks
// forever, and a blocked write must not be able to wedge callers. The queue is
// bounded, push is context-aware, and the writer goroutine is the only place
// that can be stuck in the underlying write: a canceled or timed-out call
// therefore returns immediately instead of queueing up behind a wedged pipe.
type outboundQueue struct {
	mu       sync.Mutex
	frames   [][]byte
	bytes    int64
	maxItems int
	maxBytes int64
	closed   bool
	terminal error
	peak     int
	// itemReady and roomReady are closed and replaced on every state change, so
	// the writer (waiting for an item) and producers (waiting for room) each
	// wake exactly when their condition can have changed, with no lost wakeup.
	itemReady chan struct{}
	roomReady chan struct{}
}

// newOutboundQueue returns a queue with the given bounds; non-positive bounds
// fall back to the message-queue defaults.
func newOutboundQueue(maxItems int, maxBytes int64) *outboundQueue {
	if maxItems <= 0 {
		maxItems = DefaultMaxQueuedMessages
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxQueuedBytes
	}
	return &outboundQueue{
		maxItems:  maxItems,
		maxBytes:  maxBytes,
		itemReady: make(chan struct{}),
		roomReady: make(chan struct{}),
	}
}

func (q *outboundQueue) full(size int) bool {
	if len(q.frames) >= q.maxItems {
		return true
	}
	// A single frame larger than the byte budget must still be deliverable when
	// nothing is queued behind it, or a legal frame could never be sent.
	return q.bytes > 0 && q.bytes+int64(size) > q.maxBytes
}

// push enqueues one frame, blocking while the queue is full. It returns
// ctx.Err() when the context ends first, or the queue's terminal error once
// the queue is closed. A canceled push never enqueues the frame.
func (q *outboundQueue) push(ctx context.Context, frame []byte) error {
	for {
		q.mu.Lock()
		if q.closed {
			terminal := q.terminal
			q.mu.Unlock()
			if terminal != nil {
				return terminal
			}
			return &TransportError{Reason: "gateway: outbound queue closed", Err: ErrTransportClosed}
		}
		if !q.full(len(frame)) {
			q.frames = append(q.frames, frame)
			q.bytes += int64(len(frame))
			if len(q.frames) > q.peak {
				q.peak = len(q.frames)
			}
			// Replace the channel under the lock so exactly one goroutine closes it.
			itemReady := q.itemReady
			q.itemReady = make(chan struct{})
			q.mu.Unlock()
			close(itemReady) // wake the writer: an item is available
			return nil
		}
		wait := q.roomReady
		q.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pop returns the oldest frame, blocking until one is available or the queue
// closes. A closed queue reports its terminal error once drained.
func (q *outboundQueue) pop(ctx context.Context) ([]byte, error) {
	for {
		q.mu.Lock()
		if len(q.frames) > 0 {
			frame := q.frames[0]
			q.frames[0] = nil
			q.frames = q.frames[1:]
			q.bytes -= int64(len(frame))
			roomReady := q.roomReady
			q.roomReady = make(chan struct{})
			q.mu.Unlock()
			close(roomReady) // room freed: wake producers
			return frame, nil
		}
		if q.closed {
			terminal := q.terminal
			q.mu.Unlock()
			if terminal == nil {
				terminal = &TransportError{Reason: "gateway: outbound queue closed", Err: ErrTransportClosed}
			}
			return nil, terminal
		}
		wait := q.itemReady
		q.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// close rejects further pushes and unblocks the writer. The first close wins.
func (q *outboundQueue) close(terminal error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.terminal = terminal
	itemReady, roomReady := q.itemReady, q.roomReady
	q.itemReady = make(chan struct{})
	q.roomReady = make(chan struct{})
	q.mu.Unlock()
	close(itemReady)
	close(roomReady)
}

// stats is a snapshot for TransportStats.
func (q *outboundQueue) stats() (items, bytes, peak int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return int64(len(q.frames)), q.bytes, int64(q.peak)
}
