// Command oar-view inspects a durable OAR record journal. It is the developer
// view of the stream: table by default, JSON with -format=json, and the stable
// renderer projection with -projection (the same document the W12
// tool-renderer slot consumes).
//
// Usage:
//
//	oar-view -journal .keel/oar
//	oar-view -journal .keel/oar -agent /main/worker-1 -after 4
//	oar-view -journal .keel/oar/session-oar-1.ndjson -projection
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jixoai/keel/internal/oar"
)

func main() {
	journal := flag.String("journal", "", "journal file or directory (required)")
	session := flag.String("session", "", "filter by backend session id")
	agent := flag.String("agent", "", "filter by agentPath")
	kind := flag.String("kind", "", "filter by record kind (call|result|status|message|usage|error)")
	after := flag.Int64("after", 0, "only records with seq greater than this")
	format := flag.String("format", "table", "output format: table|json")
	projection := flag.Bool("projection", false, "emit the renderer projection instead of raw records")
	flag.Parse()

	if *journal == "" {
		fmt.Fprintln(os.Stderr, "oar-view: -journal is required")
		os.Exit(2)
	}
	records, err := oar.LoadRecords(*journal)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oar-view:", err)
		os.Exit(1)
	}
	filtered := oar.FilterRecords(records, oar.ViewOptions{
		AfterSeq:  *after,
		AgentPath: *agent,
		Kind:      oar.RecordKind(*kind),
		SessionID: *session,
	})
	if *projection {
		raw, err := oar.ProjectionJSON(oar.BuildProjection(filtered))
		if err != nil {
			fmt.Fprintln(os.Stderr, "oar-view:", err)
			os.Exit(1)
		}
		if _, err := os.Stdout.Write(append(raw, '\n')); err != nil {
			fmt.Fprintln(os.Stderr, "oar-view:", err)
			os.Exit(1)
		}
		return
	}
	switch *format {
	case "table":
		err = oar.WriteTable(os.Stdout, filtered)
	case "json":
		err = oar.WriteRecordsJSON(os.Stdout, filtered)
	default:
		fmt.Fprintf(os.Stderr, "oar-view: unknown -format %q (want table|json)\n", *format)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "oar-view:", err)
		os.Exit(1)
	}
}
