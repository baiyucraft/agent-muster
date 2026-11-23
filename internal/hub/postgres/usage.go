package postgres

import (
	"context"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

// DeviceUsage 汇总已领取并已结束的运行。只读运行行，每个运行只计一次。
// 使用同一个 SQL 视图汇总项目和执行设备，使并发上报时两组覆盖数一致。
func (store *InventoryStore) DeviceUsage(ctx context.Context) (workspace.DeviceUsage, error) {
	result := workspace.DeviceUsage{Projects: []workspace.UsageGroup{}, Devices: []workspace.UsageGroup{}}
	rows, err := store.pool.Query(ctx, `
		WITH recorded AS (
			SELECT t.project_id, p.name AS project_name, r.device_id, d.name AS device_name,
				CASE WHEN r.diagnostics->'usage'->>'source' IN ($1,$2)
					THEN r.diagnostics->'usage' END AS usage
			FROM muster_runs r
			JOIN muster_tasks t ON t.id=r.task_id
			JOIN muster_projects p ON p.id=t.project_id
			JOIN muster_devices d ON d.device_id=r.device_id
			WHERE r.claimed_at IS NOT NULL AND r.status NOT IN ('queued','running')
		)
		SELECT grouping.kind, grouping.id, grouping.name, COALESCE(usage->>'source',''), COUNT(*),
			COALESCE(SUM((usage->>'input_tokens')::numeric),0)::text,
			COALESCE(SUM((usage->>'output_tokens')::numeric),0)::text,
			SUM((usage->>'cached_input_tokens')::numeric)::text, COUNT(usage->>'cached_input_tokens'),
			SUM((usage->>'cache_write_input_tokens')::numeric)::text, COUNT(usage->>'cache_write_input_tokens'),
			SUM((usage->>'reasoning_output_tokens')::numeric)::text, COUNT(usage->>'reasoning_output_tokens'),
			SUM((usage->>'total_tokens')::numeric)::text, COUNT(usage->>'total_tokens')
		FROM recorded
		CROSS JOIN LATERAL (VALUES ('project',project_id,project_name), ('device',device_id,device_name)) grouping(kind,id,name)
		GROUP BY grouping.kind, grouping.id, grouping.name, usage->>'source'
		ORDER BY grouping.kind, grouping.name, grouping.id, usage->>'source'`, runreport.DSHHeadless, runreport.CodexExec)
	if err != nil {
		return workspace.DeviceUsage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id, name string
		var source workspace.UsageSource
		if err := rows.Scan(&kind, &id, &name, &source.Source, &source.MeasuredRuns, &source.InputTokens, &source.OutputTokens,
			&source.CachedInputTokens, &source.CachedInputRuns, &source.CacheWriteInputTokens, &source.CacheWriteInputRuns,
			&source.ReasoningOutputTokens, &source.ReasoningOutputRuns, &source.TotalTokens, &source.TotalRuns); err != nil {
			return workspace.DeviceUsage{}, err
		}
		groups := &result.Projects
		if kind == "device" {
			groups = &result.Devices
		}
		if len(*groups) == 0 || (*groups)[len(*groups)-1].ID != id {
			*groups = append(*groups, workspace.UsageGroup{ID: id, Name: name, Sources: []workspace.UsageSource{}})
		}
		group := &(*groups)[len(*groups)-1]
		group.FinishedRuns += source.MeasuredRuns
		if source.Source != "" {
			group.MeasuredRuns += source.MeasuredRuns
			group.Sources = append(group.Sources, source)
		}
	}
	return result, rows.Err()
}
