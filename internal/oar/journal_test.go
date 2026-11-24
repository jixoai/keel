package oar

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func journalRecord(seq int64, text string) Record {
	return Record{
		Schema:    RecordSchema,
		Seq:       seq,
		AgentPath: RootAgentPath,
		SessionID: "oar-1",
		Kind:      KindMessage,
		Message:   text,
		Timestamp: timeForSeq(seq),
	}
}

func timeForSeq(seq int64) (value time.Time) {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Millisecond)
}

func TestJournalAppendReadAndReopen(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(dir, JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 3; seq++ {
		appended, err := journal.Append(journalRecord(seq, "record"))
		if err != nil || !appended {
			t.Fatalf("append %d: appended=%v err=%v", seq, appended, err)
		}
	}
	path := journal.Path("oar-1")
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[2].Seq != 3 {
		t.Fatalf("records = %+v", records)
	}

	reopened, err := NewJournal(dir, JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	last, err := reopened.LastSeq("oar-1")
	if err != nil || last != 3 {
		t.Fatalf("last seq = %d, err = %v", last, err)
	}
	appended, err := reopened.Append(journalRecord(4, "record"))
	if err != nil || !appended {
		t.Fatalf("append after reopen: appended=%v err=%v", appended, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalDuplicateIsIdempotentAndRegressionIsTyped(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	record := journalRecord(1, "same")
	if appended, err := journal.Append(record); err != nil || !appended {
		t.Fatalf("first append: appended=%v err=%v", appended, err)
	}
	duplicate := record
	if appended, err := journal.Append(duplicate); err != nil || appended {
		t.Fatalf("duplicate append: appended=%v err=%v", appended, err)
	}
	changed := record
	changed.Message = "different"
	if _, err := journal.Append(changed); !IsCode(err, CodeRecordOrder) {
		t.Fatalf("reused seq with different content: %v", err)
	}
	if _, err := journal.Append(journalRecord(2, "next")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(journalRecord(1, "old")); !IsCode(err, CodeRecordOrder) {
		t.Fatalf("regression: %v", err)
	}
}

func TestJournalFileNameIsSafe(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	safe := filepath.Base(journal.Path("oar-1"))
	if !strings.HasPrefix(safe, "session-oar-1") || !strings.HasSuffix(safe, ".ndjson") {
		t.Fatalf("safe path = %q", safe)
	}
	unsafe := filepath.Base(journal.Path("../../etc/passwd"))
	if strings.Contains(unsafe, "/") || strings.Contains(unsafe, "..") {
		t.Fatalf("unsafe path escaped: %q", unsafe)
	}
	if !strings.HasSuffix(unsafe, ".ndjson") {
		t.Fatalf("unsafe path = %q", unsafe)
	}
	// The read-side helper (the product projection route) locates exactly the
	// file the host journal writes, including the sanitization above.
	if JournalPath(journal.Dir(), "oar-1") != journal.Path("oar-1") {
		t.Fatalf("JournalPath = %q, want %q", JournalPath(journal.Dir(), "oar-1"), journal.Path("oar-1"))
	}
	if JournalPath(journal.Dir(), "../../etc/passwd") != journal.Path("../../etc/passwd") {
		t.Fatalf("JournalPath sanitization drifted for an unsafe reference")
	}
}

func TestParseJournalRejectsCorruption(t *testing.T) {
	if _, err := ParseJournal([]byte("{not json}\n")); err == nil {
		t.Fatal("corrupt journal was accepted")
	}
}

func TestReadJournalMissingFile(t *testing.T) {
	if _, err := ReadJournal(filepath.Join(t.TempDir(), "missing.ndjson")); err == nil {
		t.Fatal("missing journal was accepted")
	}
}
