package types

import "testing"

func TestClassifyResponseModePriorityAndKeywords(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want ResponseMode
	}{
		{"en error", "why does the service error out", BugAnalysis},
		{"cn 报错", "欧区服务半夜报错", BugAnalysis},
		{"cn 超时", "接口超时了", BugAnalysis},
		{"cn 挂了", "服务挂了", BugAnalysis},
		{"pasted trace id", "trace id: 12345678-1234-1234-1234-1234567890ab", BugAnalysis},
		{"pasted uuid", "see 12345678-1234-1234-1234-1234567890ab", BugAnalysis},
		{"kibana url", "https://kibana.example.com/app/discover", BugAnalysis},
		{"cn 能不能", "能不能加重试", RequirementsAnalysis},
		{"en implement", "how to implement a retry", RequirementsAnalysis},
		{"cn 新增", "想新增一个接口", RequirementsAnalysis},
		{"cn 架构", "为什么用这个架构", ArchitectureReview},
		{"cn 双数据源", "双数据源怎么写的", ArchitectureReview},
		{"en tradeoff", "tradeoff between consistency and availability", ArchitectureReview},
		{"cn 这段代码", "这段代码有什么问题", CodeReview},
		{"en refactor", "should I refactor this method", CodeReview},
		{"general", "这个服务是干什么的", CodebaseQA},
		{"empty", "", CodebaseQA},
		{"bug+req", "报错了能不能加个重试", BugAnalysis},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyResponseMode(c.q); got != c.want {
				t.Errorf("ClassifyResponseMode(%q) = %q, want %q", c.q, got, c.want)
			}
		})
	}
}

func TestResponseModeSignalsNonEmpty(t *testing.T) {
	for _, mode := range ResponseModeOrder {
		if len(ResponseModeSignals[mode]) == 0 {
			t.Errorf("ResponseModeSignals[%q] = empty", mode)
		}
	}
}
