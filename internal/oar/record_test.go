package oar

import (
	"encoding/json"
	"testing"
	"time"
)

func validRecord(kind RecordKind) Record {
	record := Record{
		Schema:    RecordSchema,
		Seq:       1,
		AgentPath: RootAgentPath,
		SessionID: "oar-1",
		Kind:      kind,
		Status:    StatusStarted,
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	switch kind {
	case KindCall:
		record.CallID = "call-1"
	case KindResult:
		record.CallID = "call-1"
		record.Output = json.RawMessage(`{"ok":true}`)
	case KindMessage:
		record.Message = "hello"
	case KindUsage:
		record.Usage = &RecordUsage{Metering: "external"}
	case KindError:
		record.Error = &RecordError{Code: "x", Message: "bad"}
	case KindStatus:
		record.Status = StatusCompleted
	}
	return record
}

func TestRecordValidationAcceptsEveryKind(t *testing.T) {
	for _, kind := range KnownRecordKinds() {
		record := validRecord(kind)
		if err := record.Validate(); err != nil {
			t.Fatalf("%s record is invalid: %v", kind, err)
		}
	}
}

func TestRecordValidationRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Record)
	}{
		{"schema", func(r *Record) { r.Schema = "other" }},
		{"seq", func(r *Record) { r.Seq = 0 }},
		{"agent path", func(r *Record) { r.AgentPath = "main" }},
		{"agent path traversal", func(r *Record) { r.AgentPath = "/main/../x" }},
		{"session", func(r *Record) { r.SessionID = "" }},
		{"kind", func(r *Record) { r.Kind = "telepathy" }},
		{"timestamp", func(r *Record) { r.Timestamp = time.Time{} }},
		{"call id", func(r *Record) { r.CallID = "" }},
		{"bad status", func(r *Record) { r.Status = "exploded" }},
		{"error message", func(r *Record) { r.Kind = KindError; r.Error = &RecordError{Code: "x"} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record := validRecord(KindCall)
			test.mutate(&record)
			err := record.Validate()
			if err == nil {
				t.Fatalf("record was accepted: %+v", record)
			}
			if !IsCode(err, CodeInvalidRecord) && !IsCode(err, CodeRecordTooLarge) {
				t.Fatalf("error is not typed: %v", err)
			}
		})
	}
}

func TestResultRequiresOutputOrError(t *testing.T) {
	record := validRecord(KindResult)
	record.Output = nil
	if err := record.Validate(); !IsCode(err, CodeInvalidRecord) {
		t.Fatalf("result without output: %v", err)
	}
}

func TestSeqTracker(t *testing.T) {
	tracker := NewSeqTracker()
	for _, seq := range []int64{1, 2, 4} {
		duplicate, err := tracker.Observe("s", seq)
		if err != nil || duplicate {
			t.Fatalf("seq %d: duplicate=%v err=%v", seq, duplicate, err)
		}
	}
	if tracker.Gaps() != 1 {
		t.Fatalf("gaps = %d, want 1", tracker.Gaps())
	}
	duplicate, err := tracker.Observe("s", 4)
	if err != nil || !duplicate {
		t.Fatalf("duplicate: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := tracker.Observe("s", 3); !IsCode(err, CodeRecordOrder) {
		t.Fatalf("regression: %v", err)
	}
}

func TestChildAgentPath(t *testing.T) {
	if got := ChildAgentPath(RootAgentPath, "worker-1"); got != "/main/worker-1" {
		t.Fatalf("got %q", got)
	}
	if got := ChildAgentPath("", "/child"); got != "/main/child" {
		t.Fatalf("got %q", got)
	}
}
