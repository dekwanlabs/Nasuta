package incident

import (
	"context"
	"time"
)

// EvidenceProvider supplies optional runtime evidence without coupling Incident to a provider.
type EvidenceProvider interface {
	LogsEnabled() bool
	TracesEnabled() bool
	SearchLogs(context.Context, LogSearchRequest) (*LogSearchResult, error)
	GetTrace(context.Context, string, time.Time, int) (*TraceResult, error)
}

type LogSearchRequest struct {
	Source       string    `json:"source,omitempty"`
	Service      string    `json:"service,omitempty"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Limit        int       `json:"sample_size"`
	FullText     string    `json:"full_text,omitempty"`
	ErrorsOnly   bool      `json:"errors_only,omitempty"`
	ResponseOnly bool      `json:"response_only,omitempty"`
}

type LogHit struct {
	Timestamp string  `json:"timestamp"`
	TraceID   string  `json:"trace_id"`
	Method    string  `json:"method"`
	Status    string  `json:"status"`
	Code      string  `json:"code"`
	CostMs    float64 `json:"cost_ms"`
	API       string  `json:"api"`
	URL       string  `json:"url"`
	Request   string  `json:"request"`
	Response  string  `json:"response"`
	Message   string  `json:"message"`
	Identity  string  `json:"identity,omitempty"`
}

type APISummary struct {
	API    string  `json:"api"`
	Count  int     `json:"count"`
	AvgMs  float64 `json:"avg_ms"`
	P95Ms  float64 `json:"p95_ms"`
	MaxMs  float64 `json:"max_ms"`
	Method string  `json:"method"`
	Status string  `json:"status"`
}

type LogSearchResult struct {
	Total     int          `json:"total"`
	Hits      []LogHit     `json:"hits"`
	Summaries []APISummary `json:"summaries"`
}

type Span struct {
	TraceID             string    `json:"traceId"`
	SegmentID           string    `json:"segmentId"`
	SpanID              int       `json:"spanId"`
	ParentSpanID        int       `json:"parentSpanId"`
	ServiceCode         string    `json:"serviceCode"`
	ServiceInstanceName string    `json:"serviceInstanceName"`
	StartTime           int64     `json:"startTime"`
	EndTime             int64     `json:"endTime"`
	EndpointName        string    `json:"endpointName"`
	Type                string    `json:"type"`
	Peer                string    `json:"peer"`
	Component           string    `json:"component"`
	IsError             bool      `json:"isError"`
	Layer               string    `json:"layer"`
	Refs                []SpanRef `json:"refs,omitempty"`
	Tags                []KVPair  `json:"tags"`
	Logs                []SpanLog `json:"logs"`
	DurationMs          int64     `json:"duration_ms"`
}

// TagsMap returns a detached lookup view of the span tags.
func (span Span) TagsMap() map[string]string {
	out := make(map[string]string, len(span.Tags))
	for _, tag := range span.Tags {
		out[tag.Key] = tag.Value
	}
	return out
}

type SpanRef struct {
	TraceID         string `json:"traceId"`
	ParentSegmentID string `json:"parentSegmentId"`
	ParentSpanID    int    `json:"parentSpanId"`
	Type            string `json:"type"`
}

type KVPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type SpanLog struct {
	Time int64    `json:"time"`
	Data []KVPair `json:"data"`
}

type TraceResult struct {
	TraceID           string   `json:"trace_id"`
	Found             bool     `json:"found"`
	ErrorMsg          string   `json:"error,omitempty"`
	Spans             []Span   `json:"spans"`
	TotalDurationMs   int64    `json:"total_duration_ms"`
	ErrorCount        int      `json:"error_count"`
	Services          []string `json:"services"`
	RootService       string   `json:"root_service"`
	RootEndpoint      string   `json:"root_endpoint"`
	SlowestService    string   `json:"slowest_service"`
	SlowestComponent  string   `json:"slowest_component"`
	SlowestDurationMs int64    `json:"slowest_duration_ms"`
}
