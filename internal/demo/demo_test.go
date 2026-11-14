package demo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEnablementTokens pins the switch: only an affirmative token turns the demo
// provider on, so a typo or an explicit "0" leaves a production deployment alone.
func TestEnablementTokens(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", " yes ", "on", "\tOn\n"} {
		if !Enabled(value) {
			t.Fatalf("%q must enable the demo provider", value)
		}
	}
	for _, value := range []string{"", " ", "0", "false", "no", "off", "enabled", "demo", "yes-please"} {
		if Enabled(value) {
			t.Fatalf("%q must not enable the demo provider", value)
		}
	}
	// The lookup-shaped helpers agree with the raw parser, and a nil lookup (or a
	// nil getenv) is off: a missing configuration is not an enablement.
	if EnabledFromGetenv(nil) || EnabledFromLookup(nil) {
		t.Fatal("a nil lookup must read as disabled")
	}
	env := map[string]string{Env: "1"}
	if !EnabledFromGetenv(func(name string) string { return env[name] }) {
		t.Fatal("EnabledFromGetenv must read the shared variable")
	}
	if !EnabledFromLookup(func(name string) (string, bool) { value, ok := env[name]; return value, ok }) {
		t.Fatal("EnabledFromLookup must read the shared variable")
	}
}

// TestIdentityVocabulary pins the model/family mapping: six models, each mapped to
// exactly one family, and no vendor id accepted.
func TestIdentityVocabulary(t *testing.T) {
	ids := ModelIDs()
	if len(ids) != 6 {
		t.Fatalf("model ids = %d, want 6", len(ids))
	}
	want := map[string]string{
		ModelLLM:      FamilyLLM,
		ModelImage:    FamilyImage,
		ModelDecision: FamilyDecision,
		ModelRealtime: FamilyRealtime,
		ModelTTS:      FamilyTTS,
		ModelSTT:      FamilySTT,
	}
	seen := make(map[string]struct{}, len(ids))
	for _, modelID := range ids {
		if _, duplicate := seen[modelID]; duplicate {
			t.Fatalf("%s is listed twice", modelID)
		}
		seen[modelID] = struct{}{}
		family, ok := Family(modelID)
		if !ok {
			t.Fatalf("%s must map to a family", modelID)
		}
		if family != want[modelID] {
			t.Fatalf("%s family = %q, want %q", modelID, family, want[modelID])
		}
		if !FamilyInModelIDs(modelID) {
			t.Fatalf("FamilyInModelIDs(%q) = false", modelID)
		}
		if !FamilyInModelIDs(" " + modelID + " ") {
			t.Fatalf("FamilyInModelIDs must tolerate padding around %q", modelID)
		}
	}
	for _, foreign := range []string{"gpt-5", "demo-image-x", "", "doc-demo-llm"} {
		if _, ok := Family(foreign); ok {
			t.Fatalf("Family(%q) must not resolve", foreign)
		}
	}
	if !IsProvider(" demo ") || !IsProvider("DEMO") {
		t.Fatal("IsProvider must accept the provider id regardless of case and padding")
	}
	if IsProvider("demo-mode") || IsProvider("") {
		t.Fatal("IsProvider must refuse any other id")
	}
}

// TestReplyChunksReconstructTheText pins the streaming content: the chunks split on
// rune boundaries and their concatenation is exactly the reply.
func TestReplyChunksReconstructTheText(t *testing.T) {
	chunks := ReplyChunks()
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want several (the reply must stream)", len(chunks))
	}
	var rebuilt strings.Builder
	for _, chunk := range chunks {
		if chunk == "" {
			t.Fatal("a chunk must not be empty")
		}
		// A chunk that is not valid UTF-8 would corrupt the transcript.
		for _, r := range chunk {
			if r == '\uFFFD' {
				t.Fatalf("chunk %q carries a replacement rune", chunk)
			}
		}
		rebuilt.WriteString(chunk)
	}
	if rebuilt.String() != ReplyText {
		t.Fatalf("rebuilt text = %q, want the reply", rebuilt.String())
	}
	if strings.Contains(ReplyText, "demo") || strings.Contains(ReplyText, "演示") {
		t.Fatalf("the reply text must read as ordinary content, got %q", ReplyText)
	}
}

