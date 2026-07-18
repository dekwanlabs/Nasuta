package types

import (
	"regexp"
	"strings"
	"time"
)

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type AgentConfig struct {
	MaxSteps      int           `json:"max_steps"`
	HistoryLimit  int           `json:"history_limit"`
	Timeout       time.Duration `json:"timeout"`
	AnswerReserve time.Duration `json:"answer_reserve"`
}

type ResponseMode string

const (
	BugAnalysis          ResponseMode = "bug_analysis"
	RequirementsAnalysis ResponseMode = "requirements_analysis"
	ArchitectureReview   ResponseMode = "architecture_review"
	CodeReview           ResponseMode = "code_review"
	CodebaseQA           ResponseMode = "codebase_qa"
)

var ResponseModeOrder = []ResponseMode{
	BugAnalysis,
	RequirementsAnalysis,
	ArchitectureReview,
	CodeReview,
}

var ResponseModeSignals = map[ResponseMode][]string{
	BugAnalysis: {
		"error", "exception", "bug", "failed", "failure", "crash", "incident",
		"timeout", "nullpointer", "npe", "stacktrace", "trace id", "traceid",
		"kibana", "5xx", "unavailable",
		"500", "502", "503", "504",
		"panic", "oom", "deadlock",
		"什么原因", "报错", "出错", "异常", "失败", "超时", "挂了", "不可用",
		"崩了", "重启", "打不开", "不响应", "内存溢出",
		"エラー", "バグ", "落ちた", "タイムアウト", "障害",
		"오류", "버그", "장애", "타임아웃",
		"erreur", "panne", "plantage", "indisponible",
		"fehler", "absturz", "ausgefallen", "nicht verfügbar",
	},
	RequirementsAnalysis: {
		"implement", "add a", "add an", "new feature", "new endpoint", "new api",
		"what's needed", "what is needed", "how to build", "how to implement",
		"how would you", "how to add", "how to create",
		"能不能", "可以加", "可以做个", "能否", "需求", "实现", "开发", "新增",
		"增加一个", "做一个", "想加", "能加吗", "新建",
		"追加", "作って", "実装", "機能",
		"추가", "구현", "만들어", "기능",
		"ajouter", "implémenter", "créer", "fonctionnalité",
		"implementieren", "hinzufügen", "funktionalität",
	},
	ArchitectureReview: {
		"architecture", "design pattern", "system design",
		"data source", "datasource", "dual datasource",
		"trade-off", "tradeoff", "topology", "why is",
		"scalability", "coupling", "bottleneck", "data flow",
		"为什么", "架构", "设计", "数据源", "双数据源", "双写",
		"解耦", "瓶颈", "数据流",
		"構造", "なぜ", "アーキテクチャ",
		"아키텍처", "구조", "설계",
		"conception", "pourquoi",
		"architektur", "entwurf", "warum",
	},
	CodeReview: {
		"review", "code quality", "best practice", "refactor",
		"code smell", "anti-pattern",
		"有问题", "这段代码", "这个写法", "代码审查",
		"コードレビュー", "リファクタリング",
		"코드 리뷰", "리팩토링",
		"revue de code",
		"refaktorisierung",
	},
}

var TraceIDRe = regexp.MustCompile(`(?i)(?:trace[_\-\s]?id|traceid|trace)\s*[=:：]?\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
var UUIDRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
var KibanaURLRe = regexp.MustCompile(`(?i)https?://[^\s]*kibana[^\s]*`)

func ClassifyResponseMode(question string) ResponseMode {
	q := strings.ToLower(question)
	if TraceIDRe.MatchString(q) || UUIDRe.MatchString(q) || KibanaURLRe.MatchString(q) {
		return BugAnalysis
	}
	for _, mode := range ResponseModeOrder {
		for _, kw := range ResponseModeSignals[mode] {
			if strings.Contains(q, kw) {
				return mode
			}
		}
	}
	return CodebaseQA
}
