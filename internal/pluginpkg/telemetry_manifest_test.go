package pluginpkg

import "testing"

func telemetryManifest(exporters, signals []string) *Manifest {
	return &Manifest{
		ID:          "vendor.telemetry",
		Version:     "1.0.0",
		Kind:        KindInProcess,
		Description: "telemetry exporter",
		Provides:    Provides{Telemetry: &TelemetryDecl{Exporters: exporters, Signals: signals}},
	}
}

func TestProvidesTelemetryClosedVocabulary(t *testing.T) {
	valid := telemetryManifest([]string{"otlp-http"}, []string{"traces", "metrics", "logs"})
	if err := valid.Validate(Environment{}); err != nil {
		t.Fatalf("valid telemetry manifest = %v", err)
	}
	cases := []struct {
		name      string
		exporters []string
		signals   []string
		field     string
	}{
		{"unknown exporter", []string{"otlp-grpc"}, []string{"traces"}, "provides.telemetry.exporters[0]"},
		{"no exporter", nil, []string{"traces"}, "provides.telemetry.exporters"},
		{"unknown signal", []string{"otlp-http"}, []string{"profiles"}, "provides.telemetry.signals[0]"},
		{"no signal", []string{"otlp-http"}, nil, "provides.telemetry.signals"},
		{"duplicate signal", []string{"otlp-http"}, []string{"traces", "traces"}, "provides.telemetry.signals[1]"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manifest := telemetryManifest(test.exporters, test.signals)
			err := manifest.Validate(Environment{})
			if err == nil {
				t.Fatal("invalid provides.telemetry was accepted")
			}
			found := false
			for _, field := range FieldErrors(err) {
				if field.Field == test.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("fields = %#v, want %s", FieldErrors(err), test.field)
			}
		})
	}
}

func TestProvidesTelemetryRejectsUnknownFields(t *testing.T) {
	raw := []byte(`id: vendor.telemetry
version: 1.0.0
kind: in-process
description: telemetry
provides:
  telemetry:
    exporters: [otlp-http]
    signals: [traces]
    endpoint: https://collector.example
`)
	if _, err := ParseManifest(raw); err == nil {
		t.Fatal("an unknown provides.telemetry field was accepted")
	}
}