// TestWireChunksAreTheOpenAIShape pins the streamed chunk sequence: a role chunk,
// one content chunk per delta, a terminal chunk carrying finish_reason and usage —
// the shape the shared decoder consumes.
func TestWireChunksAreTheOpenAIShape(t *testing.T) {
	chunks := WireChunks(ModelLLM)
	deltas := ReplyChunks()
	if got, want := len(chunks), len(deltas)+2; got != want {
		t.Fatalf("chunks = %d, want %d (role + deltas + terminal)", got, want)
	}

	type choice struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	type chunk struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
		Usage   *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	decode := func(raw json.RawMessage) chunk {
		var out chunk
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("chunk is not JSON: %v (%s)", err, raw)
		}
		return out
	}

	first := decode(chunks[0])
	if first.Object != "chat.completion.chunk" || first.Model != ModelLLM || first.ID != ResponseID {
		t.Fatalf("first chunk envelope = %+v", first)
	}
	if len(first.Choices) != 1 || first.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk must carry the assistant role: %+v", first.Choices)
	}
	if first.Created != createdSeconds {
		t.Fatalf("created = %d, want the fixed timestamp %d", first.Created, createdSeconds)
	}

	for i, delta := range deltas {
		got := decode(chunks[i+1])
		if len(got.Choices) != 1 || got.Choices[0].Delta.Content != delta {
			t.Fatalf("chunk %d content = %+v, want %q", i+1, got.Choices, delta)
		}
		if got.Choices[0].FinishReason != nil {
			t.Fatalf("chunk %d must not carry a finish reason", i+1)
		}
	}

	terminal := decode(chunks[len(chunks)-1])
	if len(terminal.Choices) != 1 || terminal.Choices[0].FinishReason == nil || *terminal.Choices[0].FinishReason != "stop" {
		t.Fatalf("terminal chunk = %+v, want finish_reason stop", terminal.Choices)
	}
	if terminal.Usage == nil || terminal.Usage.CompletionTokens != len([]rune(ReplyText)) {
		t.Fatalf("terminal usage = %+v, want %d completion tokens", terminal.Usage, len([]rune(ReplyText)))
	}

	// Determinism: the same model id yields byte-identical chunks.
	again := WireChunks(ModelLLM)
	if len(again) != len(chunks) {
		t.Fatalf("second call produced %d chunks, want %d", len(again), len(chunks))
	}
	for i := range chunks {
		if !bytes.Equal(chunks[i], again[i]) {
			t.Fatalf("chunk %d is not reproducible:\n%s\n%s", i, chunks[i], again[i])
		}
	}

	// An unknown model id is normalized onto the LLM model, so a payload is always
	// well formed; the driver refuses the model before this is reached.
	if decode(WireChunks("not-a-demo-model")[0]).Model != ModelLLM {
		t.Fatal("an unknown model id must normalize to the LLM model")
	}
}

// TestCompletionIsTheSingleObjectShape pins the non-streaming body.
func TestCompletionIsTheSingleObjectShape(t *testing.T) {
	var body struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(Completion(ModelLLM), &body); err != nil {
		t.Fatalf("completion is not JSON: %v", err)
	}
	if body.Object != "chat.completion" || body.Model != ModelLLM {
		t.Fatalf("completion envelope = %+v", body)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != ReplyText ||
		body.Choices[0].Message.Role != "assistant" || body.Choices[0].FinishReason != "stop" {
		t.Fatalf("completion choices = %+v", body.Choices)
	}
}

// TestSSEBodyIsTheWireShape pins the streaming body's transport form: one
// "data: <json>" line per chunk, blank-line separated, terminated by [DONE].
func TestSSEBodyIsTheWireShape(t *testing.T) {
	body := string(SSE(ModelLLM))
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("SSE body does not end with the terminator: %q", body[len(body)-40:])
	}
	// Drop the [DONE] terminator (with its trailing blank line) and the body's
	// final blank line, leaving exactly one entry per chunk.
	chunks := strings.TrimSuffix(body, "data: [DONE]\n\n")
	events := strings.Split(strings.TrimSuffix(chunks, "\n\n"), "\n\n")
	if len(events) != len(WireChunks(ModelLLM)) {
		t.Fatalf("SSE events = %d, want %d", len(events), len(WireChunks(ModelLLM)))
	}
	for _, event := range events {
		payload, ok := strings.CutPrefix(event, "data: ")
		if !ok {
			t.Fatalf("SSE line is not a data line: %q", event)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("SSE payload is not JSON: %v (%s)", err, payload)
		}
	}
}

