package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/responses"

	"github.com/jixoai/keel/internal/agentcore"
)

// Usage decoding: only fields the upstream actually returns are mapped; any
// absent counter stays zero. Reasoning tokens are reported, never subtracted
// from output tokens (providers include them there).

func assertUsage(t *testing.T, usage agentcore.Usage, input, output, cacheRead, cacheWrite, reasoning int) {
	t.Helper()
	if usage.InputTokens != input || usage.OutputTokens != output ||
		usage.CacheReadTokens != cacheRead || usage.CacheWriteTokens != cacheWrite ||
		usage.ReasoningTokens != reasoning {
		t.Fatalf("usage = %+v, want input=%d output=%d cacheRead=%d cacheWrite=%d reasoning=%d",
			usage, input, output, cacheRead, cacheWrite, reasoning)
	}
}

func TestOpenAIDecoderNormalizesCacheSubsetsAndMapsReasoning(t *testing.T) {
	d := NewOpenAIDecoder()
	payload := []byte(`{
	  "id": "chatcmpl-1",
	  "model": "gpt-5",
	  "choices": [{"delta": {"content": "hi"}}],
	  "usage": {
	    "prompt_tokens": 100,
	    "completion_tokens": 40,
	    "prompt_tokens_details": {"cached_tokens": 30, "cache_write_tokens": 20},
	    "completion_tokens_details": {"reasoning_tokens": 10}
	  }
	}`)
	events, err := d.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	var partial *agentcore.Usage
	for _, event := range events {
		if e, ok := event.(StreamTextEvent); ok {
			partial = e.Partial.Usage
		}
	}
	if partial == nil {
		t.Fatal("text event carried no usage")
	}
	// prompt_tokens includes both cache subsets; input is the exclusive
	// remainder (100 - 30 - 20), so input+cache_read+cache_write = 100.
	assertUsage(t, *partial, 50, 40, 30, 20, 10)

	terminal, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(terminal) != 1 {
		t.Fatalf("finish events = %d", len(terminal))
	}
	done, ok := terminal[0].(StreamDoneEvent)
	if !ok || done.Message.Usage == nil {
		t.Fatalf("terminal event = %#v", terminal[0])
	}
	assertUsage(t, *done.Message.Usage, 50, 40, 30, 20, 10)
}

func TestOpenAIDecoderCachedSubsetDoesNotDoubleCount(t *testing.T) {
	d := NewOpenAIDecoder()
	if _, err := d.Decode([]byte(`{
	  "choices": [{"delta": {"content": "hi"}}],
	  "usage": {"prompt_tokens": 100, "completion_tokens": 40,
	            "prompt_tokens_details": {"cached_tokens": 30}}
	}`)); err != nil {
		t.Fatal(err)
	}
	terminal, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	done, ok := terminal[0].(StreamDoneEvent)
	if !ok || done.Message.Usage == nil {
		t.Fatalf("terminal event = %#v", terminal[0])
	}
	assertUsage(t, *done.Message.Usage, 70, 40, 30, 0, 0)
}

func TestOpenAIDecoderClampsInputAtZero(t *testing.T) {
	cases := []struct {
		name           string
		usage          string
		wantInput      int
		wantOutput     int
		wantCacheRead  int
		wantCacheWrite int
	}{
		{
			name:          "prompt fully covered by cached",
			usage:         `{"prompt_tokens": 100, "completion_tokens": 40, "prompt_tokens_details": {"cached_tokens": 100}}`,
			wantOutput:    40,
			wantCacheRead: 100,
		},
		{
			name:           "cache write exceeds prompt",
			usage:          `{"prompt_tokens": 10, "completion_tokens": 1, "prompt_tokens_details": {"cache_write_tokens": 20}}`,
			wantOutput:     1,
			wantCacheWrite: 20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewOpenAIDecoder()
			payload := []byte(`{"choices": [{"delta": {"content": "hi"}}], "usage": ` + tc.usage + `}`)
			if _, err := d.Decode(payload); err != nil {
				t.Fatal(err)
			}
			terminal, err := d.Finish()
			if err != nil {
				t.Fatal(err)
			}
			done, ok := terminal[0].(StreamDoneEvent)
			if !ok || done.Message.Usage == nil {
				t.Fatalf("terminal event = %#v", terminal[0])
			}
			assertUsage(t, *done.Message.Usage, tc.wantInput, tc.wantOutput,
				tc.wantCacheRead, tc.wantCacheWrite, 0)
			if done.Message.Usage.InputTokens != 0 {
				t.Fatalf("input = %d, want clamped to 0", done.Message.Usage.InputTokens)
			}
		})
	}
}

