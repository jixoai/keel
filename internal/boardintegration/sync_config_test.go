package boardintegration

import "testing"

// TestSyncEntryInputSecretRefSpellings covers both the API's camelCase and the
// design document's secret_ref spelling. Two spellings in one body is a 400,
// not a silent merge.
func TestSyncEntryInputSecretRefSpellings(t *testing.T) {
	camel, err := parseSyncEntryInput([]byte(`{"key":"slack","platform":"slack","secretRef":{"signingSecret":"S"}}`))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := camel.secretRef()
	if err != nil || ref["signingSecret"] != "S" {
		t.Fatalf("camel secretRef = %+v/%v", ref, err)
	}
	snake, err := parseSyncEntryInput([]byte(`{"key":"slack","platform":"slack","secret_ref":{"signingSecret":"S"}}`))
	if err != nil {
		t.Fatal(err)
	}
	ref, err = snake.secretRef()
	if err != nil || ref["signingSecret"] != "S" {
		t.Fatalf("snake secret_ref = %+v/%v", ref, err)
	}
	both, err := parseSyncEntryInput([]byte(`{"key":"slack","platform":"slack","secretRef":{"signingSecret":"S"},"secret_ref":{"signingSecret":"T"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := both.secretRef(); err == nil {
		t.Fatal("both spellings were accepted")
	}
}