// TestAssetsAreRealPNGsOfTheDeclaredSize pins the hosted images: three tiles, each
// a real PNG whose IHDR declares the size the answer reports.
func TestAssetsAreRealPNGsOfTheDeclaredSize(t *testing.T) {
	assets := Assets()
	if len(assets) != 3 {
		t.Fatalf("assets = %d, want 3", len(assets))
	}
	seen := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if _, duplicate := seen[asset.Name]; duplicate {
			t.Fatalf("%s is listed twice", asset.Name)
		}
		seen[asset.Name] = struct{}{}
		if len(asset.Bytes) < 33 || string(asset.Bytes[:8]) != "\x89PNG\r\n\x1a\n" {
			t.Fatalf("%s is not a PNG (len %d)", asset.Name, len(asset.Bytes))
		}
		if tag := string(asset.Bytes[12:16]); tag != "IHDR" {
			t.Fatalf("%s first chunk = %q, want IHDR", asset.Name, tag)
		}
		width := int(asset.Bytes[16])<<24 | int(asset.Bytes[17])<<16 | int(asset.Bytes[18])<<8 | int(asset.Bytes[19])
		height := int(asset.Bytes[20])<<24 | int(asset.Bytes[21])<<16 | int(asset.Bytes[22])<<8 | int(asset.Bytes[23])
		if width != asset.Width || height != asset.Height {
			t.Fatalf("%s IHDR says %dx%d, reported %dx%d", asset.Name, width, height, asset.Width, asset.Height)
		}
		if asset.Width <= 0 || asset.Height <= 0 {
			t.Fatalf("%s has a non-positive size", asset.Name)
		}
	}

	// The accessor copies: mutating the result must not corrupt the compiled-in data.
	mutated := Assets()
	mutated[0].Bytes[0] = 0
	if Assets()[0].Bytes[0] == 0 {
		t.Fatal("Assets() must return copies")
	}
}

// TestAssetSelectionBySize pins the deterministic tile choice.
func TestAssetSelectionBySize(t *testing.T) {
	cases := map[string]string{
		"1024x1024": AssetSquare,
		"1024x1536": AssetPortrait,
		"1536x1024": AssetLandscape,
		"":          AssetSquare, // no size: the square tile
		"bogus":     AssetSquare, // unreadable: the square tile
	}
	for size, want := range cases {
		if got := AssetForSize(size).Name; got != want {
			t.Fatalf("AssetForSize(%q) = %q, want %q", size, got, want)
		}
	}
	for _, name := range []string{AssetSquare, AssetPortrait, AssetLandscape} {
		if _, ok := AssetByName(name); !ok {
			t.Fatalf("AssetByName(%q) must resolve", name)
		}
		if _, ok := AssetByName(name + ".png"); !ok {
			t.Fatalf("AssetByName must accept the .png suffix for %q", name)
		}
	}
	for _, name := range []string{"", "nope", "../etc/passwd", "square/../portrait"} {
		if _, ok := AssetByName(name); ok {
			t.Fatalf("AssetByName(%q) must not resolve", name)
		}
	}
	if got := AssetURL(AssetPortrait); got != "/v1/demo/assets/portrait.png" {
		t.Fatalf("AssetURL = %q", got)
	}
	if AssetPathPrefix+AssetPortrait+".png" != AssetURL(AssetPortrait) {
		t.Fatal("AssetPathPrefix and AssetURL disagree")
	}
}

