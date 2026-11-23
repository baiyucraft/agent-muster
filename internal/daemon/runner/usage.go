package runner

import (
	"encoding/json"
	"math"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

type dshUsage struct {
	steps   map[[2]int]struct{}
	total   *runreport.Usage
	invalid bool
}

func (usage *dshUsage) add(turn, step int, raw json.RawMessage) {
	if usage.invalid {
		return
	}
	var sample struct {
		InputTokens      *int64 `json:"inputTokens"`
		OutputTokens     *int64 `json:"outputTokens"`
		CacheReadTokens  *int64 `json:"cacheReadTokens"`
		CacheWriteTokens *int64 `json:"cacheWriteTokens"`
		ReasoningTokens  *int64 `json:"reasoningTokens"`
		TotalTokens      *int64 `json:"totalTokens"`
	}
	key := [2]int{turn, step}
	if _, seen := usage.steps[key]; seen || turn < 0 || step < 0 ||
		json.Unmarshal(raw, &sample) != nil ||
		!validTokenCounts(sample.InputTokens, sample.OutputTokens, sample.CacheReadTokens, sample.CacheWriteTokens, sample.ReasoningTokens, sample.TotalTokens) {
		usage.invalid = true
		return
	}
	if usage.steps == nil {
		usage.steps = make(map[[2]int]struct{})
	}
	usage.steps[key] = struct{}{}
	report := runreport.Usage{
		Source:                runreport.DSHHeadless,
		InputTokens:           *sample.InputTokens,
		OutputTokens:          *sample.OutputTokens,
		CachedInputTokens:     sample.CacheReadTokens,
		CacheWriteInputTokens: sample.CacheWriteTokens,
		ReasoningOutputTokens: sample.ReasoningTokens,
		TotalTokens:           sample.TotalTokens,
	}
	if usage.total == nil {
		usage.total = &report
		return
	}
	// step_end 已包含该步骤的全部模型尝试。可选桶在每个步骤都提供时才有整轮值。
	if usage.total.InputTokens > math.MaxInt64-report.InputTokens || usage.total.OutputTokens > math.MaxInt64-report.OutputTokens {
		usage.invalid = true
		return
	}
	usage.total.InputTokens += report.InputTokens
	usage.total.OutputTokens += report.OutputTokens
	for _, count := range []struct {
		total  **int64
		sample *int64
	}{
		{&usage.total.CachedInputTokens, report.CachedInputTokens},
		{&usage.total.CacheWriteInputTokens, report.CacheWriteInputTokens},
		{&usage.total.ReasoningOutputTokens, report.ReasoningOutputTokens},
		{&usage.total.TotalTokens, report.TotalTokens},
	} {
		if *count.total == nil || count.sample == nil {
			*count.total = nil
			continue
		}
		if **count.total > math.MaxInt64-*count.sample {
			usage.invalid = true
			return
		}
		**count.total += *count.sample
	}
}

func (usage *dshUsage) result() *runreport.Usage {
	if usage.invalid {
		return nil
	}
	return usage.total
}

func codexUsage(raw json.RawMessage, resumed bool) *runreport.Usage {
	// Codex 返回线程累计用量。续接时缺少历史基线，无法计算本轮用量。
	if resumed {
		return nil
	}
	var sample struct {
		InputTokens           *int64 `json:"input_tokens"`
		OutputTokens          *int64 `json:"output_tokens"`
		CachedInputTokens     *int64 `json:"cached_input_tokens"`
		CacheWriteInputTokens *int64 `json:"cache_write_input_tokens"`
		ReasoningOutputTokens *int64 `json:"reasoning_output_tokens"`
	}
	if json.Unmarshal(raw, &sample) != nil ||
		!validTokenCounts(sample.InputTokens, sample.OutputTokens, sample.CachedInputTokens, sample.CacheWriteInputTokens, sample.ReasoningOutputTokens) {
		return nil
	}
	// CLI 在缺少计量样本时写入全零对象。
	if *sample.InputTokens == 0 && *sample.OutputTokens == 0 &&
		!positiveTokenCount(sample.CachedInputTokens) && !positiveTokenCount(sample.CacheWriteInputTokens) && !positiveTokenCount(sample.ReasoningOutputTokens) {
		return nil
	}
	return &runreport.Usage{
		Source:                runreport.CodexExec,
		InputTokens:           *sample.InputTokens,
		OutputTokens:          *sample.OutputTokens,
		CachedInputTokens:     sample.CachedInputTokens,
		CacheWriteInputTokens: sample.CacheWriteInputTokens,
		ReasoningOutputTokens: sample.ReasoningOutputTokens,
	}
}

func validTokenCounts(input, output *int64, optional ...*int64) bool {
	if input == nil || output == nil || *input < 0 || *output < 0 {
		return false
	}
	for _, count := range optional {
		if count != nil && *count < 0 {
			return false
		}
	}
	return true
}

func positiveTokenCount(count *int64) bool {
	return count != nil && *count > 0
}
