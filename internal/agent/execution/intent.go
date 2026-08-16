package execution

import (
	"regexp"
	"slices"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

// AnswerInstructionFor derives answer organization without changing evidence access.
func AnswerInstructionFor(kind domain.QueryKind) string {
	return prompts.MustRender(prompts.AgentQAQueryKind, struct {
		Kind string
	}{Kind: string(kind)})
}

var metaCapabilityPhrases = []string{
	"你能做什么", "可以做什么", "能做什么", "能干什么", "能干嘛",
	"你会什么", "你可以做什么", "你能帮我做什么",
	"你支持什么", "你有哪些功能", "你有哪些能力",
	"有什么功能", "支持哪些", "可以用什么",
	"who are you", "what can you do", "what do you support",
	"what are your capabilities", "what do you do",
	"how can you help",
	"何ができる", "できること", "あなたは何", "あなたは誰", "何者",
	"どんな機能", "何ができます",
	"뭘 할 수 있", "무엇을 할 수", "당신은 누구", "어떤 기능",
	"que peux-tu faire", "que pouvez-vous faire", "qui es-tu",
	"qui êtes-vous", "tes capacités",
	"was kannst du", "wer bist du", "deine fähigkeiten",
	"你是什么模型", "你是谁", "你是哪个模型", "你叫什么", "你叫什么名字",
	"用什么模型", "用的什么模型", "你是基于什么",
	"你是claude", "你是gpt", "你是chatgpt", "你是ai吗", "你是机器人吗",
	"what model are you", "which model are you", "are you an ai",
	"are you a bot", "are you chatgpt", "are you claude",
	"何のモデル", "どのモデル", "aiですか", "ロボットですか",
	"어떤 모델", "어떤 모델이", "ai야", "봇이야",
	"quel modèle es-tu", "quel modèle êtes-vous",
	"es-tu une ia", "es-tu un bot",
	"welches modell bist du", "bist du eine ki", "bist du ein bot",
	"你比gpt", "你比chatgpt", "你比gpt厉害", "比gpt强", "比chatgpt强",
	"你比谁厉害", "你和gpt", "你和chatgpt", "和gpt比", "和chatgpt比",
	"谁更厉害", "哪个更强", "谁更好",
	"better than gpt", "better than chatgpt", "smarter than gpt",
	"are you better", "how do you compare",
	"gptより", "chatgptより", "どっちがすごい", "どちらが優れ",
	"gpt보다", "chatgpt보다", "누가 더 똑똑",
	"meilleur que gpt", "meilleur que chatgpt", "plus intelligent que gpt",
	"besser als gpt", "besser als chatgpt", "schlauer als gpt",
}

var metaSignalRe = regexp.MustCompile(`(?i)(hs[a-z]+-[a-z]|trace[_\-\s]?id|\.[a-z]{2,4}\b|[a-z_][a-z0-9_]*\.[a-z][a-z0-9_]*)`)

// ShouldShortCircuitMeta identifies exact capability questions that need no evidence lookup.
func ShouldShortCircuitMeta(question string) bool {
	q := strings.Trim(strings.TrimSpace(question), "?？!！。.")
	if len([]rune(q)) >= 30 {
		return false
	}
	low := strings.ToLower(q)
	if !slices.Contains(metaCapabilityPhrases, low) {
		return false
	}
	return !metaSignalRe.MatchString(q)
}
