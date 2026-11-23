package workspace

// DeviceUsage 汇总 Hub 已收到的设备运行用量，按项目和执行设备查询同一批运行。
type DeviceUsage struct {
	Projects []UsageGroup `json:"projects"`
	Devices  []UsageGroup `json:"devices"`
}

// UsageGroup 保存已结束且曾领取的运行总数以及其中有用量的运行数。
type UsageGroup struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	FinishedRuns int64         `json:"finished_runs"`
	MeasuredRuns int64         `json:"measured_runs"`
	Sources      []UsageSource `json:"sources"`
}

// UsageSource 保存单一 CLI 统计口径下的 Token 合计和覆盖数。
// 合计使用十进制字符串，避免跨运行累加后超出浏览器的整数精度。
// 可选合计只在对应字段至少有一个样本时返回。
type UsageSource struct {
	Source                string  `json:"source"`
	MeasuredRuns          int64   `json:"measured_runs"`
	InputTokens           string  `json:"input_tokens"`
	OutputTokens          string  `json:"output_tokens"`
	CachedInputTokens     *string `json:"cached_input_tokens,omitempty"`
	CachedInputRuns       int64   `json:"cached_input_runs"`
	CacheWriteInputTokens *string `json:"cache_write_input_tokens,omitempty"`
	CacheWriteInputRuns   int64   `json:"cache_write_input_runs"`
	ReasoningOutputTokens *string `json:"reasoning_output_tokens,omitempty"`
	ReasoningOutputRuns   int64   `json:"reasoning_output_runs"`
	TotalTokens           *string `json:"total_tokens,omitempty"`
	TotalRuns             int64   `json:"total_runs"`
}