func TestOpenAIDecoderLeavesAbsentCountersZero(t *testing.T) {
	d := NewOpenAIDecoder()
	if _, err := d.Decode([]byte(`{"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)); err != nil {
		t.Fatal(err)
	}
	terminal, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	done, ok := terminal[0].(StreamDoneEvent)
	if !ok || done.Message.Usage == nil {
		t.Fatalf("terminal event = %#v", terminal[0])
	}
	assertUsage(t, *done.Message.Usage, 7, 2, 0, 0, 0)
}

func TestAnthropicDecoderMapsCacheTokens(t *testing.T) {
	d := NewAnthropicDecoder()
	start := []byte(`{
	  "type": "message_start",
	  "message": {
	    "id": "msg_1",
	    "model": "claude",
	    "usage": {
	      "input_tokens": 10,
	      "output_tokens": 1,
	      "cache_read_input_tokens": 30,
	      "cache_creation_input_tokens": 20
	    }
	  }
	}`)
	if _, err := d.Decode(start); err != nil {
		t.Fatal(err)
	}
	delta := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`)
	if _, err := d.Decode(delta); err != nil {
		t.Fatal(err)
	}
	events, err := d.Decode([]byte(`{"type":"message_stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("message_stop events = %d", len(events))
	}
	done, ok := events[0].(StreamDoneEvent)
	if !ok || done.Message.Usage == nil {
		t.Fatalf("terminal event = %#v", events[0])
	}
	assertUsage(t, *done.Message.Usage, 10, 12, 30, 20, 0)
}

func TestResponsesNormalizesCacheSubsets(t *testing.T) {
	driver := &responsesDriver{name: "openai"}
	// Decode real wire JSON so the SDK's extra-field bag carries the
	// cache_write_tokens counter that is not on the typed struct.
	raw := []byte(`{
	  "id": "resp_1",
	  "model": "gpt-5",
	  "usage": {
	    "input_tokens": 100,
	    "output_tokens": 40,
	    "input_tokens_details": {"cached_tokens": 30, "cache_write_tokens": 20},
	    "output_tokens_details": {"reasoning_tokens": 10},
	    "total_tokens": 140
	  }
	}`)
	var resp responses.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	msg := driver.mapResponse(&resp)
	if msg.Usage == nil {
		t.Fatal("usage missing")
	}
	assertUsage(t, *msg.Usage, 50, 40, 30, 20, 10)
}

func TestResponsesCachedSubsetDoesNotDoubleCount(t *testing.T) {
	driver := &responsesDriver{name: "openai"}
	var resp responses.Response
	if err := json.Unmarshal([]byte(`{
	  "id": "resp_2",
	  "model": "gpt-5",
	  "usage": {"input_tokens": 100, "output_tokens": 40,
	            "input_tokens_details": {"cached_tokens": 30},
	            "output_tokens_details": {}}
	}`), &resp); err != nil {
		t.Fatal(err)
	}
	msg := driver.mapResponse(&resp)
	if msg.Usage == nil {
		t.Fatal("usage missing")
	}
	assertUsage(t, *msg.Usage, 70, 40, 30, 0, 0)
}

func TestResponsesClampsInputAtZero(t *testing.T) {
	cases := []struct {
		name           string
		usage          string
		wantOutput     int
		wantCacheRead  int
		wantCacheWrite int
	}{
		{
			name:          "input fully covered by cached",
			usage:         `{"input_tokens": 100, "output_tokens": 40, "input_tokens_details": {"cached_tokens": 100}, "output_tokens_details": {}}`,
			wantOutput:    40,
			wantCacheRead: 100,
		},
		{
			name:           "cache write exceeds input",
			usage:          `{"input_tokens": 10, "output_tokens": 1, "input_tokens_details": {"cache_write_tokens": 20}, "output_tokens_details": {}}`,
			wantOutput:     1,
			wantCacheWrite: 20,
		},
	}
	driver := &responsesDriver{name: "openai"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp responses.Response
			if err := json.Unmarshal([]byte(`{"id": "resp_clamp", "model": "gpt-5", "usage": `+tc.usage+`}`), &resp); err != nil {
				t.Fatal(err)
			}
			msg := driver.mapResponse(&resp)
			if msg.Usage == nil {
				t.Fatal("usage missing")
			}
			assertUsage(t, *msg.Usage, 0, tc.wantOutput, tc.wantCacheRead, tc.wantCacheWrite, 0)
			if msg.Usage.InputTokens != 0 {
				t.Fatalf("input = %d, want clamped to 0", msg.Usage.InputTokens)
			}
		})
	}
}

func TestResponsesOmitsEmptyUsage(t *testing.T) {
	driver := &responsesDriver{name: "openai"}
	msg := driver.mapResponse(&responses.Response{ID: "resp_3", Model: "gpt-5"})
	if msg.Usage != nil {
		t.Fatalf("usage = %+v, want nil", msg.Usage)
	}
}

func TestUsageJSONPreservesNewCounters(t *testing.T) {
	usage := agentcore.Usage{
		InputTokens: 1, OutputTokens: 2,
		CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5,
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"cacheReadTokens", "cacheWriteTokens", "reasoningTokens"} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("%s missing from %s", field, raw)
		}
	}
}
