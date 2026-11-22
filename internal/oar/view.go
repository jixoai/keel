package oar

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

// ViewOptions filters a record stream for the developer CLI.
type ViewOptions struct {
	AfterSeq  int64
	AgentPath string
	Kind      RecordKind
	SessionID string
}

// FilterRecords applies the CLI filters while preserving order.
func FilterRecords(records []Record, opts ViewOptions) []Record {
	out := make([]Record, 0, len(records))
	for _, record := range records {
		if opts.AfterSeq > 0 && record.Seq <= opts.AfterSeq {
			continue
		}
		if opts.AgentPath != "" && record.AgentPath != opts.AgentPath {
			continue
		}
		if opts.Kind != "" && record.Kind != opts.Kind {
			continue
		}
		if opts.SessionID != "" && record.SessionID != opts.SessionID {
			continue
		}
		out = append(out, record)
	}
	return out
}

// LoadRecords reads one journal file, or every *.ndjson file in a directory
// (sorted by name). Directory mode is for the developer who does not remember
// which backend session file to open.
func LoadRecords(path string) ([]Record, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("oar: open journal %s: %w", path, err)
	}
	if !info.IsDir() {
		return ReadJournal(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("oar: read journal directory %s: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	var records []Record
	for _, name := range names {
		fileRecords, err := ReadJournal(filepath.Join(path, name))
		if err != nil {
			return nil, err
		}
		records = append(records, fileRecords...)
	}
	return records, nil
}

// WriteTable renders a record stream for a terminal. It is deliberately plain:
// the renderer seam for the product is Projection, not this table.
func WriteTable(w io.Writer, records []Record) error {
	writer := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "SEQ\tAGENT\tKIND\tNAME\tSTATUS\tCALL\tSUMMARY"); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			record.Seq,
			record.AgentPath,
			record.Kind,
			record.Name,
			record.Status,
			record.CallID,
			summary(record),
		); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// WriteRecordsJSON writes the filtered records as a JSON array.
func WriteRecordsJSON(w io.Writer, records []Record) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(records)
}

// summary is the one-line description of a record for the table view.
func summary(record Record) string {
	switch record.Kind {
	case KindMessage:
		return record.Message
	case KindUsage:
		if record.Usage == nil {
			return ""
		}
		return fmt.Sprintf("%s in=%d out=%d total=%d", record.Usage.Model, record.Usage.InputTokens, record.Usage.OutputTokens, record.Usage.TotalTokens)
	case KindError:
		if record.Error != nil {
			return record.Error.Message
		}
		return record.Message
	case KindStatus:
		if record.Message != "" {
			return record.Message
		}
		return record.Status
	default:
		if len(record.Output) > 0 {
			return truncateRunes(string(record.Output), 96)
		}
		if len(record.Input) > 0 {
			return truncateRunes(string(record.Input), 96)
		}
		return record.Message
	}
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}
