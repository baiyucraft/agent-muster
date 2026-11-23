package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestDSHUsageAggregatesSteps(t *testing.T) {
	var usage dshUsage
	usage.add(2, 1, json.RawMessage(`{"inputTokens":10,"outputTokens":2,"cacheReadTokens":3,"cacheWriteTokens":0,"reasoningTokens":1,"totalTokens":15}`))
	usage.add(2, 2, json.RawMessage(`{"inputTokens":11,"outputTokens":4,"cacheReadTokens":5,"cacheWriteTokens":0,"totalTokens":20}`))
	cached, written, total := int64(8), int64(0), int64(35)
	want := &runreport.Usage{
		Source:                runreport.DSHHeadless,
		InputTokens:           21,
		OutputTokens:          6,
		CachedInputTokens:     &cached,
		CacheWriteInputTokens: &written,
		TotalTokens:           &total,
	}
	if got := usage.result(); !reflect.DeepEqual(got, want) {
		t.Fatalf("用量 = %+v，期望 %+v", got, want)
	}
}

func TestDSHUsageUnavailableWhenStepIsUncertain(t *testing.T) {
	const validStep = `{"inputTokens":10,"outputTokens":2,"cacheReadTokens":3}`
	for _, tt := range []struct {
		name string
		step int
		raw  string
	}{
		{"missing sample", 2, ""},
		{"missing input", 2, `{"outputTokens":2}`},
		{"invalid value", 2, `{"inputTokens":1,"outputTokens":2,"cacheReadTokens":-1}`},
		{"invalid number", 2, `{"inputTokens":1.5,"outputTokens":2}`},
		{"duplicate step", 1, validStep},
		{"input overflow", 2, `{"inputTokens":9223372036854775807,"outputTokens":2}`},
		{"optional overflow", 2, `{"inputTokens":1,"outputTokens":2,"cacheReadTokens":9223372036854775807}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var usage dshUsage
			usage.add(1, 1, json.RawMessage(validStep))
			usage.add(1, tt.step, json.RawMessage(tt.raw))
			usage.add(1, 3, json.RawMessage(validStep))
			if got := usage.result(); got != nil {
				t.Fatalf("不完整的用量 = %+v，期望 nil", got)
			}
		})
	}
}

func TestCodexUsageReportsSourceCounters(t *testing.T) {
	const raw = `{"input_tokens":20,"output_tokens":5,"cached_input_tokens":8,"cache_write_input_tokens":0,"reasoning_output_tokens":3}`
	cached, written, reasoning := int64(8), int64(0), int64(3)
	want := &runreport.Usage{
		Source:                runreport.CodexExec,
		InputTokens:           20,
		OutputTokens:          5,
		CachedInputTokens:     &cached,
		CacheWriteInputTokens: &written,
		ReasoningOutputTokens: &reasoning,
	}
	if got := codexUsage(json.RawMessage(raw), false); !reflect.DeepEqual(got, want) {
		t.Fatalf("用量 = %+v，期望 %+v", got, want)
	}
	if got := codexUsage(json.RawMessage(raw), true); got != nil {
		t.Fatalf("续接的本轮用量 = %+v，期望 nil", got)
	}
}

func TestCodexUsageUnavailableWithoutMeasuredCounts(t *testing.T) {
	for _, raw := range []string{
		"",
		"null",
		`{"output_tokens":1}`,
		`{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":-1}`,
		`{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0,"reasoning_output_tokens":0}`,
	} {
		if got := codexUsage(json.RawMessage(raw), false); got != nil {
			t.Errorf("输入 %q 的用量 = %+v，期望 nil", raw, got)
		}
	}
}
