package modelcfg

import (
	"reflect"
	"testing"
)

func TestMergePatch(t *testing.T) {
	tests := []struct {
		name   string
		target map[string]any
		patch  map[string]any
		want   map[string]any
	}{
		{
			name:   "absent keys inherit",
			target: map[string]any{"keep": "old"},
			patch:  map[string]any{"added": "new"},
			want:   map[string]any{"keep": "old", "added": "new"},
		},
		{
			name:   "null clears a key",
			target: map[string]any{"a": "x", "b": "y"},
			patch:  map[string]any{"a": nil},
			want:   map[string]any{"b": "y"},
		},
		{
			name:   "value replaces",
			target: map[string]any{"a": "old"},
			patch:  map[string]any{"a": "new"},
			want:   map[string]any{"a": "new"},
		},
		{
			name:   "nested objects merge recursively",
			target: map[string]any{"p": map[string]any{"x": 1, "y": 2}},
			patch:  map[string]any{"p": map[string]any{"y": nil, "z": 3}},
			want:   map[string]any{"p": map[string]any{"x": 1, "z": 3}},
		},
		{
			name:   "arrays replace wholesale",
			target: map[string]any{"a": []any{1, 2, 3}},
			patch:  map[string]any{"a": []any{9}},
			want:   map[string]any{"a": []any{9}},
		},
		{
			name: "array elements are not merged",
			target: map[string]any{"a": []any{
				map[string]any{"x": 1, "y": 1},
			}},
			patch: map[string]any{"a": []any{
				map[string]any{"x": 2},
			}},
			want: map[string]any{"a": []any{
				map[string]any{"x": 2},
			}},
		},
		{
			name:   "object replaces scalar",
			target: map[string]any{"a": 1},
			patch:  map[string]any{"a": map[string]any{"b": 2}},
			want:   map[string]any{"a": map[string]any{"b": 2}},
		},
		{
			name:   "scalar replaces object",
			target: map[string]any{"a": map[string]any{"b": 2}},
			patch:  map[string]any{"a": 3},
			want:   map[string]any{"a": 3},
		},
		{
			name:   "empty patch keeps target",
			target: map[string]any{"a": 1},
			patch:  map[string]any{},
			want:   map[string]any{"a": 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targetBefore := cloneJSONMap(test.target)
			got, _ := mergePatch(test.target, test.patch).(map[string]any)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mergePatch:\n got: %#v\nwant: %#v", got, test.want)
			}
			if !reflect.DeepEqual(test.target, targetBefore) {
				t.Fatalf("mergePatch mutated target:\n got: %#v\nwant: %#v", test.target, targetBefore)
			}
		})
	}
}

func TestMergePatchNullReplacesDocument(t *testing.T) {
	got := mergePatch(map[string]any{"a": 1}, nil)
	if got != nil {
		t.Fatalf("null patch must replace the document, got %#v", got)
	}
}

func TestMergeJSONObjectsDoesNotMutateInputs(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"x": 1}}
	patch := map[string]any{"a": map[string]any{"y": 2}}
	got := mergeJSONObjects(dst, patch)
	want := map[string]any{"a": map[string]any{"x": 1, "y": 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeJSONObjects:\n got: %#v\nwant: %#v", got, want)
	}
	if _, ok := dst["a"].(map[string]any)["y"]; ok {
		t.Fatalf("mergeJSONObjects mutated dst: %#v", dst)
	}
	if _, ok := patch["a"].(map[string]any)["x"]; ok {
		t.Fatalf("mergeJSONObjects mutated patch: %#v", patch)
	}
}

func TestMatchModelGlob(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*", "deepseek-v4.1-flash", true},
		{"*deepseek*", "deepseek-v4.1-flash", true},
		{"*DeepSeek*", "deepseek-v4.1-flash", true},
		{"deepseek-*", "DeepSeek-V4.1-Flash", true},
		{"*-flash", "DEEPSEEK-V4.1-FLASH", true},
		{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", true},
		{"*claude*", "deepseek-v4.1-flash", false},
		{"deepseek-*", "deepseek", false},
		{"*flash", "deepseek-v4.1-flash-extra", false},
		{"", "anything", false},
	}
	for _, test := range tests {
		if got := matchModelGlob(test.pattern, test.name); got != test.want {
			t.Errorf("matchModelGlob(%q, %q) = %v, want %v", test.pattern, test.name, got, test.want)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://api.example.com/v1/", "https://api.example.com/v1"},
		{"HTTPS://API.Example.COM/v1", "https://api.example.com/v1"},
		{"  https://api.example.com/v1///  ", "https://api.example.com/v1"},
		{"http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1"},
		{"", ""},
	}
	for _, test := range tests {
		if got := normalizeBaseURL(test.in); got != test.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}
