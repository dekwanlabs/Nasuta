package incident

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

type Analysis = llmAnalysis

func (manager *Manager) Analyze(ctx context.Context, id string) error {
	log.Infof("[incident] ===== analyze start: %s =====", id)
	inc, err := manager.Get(ctx, id)
	if err != nil {
		log.Errorf("[incident] analyze %s: get failed: %v", id, err)
		return err
	}
	alert := parseAlertMap(inc.AlertPayload)
	from, to := alertWindow(alert)
	log.Infof("[incident] analyze %s: window=%s ~ %s, affected_svcs=%v", id, from.Format(time.RFC3339), to.Format(time.RFC3339), inc.AffectedSvcs)

	var logs *LogSearchResult
	if len(inc.ErrorLogs) > 0 {
		log.Warnf("[incident] analyze %s: using existing %d error logs, skip Kibana", id, len(inc.ErrorLogs))
		logs = &LogSearchResult{Hits: inc.ErrorLogs, Total: len(inc.ErrorLogs)}
	} else if manager.evidence != nil && manager.evidence.LogsEnabled() {
		log.Infof("[incident] analyze %s: searching configured observe source", id)
		logs, err = manager.evidence.SearchLogs(ctx, LogSearchRequest{
			From:       from,
			To:         to,
			Limit:      200,
			ErrorsOnly: true,
		})
		if err != nil {
			log.Errorf("[incident] analyze %s: kibana search error: %v", id, err)
		} else if logs != nil {
			inc.ErrorLogs = logs.Hits
			log.Infof("[incident] analyze %s: kibana found %d hits (total=%d)", id, len(logs.Hits), logs.Total)
		}
	}

	if manager.evidence != nil && manager.evidence.TracesEnabled() && len(inc.ErrorLogs) > 0 {
		seen := map[string]bool{}
		for _, hit := range inc.ErrorLogs {
			if hit.TraceID == "" || seen[hit.TraceID] || len(inc.Traces) >= 10 {
				continue
			}
			seen[hit.TraceID] = true
			tr, trErr := manager.evidence.GetTrace(ctx, hit.TraceID, parseTime(hit.Timestamp), 5)
			if trErr == nil {
				inc.Traces = append(inc.Traces, tr)
			} else {
				inc.Traces = append(inc.Traces, &TraceResult{TraceID: hit.TraceID, ErrorMsg: trErr.Error()})
			}
		}
	}

	inc.AffectedSvcs = mergeStrings(inc.AffectedSvcs, servicesFromTraces(inc.Traces))
	inc.RootCause = inferRootCause(inc, logs, err)
	inc.Solution = inferSolution(inc)
	log.Infof("[incident] analyze %s: root_cause=%s", id, platform.TruncateForLog(inc.RootCause, 120))
	var llmDoc string
	if llmResult, llmErr := manager.analyzeWithLLM(ctx, inc, from, to, logs, err); llmErr == nil && llmResult != nil {
		if strings.TrimSpace(llmResult.RootCause) != "" {
			inc.RootCause = strings.TrimSpace(llmResult.RootCause)
		}
		if strings.TrimSpace(llmResult.Solution) != "" {
			inc.Solution = strings.TrimSpace(llmResult.Solution)
		}
		if strings.TrimSpace(llmResult.AnalysisDoc) != "" {
			llmDoc = strings.TrimSpace(llmResult.AnalysisDoc)
		}
	} else if llmErr != nil {
		log.Errorf("[incident] analyze %s: LLM failed: %v", id, llmErr)
	}
	inc.AnalysisDoc = buildAnalysisDoc(inc, from, to, logs, err)
	if llmDoc != "" {
		inc.AnalysisDoc += "\n\n## LLM 深度分析\n\n" + llmDoc + "\n"
	}
	inc.Status = StatusOpen
	if err := manager.save(ctx, inc); err != nil {
		return err
	}
	manager.notify(ctx, inc)
	return nil
}

type llmAnalysis struct {
	RootCause   string `json:"root_cause"`
	Solution    string `json:"solution"`
	AnalysisDoc string `json:"analysis_doc"`
}

func (manager *Manager) analyzeWithLLM(ctx context.Context, inc *Incident, from, to time.Time, logs *LogSearchResult, searchErr error) (*llmAnalysis, error) {
	prompt := manager.buildLLMPrompt(ctx, inc, from, to, logs, searchErr)
	var out llmAnalysis
	if err := manager.llm.ChatJSON(ctx, prompts.Text(prompts.IncidentSystem), prompt, &out, llm.CallOptions{}); err != nil {
		return nil, fmt.Errorf("decode LLM JSON: %w", err)
	}
	return &out, nil
}

