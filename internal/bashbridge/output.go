package bashbridge

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// Default bounded-output window. The model sees at most HeadLimit+TailLimit
// bytes per stream; the full stream goes to the caller's blob store. The
// defaults are deliberately small: two streams per call, and the tool result
// must stay bounded regardless of how much a command prints.
const (
	DefaultHeadLimit = 4 << 10
	DefaultTailLimit = 4 << 10
)

// Bounded retains a fixed-size head and a fixed-size tail of a stream while
// counting every byte written through it. It never grows with the stream, so a
// 200MB output costs the same memory as a 2KB one.
//
// It implements io.Writer and is safe for one writer and concurrent readers
// (the caller reads a snapshot while the command is still running).
type Bounded struct {
	mu        sync.Mutex
	headLimit int
	tailLimit int
	head      []byte
	tail      []byte // ring buffer, allocated once
	tailStart int    // index of the oldest byte once full
	tailLen   int
	total     int64
}

// NewBounded builds a bounded buffer. Non-positive limits fall back to the
// defaults.
func NewBounded(headLimit, tailLimit int) *Bounded {
	if headLimit <= 0 {
		headLimit = DefaultHeadLimit
	}
	if tailLimit <= 0 {
		tailLimit = DefaultTailLimit
	}
	return &Bounded{
		headLimit: headLimit,
		tailLimit: tailLimit,
		head:      make([]byte, 0, headLimit),
		tail:      make([]byte, tailLimit),
	}
}

// Write implements io.Writer. It never fails and never retains more than the
// configured windows.
func (b *Bounded) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.head) < b.headLimit {
		take := b.headLimit - len(b.head)
		if take > n {
			take = n
		}
		b.head = append(b.head, p[:take]...)
	}
	for _, chunk := range p {
		if b.tailLen < b.tailLimit {
			index := (b.tailStart + b.tailLen) % b.tailLimit
			b.tail[index] = chunk
			b.tailLen++
			continue
		}
		b.tail[b.tailStart] = chunk
		b.tailStart = (b.tailStart + 1) % b.tailLimit
	}
	b.total += int64(n)
	return n, nil
}

// Snapshot returns the retained head and tail as text plus the total byte
// count. When the whole stream fits in the head window the tail is empty; when
// the stream is only slightly longer than the head window, the overlapping
// bytes are trimmed from the tail so head and tail never repeat content.
func (b *Bounded) Snapshot() (head, tail string, total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total = b.total
	head = validUTF8(b.head)
	if int64(len(b.head)) < b.total {
		tailBytes := b.tailBytesLocked()
		// tailBytes is the last min(total, tailLimit) bytes. If fewer than
		// tailLimit bytes follow the head, keep only that suffix.
		keep := b.total - int64(len(b.head))
		if keep < int64(len(tailBytes)) {
			tailBytes = tailBytes[int64(len(tailBytes))-keep:]
		}
		tail = validUTF8(tailBytes)
	}
	return head, tail, total
}

// Total returns the number of bytes written.
func (b *Bounded) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func (b *Bounded) tailBytesLocked() []byte {
	if b.tailLen < b.tailLimit {
		return b.tail[:b.tailLen]
	}
	out := make([]byte, b.tailLimit)
	n := copy(out, b.tail[b.tailStart:])
	copy(out[n:], b.tail[:b.tailStart])
	return out
}

// validUTF8 converts arbitrary bytes into valid UTF-8, replacing invalid
// sequences. A truncated multibyte rune at a window edge must not corrupt the
// JSON document the model reads.
func validUTF8(raw []byte) string {
	value := strings.ToValidUTF8(string(raw), "\uFFFD")
	if len(value) > 0 && !utf8.ValidString(value) {
		// ToValidUTF8 guarantees validity; keep the guard so a future change
		// cannot put invalid bytes into a tool result.
		return strings.ToValidUTF8(value, "\uFFFD")
	}
	return value
}
