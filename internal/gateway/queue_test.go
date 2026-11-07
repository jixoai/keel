package gateway

import (
	"testing"
	"time"
)

func TestMessageQueueBoundsByCount(t *testing.T) {
	queue := newMessageQueue(2, 1<<20)
	first := &Notification{JSONRPC: JSONRPCVersion, Method: "first"}
	second := &Notification{JSONRPC: JSONRPCVersion, Method: "second"}
	third := &Notification{JSONRPC: JSONRPCVersion, Method: "third"}
	if !queue.push(first, 10) || !queue.push(second, 10) {
		t.Fatal("push within bounds failed")
	}
	pushed := make(chan bool, 1)
	go func() { pushed <- queue.push(third, 10) }()
	select {
	case <-pushed:
		t.Fatal("push beyond the count bound did not block")
	case <-time.After(50 * time.Millisecond):
	}
	message, ok := queue.pop()
	if !ok || message != Message(first) {
		t.Fatalf("pop = %v, %v; want first, true", message, ok)
	}
	select {
	case ok := <-pushed:
		if !ok {
			t.Fatal("push after a pop returned false")
		}
	case <-time.After(testTimeout):
		t.Fatal("push after a pop did not proceed")
	}
	items, bytes, peakItems, _ := queue.stats()
	if items != 2 || bytes != 20 {
		t.Errorf("stats = items %d bytes %d, want 2 and 20", items, bytes)
	}
	if peakItems != 2 {
		t.Errorf("peak items = %d, want 2", peakItems)
	}
}

func TestMessageQueueBoundsByBytes(t *testing.T) {
	queue := newMessageQueue(10, 100)
	if !queue.push(&Notification{Method: "big"}, 80) {
		t.Fatal("first push failed")
	}
	pushed := make(chan bool, 1)
	go func() { pushed <- queue.push(&Notification{Method: "next"}, 80) }()
	select {
	case <-pushed:
		t.Fatal("push beyond the byte bound did not block")
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok := queue.pop(); !ok {
		t.Fatal("pop failed")
	}
	select {
	case ok := <-pushed:
		if !ok {
			t.Fatal("push after the queue drained returned false")
		}
	case <-time.After(testTimeout):
		t.Fatal("push after the queue drained did not proceed")
	}
}

func TestMessageQueueAllowsASingleOversizeMessage(t *testing.T) {
	// A frame that is legal (under the frame cap) but larger than the queue's
	// byte budget must still be deliverable, or a valid frame could never be
	// consumed.
	queue := newMessageQueue(4, 64)
	if !queue.push(&Notification{Method: "large"}, 128) {
		t.Fatal("a single oversize message must be accepted when the queue is empty")
	}
	if _, ok := queue.pop(); !ok {
		t.Fatal("pop failed")
	}
}

func TestMessageQueueCloseDrainsAndUnblocks(t *testing.T) {
	queue := newMessageQueue(4, 1<<20)
	queue.push(&Notification{Method: "queued"}, 5)
	blocked := make(chan bool, 1)
	go func() { blocked <- queue.push(&Notification{Method: "late"}, 5) }()
	queue.close(errTerminalTest)

	select {
	case ok := <-blocked:
		if ok {
			t.Fatal("push after close returned true")
		}
	case <-time.After(testTimeout):
		t.Fatal("close did not unblock a blocked push")
	}
	message, ok := queue.pop()
	if !ok || message == nil {
		t.Fatal("close dropped an already queued message")
	}
	if _, ok := queue.pop(); ok {
		t.Fatal("queue not empty after draining")
	}
	closed, terminal := queue.state()
	if !closed || terminal != errTerminalTest {
		t.Fatalf("state = %v, %v; want closed with the terminal error", closed, terminal)
	}
	// The first close wins.
	queue.close(nil)
	if _, terminal := queue.state(); terminal != errTerminalTest {
		t.Fatal("a second close overwrote the terminal error")
	}
}

var errTerminalTest = &TransportError{Reason: "test terminal"}