func (manager *Manager) buildLLMPrompt(ctx context.Context, inc *Incident, from, to time.Time, logs *LogSearchResult, searchErr error) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Incident ID: %s\nTitle: %s\nWindow: %s ~ %s\nServices: %s\n\n",
		inc.ID, inc.AlertTitle, from.Format(time.RFC3339), to.Format(time.RFC3339), strings.Join(inc.AffectedSvcs, ", "))
	if searchErr != nil {
		sb.WriteString("Kibana search error: " + searchErr.Error() + "\n\n")
	}
	if logs != nil {
		sb.WriteString("Log summaries:\n")
		for _, s := range logs.Summaries[:min(len(logs.Summaries), 12)] {
			fmt.Fprintf(&sb, "- api=%s method=%s count=%d p95=%.0fms max=%.0fms status=%s\n", s.API, s.Method, s.Count, s.P95Ms, s.MaxMs, s.Status)
		}
		sb.WriteString("\nRepresentative logs:\n")
		for _, h := range logs.Hits[:min(len(logs.Hits), 12)] {
			fmt.Fprintf(&sb, "- time=%s api=%s cost=%.0fms status=%s trace=%s msg=%s\n", h.Timestamp, h.API, h.CostMs, h.Status, h.TraceID, truncate(sanitizeOneLine(h.Message), 260))
		}
	}
	if manager.knowledge != nil {
		sb.WriteString("\nCode hints:\n")
		query := strings.TrimSpace(inc.AlertTitle + " " + inc.RootCause + " " + strings.Join(inc.AffectedSvcs, " "))
		result, err := manager.knowledge.SearchCode(ctx, knowledge.CodeSearchQuery{Query: query, Limit: 8})
		if err == nil {
			for _, match := range result.Matches[:min(len(result.Matches), 8)] {
				fmt.Fprintf(&sb, "- %s L%d-L%d: %s\n", match.Path, match.StartLine, match.EndLine, truncate(sanitizeOneLine(match.Text), 360))
			}
		}
	}
	return sb.String()
}

func buildAnalysisDoc(inc *Incident, from, to time.Time, logs *LogSearchResult, searchErr error) string {
	var sb strings.Builder
	sb.WriteString("# 问题分析记录\n\n")
	fmt.Fprintf(&sb, "**问题单 ID**：%s\n", inc.ID)
	fmt.Fprintf(&sb, "**创建时间**：%s\n", inc.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "**告警来源**：%s\n", inc.Source)
	fmt.Fprintf(&sb, "**状态**：%s\n\n", inc.Status)
	sb.WriteString("## 告警信息\n\n")
	fmt.Fprintf(&sb, "- **标题**：%s\n", inc.AlertTitle)
	fmt.Fprintf(&sb, "- **时间窗口**：%s ~ %s\n", from.Format(time.RFC3339), to.Format(time.RFC3339))
	sb.WriteString("\n## 根因分析\n\n")
	sb.WriteString(inc.RootCause + "\n\n")
	sb.WriteString("## 建议修复方案\n\n")
	sb.WriteString(inc.Solution + "\n")
	return sb.String()
}

func inferRootCause(inc *Incident, logs *LogSearchResult, searchErr error) string {
	if searchErr != nil {
		return "Kibana 查询失败，需人工确认告警上下文与日志权限。"
	}
	for _, tr := range inc.Traces {
		if tr == nil {
			continue
		}
		if tr.ErrorCount > 0 {
			return fmt.Sprintf("%s 调用链存在 %d 个错误 Span，最慢节点为 %s/%s（%dms）。",
				tr.TraceID, tr.ErrorCount, tr.SlowestService, tr.SlowestComponent, tr.SlowestDurationMs)
		}
	}
	if logs != nil && len(logs.Summaries) > 0 {
		s := logs.Summaries[0]
		return fmt.Sprintf("告警窗口内 `%s` 出现慢请求，P95 %.0fms，最大 %.0fms。", s.API, s.P95Ms, s.MaxMs)
	}
	return "暂未从日志或调用链中定位明确根因，需要人工继续排查。"
}

func inferSolution(inc *Incident) string {
	if len(inc.AffectedSvcs) > 0 {
		return fmt.Sprintf("优先检查服务 `%s` 在告警窗口内的错误日志、慢 SQL、下游超时和连接池状态；按分析文档中的 TraceID 复现调用链。", strings.Join(inc.AffectedSvcs, ", "))
	}
	return "补充告警服务标签或 TraceID 后重新分析，并检查告警窗口内的 5xx、慢请求和下游依赖。"
}