// TestSpeechWAVIsAValidMonoPCMClip pins the synthesized audio: a canonical WAV
// container carrying 16 kHz mono 16-bit PCM, reproducible byte for byte, bounded,
// and starting/ending at silence (no click).
func TestSpeechWAVIsAValidMonoPCMClip(t *testing.T) {
	clip := SpeechWAV()
	if !SpeechWAVLooksValid(clip) {
		t.Fatal("the clip is not a RIFF container")
	}
	if string(clip[8:12]) != "WAVE" || string(clip[12:16]) != "fmt " || string(clip[36:40]) != "data" {
		t.Fatalf("unexpected WAV chunk layout: % x", clip[:44])
	}
	readU16 := func(offset int) int { return int(clip[offset]) | int(clip[offset+1])<<8 }
	readU32 := func(offset int) int {
		return int(clip[offset]) | int(clip[offset+1])<<8 | int(clip[offset+2])<<16 | int(clip[offset+3])<<24
	}
	if format := readU16(20); format != 1 {
		t.Fatalf("format = %d, want 1 (PCM)", format)
	}
	if channels := readU16(22); channels != AudioChannels {
		t.Fatalf("channels = %d, want %d", channels, AudioChannels)
	}
	if rate := readU32(24); rate != AudioSampleRate {
		t.Fatalf("sample rate = %d, want %d", rate, AudioSampleRate)
	}
	if bits := readU16(34); bits != AudioBitsPerSample {
		t.Fatalf("bits per sample = %d, want %d", bits, AudioBitsPerSample)
	}
	if declared := readU32(40); declared != len(clip)-44 {
		t.Fatalf("data chunk length = %d, want %d", declared, len(clip)-44)
	}
	wantBytes := AudioSampleRate * audioToneSeconds * AudioChannels * AudioBitsPerSample / 8
	if len(clip)-44 != wantBytes {
		t.Fatalf("pcm bytes = %d, want %d", len(clip)-44, wantBytes)
	}

	// The clip is bounded and non-silent, and its first/last samples are zero.
	const maxBytes = 200 << 10
	if len(clip) > maxBytes {
		t.Fatalf("clip is %d bytes, want under %d", len(clip), maxBytes)
	}
	first := int16(uint16(readU16(44)))
	last := int16(uint16(readU16(len(clip) - 2)))
	if first != 0 || last != 0 {
		t.Fatalf("clip edges = %d/%d, want silence (no click)", first, last)
	}
	peak := 0
	for offset := 44; offset+1 < len(clip); offset += 2 {
		sample := int(int16(uint16(readU16(offset))))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	if peak == 0 {
		t.Fatal("the clip is silent")
	}
	// The declared amplitude is a quarter of full scale; the tolerance absorbs the
	// small series approximation in the sine helper without letting a full-scale
	// (startling) clip pass.
	amplitude := audioAmplitude // a variable, so the int conversion is a runtime one
	if ceiling := int(amplitude*32767)*2 + 2; peak > ceiling {
		t.Fatalf("peak amplitude = %d, want at most %d (a quarter-scale tone)", peak, ceiling)
	}

	// Determinism: two calls are byte-identical (no clock, no randomness).
	if !bytes.Equal(clip, SpeechWAV()) {
		t.Fatal("SpeechWAV is not deterministic")
	}
}

// TestTranscriptionIsContent pins that the fixed transcript reads as content: it
// names no mode the client would have to render specially.
func TestTranscriptionIsContent(t *testing.T) {
	if strings.TrimSpace(TranscriptionText) == "" {
		t.Fatal("the demo transcript must not be empty")
	}
	for _, banned := range []string{"demo", "演示"} {
		if strings.Contains(TranscriptionText, banned) {
			t.Fatalf("the transcript must read as ordinary content, got %q", TranscriptionText)
		}
	}
}
