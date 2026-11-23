package workspace

// DeviceUsage 按项目和执行设备汇总同一批设备运行，并单列中心投递的用量。
type DeviceUsage struct {
	Projects []UsageGroup `json:"projects"`
	Devices  []UsageGroup `json:"devices"`
	Center   *UsageGroup  `json:"center,omitempty"`
}

// UsageGroup 保存已结束的设备运行或中心投递的总数以及其中有用量的轮数。
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
