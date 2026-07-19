package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/platform/httpclient"
)

type Alert = AlertPayload

type AlertPayload struct {
	Title     string         `json:"title"`
	State     string         `json:"state"`
	Message   string         `json:"message"`
	RuleURL   string         `json:"ruleUrl"`
	Tags      map[string]any `json:"tags"`
	TimeRange *TimeRange     `json:"timeRange"`
	Raw       map[string]any `json:"-"`
}

type TimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type ManualAlertRequest struct {
	Title     string   `json:"title"`
	Message   string   `json:"message"`
	TimeFrom  string   `json:"timeFrom"`
	TimeTo    string   `json:"timeTo"`
	Services  []string `json:"services"`
	ErrorLogs []LogHit `json:"errorLogs"`
}

func ParseAlertPayload(raw map[string]any) AlertPayload {
	alert := parseAlertMap(raw)
	alert.Raw = raw
	return alert
}

func ParseManualAlert(req ManualAlertRequest) AlertPayload {
	tags := map[string]any{}
	if len(req.Services) > 0 {
		tags["service"] = strings.Join(req.Services, ",")
	}
	from := parseTime(req.TimeFrom)
	to := parseTime(req.TimeTo)
	if from.IsZero() {
		from = time.Now().Add(-15 * time.Minute)
	}
	if to.IsZero() {
		to = time.Now()
	}
	raw := map[string]any{"title": req.Title, "message": req.Message, "tags": tags, "timeRange": map[string]any{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)}}
	return AlertPayload{Title: req.Title, Message: req.Message, Tags: tags, TimeRange: &TimeRange{From: from, To: to}, Raw: raw}
}

func parseAlertMap(raw map[string]any) AlertPayload {
	alert := AlertPayload{Raw: raw}
	if raw == nil {
		return alert
	}
	alert.Title = strAny(raw["title"])
	alert.State = strAny(raw["state"])
	alert.Message = strAny(raw["message"])
	alert.RuleURL = strAny(raw["ruleUrl"])
	if tags, ok := raw["tags"].(map[string]any); ok {
		alert.Tags = tags
	}
	if tr, ok := raw["timeRange"].(map[string]any); ok {
		from := parseTime(strAny(tr["from"]))
		to := parseTime(strAny(tr["to"]))
		if !from.IsZero() || !to.IsZero() {
			alert.TimeRange = &TimeRange{From: from, To: to}
		}
	}
	return alert
}

func (manager *Manager) notify(ctx context.Context, inc *Incident) {
	if manager.cfg.NotifyFeishuWebhook != "" {
		postJSON(ctx, manager.cfg.NotifyFeishuWebhook, feishuCardPayload(manager.cfg.WebBaseURL, inc))
		return
	}
	url := platform.FirstNonEmpty(manager.cfg.NotifyWecomWebhook, manager.cfg.NotifyHTTPWebhook)
	if url == "" {
		return
	}
	postJSON(ctx, url, map[string]any{
		"msg_type": "text",
		"content":  map[string]any{"text": fmt.Sprintf("CodeLoom Incident %s\n%s\n根因: %s", inc.ID, inc.AlertTitle, inc.RootCause)},
	})
}

func postJSON(ctx context.Context, endpoint string, payload any) {
	body, _ := json.Marshal(payload)
	_, _ = httpclient.Request(ctx, httpclient.New(15*time.Second, nil)).
		SetBody(body).
		Post(endpoint)
}

func feishuCardPayload(baseURL string, inc *Incident) map[string]any {
	detailURL := strings.TrimRight(baseURL, "/") + "/incidents"
	return map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"template": "red",
				"title":    map[string]any{"tag": "plain_text", "content": "CodeLoom 线上告警 | " + inc.AlertTitle},
			},
			"elements": []any{
				map[string]any{
					"tag": "div",
					"fields": []any{
						map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": "**问题单**\n" + inc.ID}},
						map[string]any{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": "**状态**\n" + string(inc.Status)}},
						map[string]any{"is_short": false, "text": map[string]any{"tag": "lark_md", "content": "**涉及服务**\n" + emptyDash(strings.Join(inc.AffectedSvcs, ", "))}},
						map[string]any{"is_short": false, "text": map[string]any{"tag": "lark_md", "content": "**根因**\n" + emptyDash(inc.RootCause)}},
					},
				},
				map[string]any{
					"tag": "action",
					"actions": []any{
						map[string]any{
							"tag":  "button",
							"text": map[string]any{"tag": "plain_text", "content": "查看详情"},
							"type": "primary",
							"url":  detailURL,
						},
					},
				},
			},
		},
	}
}

func servicesFromAlert(alert AlertPayload) []string {
	if alert.Tags == nil {
		return nil
	}
	var out []string
	for _, key := range []string{"service", "svc", "app", "application"} {
		v := strAny(alert.Tags[key])
		if v == "" {
			continue
		}
		for _, p := range strings.Split(v, ",") {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
	}
	return unique(out)
}

func servicesFromTraces(traces []*TraceResult) []string {
	var out []string
	for _, tr := range traces {
		if tr != nil {
			out = append(out, tr.Services...)
		}
	}
	return unique(out)
}

func alertWindow(alert AlertPayload) (time.Time, time.Time) {
	if alert.TimeRange != nil {
		from, to := alert.TimeRange.From, alert.TimeRange.To
		if from.IsZero() {
			from = time.Now().Add(-15 * time.Minute)
		}
		if to.IsZero() {
			to = time.Now()
		}
		return from, to
	}
	return time.Now().Add(-15 * time.Minute), time.Now()
}

func dedupKey(alert AlertPayload, services []string) string {
	from, to := alertWindow(alert)
	from = from.Truncate(time.Minute)
	to = to.Truncate(time.Minute)
	svcs := strings.Join(unique(services), ",")
	return strings.ToLower(strings.TrimSpace(alert.Title)) + "|" + svcs + "|" + from.Format(time.RFC3339) + "|" + to.Format(time.RFC3339)
}

func stripRegion(svc string) string {
	return regionSuffix.ReplaceAllString(svc, "")
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return strings.TrimSpace(s)
}
