package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

var (
	serviceURLRe       = regexp.MustCompile(`(?i)(?:https?|lb)://([a-z0-9._-]+)(?::\d+)?`)
	grpcTargetRe       = regexp.MustCompile(`(?i)(?:forAddress|insecure_channel|secure_channel|grpc\.Dial)\s*\(\s*["']([^"']+)["']`)
	protocolHTTPCallRe = regexp.MustCompile(`(?is)(?:restTemplate|webClient|httpClient)[\w.\s]*\(|(?:requests|httpx|aiohttp)\.[A-Za-z]+\s*\(`)
	dubboRefRe         = regexp.MustCompile(`@(?:DubboReference|Reference)\s*\([^\n)]*interfaceClass\s*=\s*([A-Za-z][\w.]*)\.class`)

	websocketURLRe       = regexp.MustCompile(`(?i)\b(ws|wss)://[^"'\s,;()]+`)
	loadBalancerChooseRe = regexp.MustCompile(`(?m)\bloadBalancer\s*\.\s*choose\s*\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)`)

	kafkaSendRe         = regexp.MustCompile(`(?m)(?:\.send\s*\(\s*|ProducerRecord(?:<[^>]*>)?\s*\(\s*)["']([^"']+)["']`)
	kafkaListenPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?m)@KafkaListener\s*\([^\n)]*(?:topics\s*=\s*)?["']([^"']+)["']`),
		regexp.MustCompile(`(?m)(?:\.subscribe\s*\(\s*(?:List\.of\s*\()?|KafkaConsumer\s*\(\s*)["']([^"']+)["']`),
	}
)

// scanJVMAndPythonDependencies covers literal HTTP, gRPC and Dubbo clients.
func scanJVMAndPythonDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return supportedProtocolFile(name)
	})
	edges := make([]domain.DependencyEdge, 0)
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if text == "" {
			continue
		}
		caller := dependencyIdentity(root, file)
		rel := relativeTo(root, file)
		if hasHTTPClient(text) {
			for _, match := range serviceURLRe.FindAllStringSubmatchIndex(text, -1) {
				if len(match) < 4 || !httpURLUsedByClient(text, match[0], match[1], protocolHTTPCallRe) {
					continue
				}
				target := text[match[2]:match[3]]
				if skipDependencyTarget(target) {
					continue
				}
				edges = append(edges, protocolEdge(caller, target, domain.EdgeHTTP, rel, lineAt(text, match[0]), 0.7))
			}
		}
		for _, match := range grpcTargetRe.FindAllStringSubmatchIndex(text, -1) {
			target := strings.Split(text[match[2]:match[3]], ":")[0]
			if skipDependencyTarget(target) {
				continue
			}
			edges = append(edges, protocolEdge(caller, target, domain.EdgeGRPC, rel, lineAt(text, match[0]), 0.75))
		}
		if filepath.Ext(file) == ".java" || filepath.Ext(file) == ".kt" {
			for _, match := range dubboRefRe.FindAllStringSubmatchIndex(text, -1) {
				target := text[match[2]:match[3]]
				edges = append(edges, protocolEdge(caller, target, domain.EdgeRPC, rel, lineAt(text, match[0]), 0.7))
			}
		}
	}
	return edges
}

type topicUse struct {
	service  serviceIdentity
	topic    string
	evidence domain.Evidence
}

// scanWebSocketDependencies indexes Netty/raw WebSocket clients. Unlike the
// generic HTTP scanner, the target is usually a Spring `@Value` placeholder
// (for example `${speech.transcribe.service.name:tts-proxy}`). We keep that
// expression as a placeholder so the tool layer can resolve it against the
// active environment rather than baking in the fallback at scan time.
func scanWebSocketDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return filepath.Ext(name) == ".java" || filepath.Ext(name) == ".kt"
	})
	edges := make([]domain.DependencyEdge, 0)
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if text == "" || !hasNettyWebSocketClient(text) {
			continue
		}
		caller := dependencyIdentity(root, file)
		rel := relativeTo(root, file)
		seen := make(map[string]struct{})
		for _, target := range websocketTargets(text, caller.Name) {
			if skipWebSocketDependencyTarget(target) {
				continue
			}
			if _, duplicate := seen[target]; duplicate {
				continue
			}
			seen[target] = struct{}{}
			edges = append(edges, websocketEdge(caller, target, rel, text))
		}
	}
	return edges
}

func hasNettyWebSocketClient(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "websocketclientbootstrap") ||
		strings.Contains(lower, "websocketclient") ||
		strings.Contains(lower, "new websocketclient") ||
		strings.Contains(lower, "uri.create") && strings.Contains(lower, "ws://")
}

func websocketTargets(text, caller string) []string {
	loadBalancer := loadBalancerServiceNames(text)
	for _, name := range loadBalancer {
		if expr := springValueForField(text, name); expr != "" {
			return []string{expr}
		}
		return []string{name}
	}
	// A literal ws://host/path URI can only be matched to a workspace service
	// by the host part; keep the host as a best-effort target.
	targets := make([]string, 0, 1)
	for _, match := range websocketURLRe.FindAllString(text, -1) {
		target := normalizeWebSocketHost(match)
		if target != "" {
			targets = append(targets, target)
			break
		}
	}
	return targets
}

func loadBalancerServiceNames(text string) []string {
	matches := loadBalancerChooseRe.FindAllStringSubmatch(text, -1)
	names := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		name := strings.TrimSpace(match[1])
		if name == "" || name == "this" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func springValueForField(text, field string) string {
	if field == "" {
		return ""
	}
	pattern := regexp.MustCompile(`(?s)@Value\s*\(\s*"([^"]+)"\s*\)[^;]*?\b` +
		regexp.QuoteMeta(field) + `\b`)
	match := pattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	expression := strings.TrimSpace(match[1])
	if strings.HasPrefix(expression, "${") && strings.HasSuffix(expression, "}") {
		return expression
	}
	return expression
}

func normalizeWebSocketHost(uri string) string {
	uri = strings.TrimPrefix(strings.TrimPrefix(uri, "ws://"), "wss://")
	host, _, _ := strings.Cut(uri, "/")
	host, _, _ = strings.Cut(host, ":")
	return strings.ToLower(strings.TrimSpace(host))
}

func websocketEdge(caller serviceIdentity, target, path, text string) domain.DependencyEdge {
	edge := protocolEdge(caller, target, domain.EdgeWebSocket, path, 0, 0.75)
	line := websocketTargetLine(text, target)
	if line > 0 {
		edge.Evidence[0].Line = line
	}
	edge.TargetExpression = target
	return edge
}

func websocketTargetLine(text, target string) int {
	idx := strings.Index(text, target)
	if idx < 0 {
		return 0
	}
	return lineAt(text, idx)
}

// scanKafkaDependencies joins literal producer topics to workspace consumers.
func scanKafkaDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return supportedProtocolFile(name)
	})
	producers := make([]topicUse, 0)
	consumersByTopic := make(map[string][]topicUse)
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		lower := strings.ToLower(text)
		if !strings.Contains(lower, "kafka") {
			continue
		}
		service := dependencyIdentity(root, file)
		rel := relativeTo(root, file)
		for _, match := range kafkaSendRe.FindAllStringSubmatchIndex(text, -1) {
			producers = append(producers, topicUse{
				service: service, topic: text[match[2]:match[3]],
				evidence: domain.Evidence{Path: rel, Line: lineAt(text, match[0]), Kind: domain.SourceCodeScan},
			})
		}
		for _, pattern := range kafkaListenPatterns {
			for _, match := range pattern.FindAllStringSubmatchIndex(text, -1) {
				use := topicUse{
					service: service, topic: text[match[2]:match[3]],
					evidence: domain.Evidence{Path: rel, Line: lineAt(text, match[0]), Kind: domain.SourceCodeScan},
				}
				consumersByTopic[use.topic] = append(consumersByTopic[use.topic], use)
			}
		}
	}

	edges := make([]domain.DependencyEdge, 0, len(producers))
	for _, producer := range producers {
		consumers := consumersByTopic[producer.topic]
		joined := false
		for _, consumer := range consumers {
			if consumer.service.Key != "" && consumer.service.Key == producer.service.Key {
				continue
			}
			joined = true
			edges = append(edges, domain.DependencyEdge{
				CallerServiceKey: producer.service.Key,
				From:             producer.service.Name, To: consumer.service.Name, Type: domain.EdgeKafka,
				Evidence: []domain.Evidence{producer.evidence, consumer.evidence}, Confidence: 0.85,
			})
		}
		if !joined {
			edges = append(edges, domain.DependencyEdge{
				CallerServiceKey: producer.service.Key,
				From:             producer.service.Name, To: "kafka:" + producer.topic, Type: domain.EdgeKafka,
				Evidence: []domain.Evidence{producer.evidence}, Confidence: 0.65,
			})
		}
	}
	return edges
}

func findPythonDependencyRoot(root, file string) string {
	current := filepath.Dir(file)
	for {
		for _, marker := range []string{"main.py", ".env.example", "pyproject.toml"} {
			if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current || !strings.HasPrefix(parent, root) {
			return filepath.Dir(file)
		}
		current = parent
	}
}

func hasHTTPClient(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "resttemplate") || strings.Contains(lower, "webclient") ||
		strings.Contains(lower, "httpclient") || strings.Contains(lower, "okhttp") ||
		strings.Contains(lower, "requests.") || strings.Contains(lower, "httpx.") ||
		strings.Contains(lower, "aiohttp")
}

func supportedProtocolFile(name string) bool {
	switch filepath.Ext(name) {
	case ".java", ".kt", ".py":
		return true
	default:
		return false
	}
}

func protocolEdge(caller serviceIdentity, to string, edgeType domain.EdgeType, path string, line int, confidence float64) domain.DependencyEdge {
	return domain.DependencyEdge{
		CallerServiceKey: caller.Key,
		From:             caller.Name, To: to, Type: edgeType,
		Evidence:   []domain.Evidence{{Path: path, Line: line, Kind: domain.SourceCodeScan}},
		Confidence: confidence,
	}
}

func skipDependencyTarget(target string) bool {
	target = strings.ToLower(target)
	return target == "" || strings.Contains(target, "localhost") || strings.Contains(target, "127.0.0.1") || strings.Contains(target, "${")
}

// skipWebSocketDependencyTarget keeps Spring placeholders because they are the
// authoritative cross-environment target references, resolved at tool time.
func skipWebSocketDependencyTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return true
	}
	lower := strings.ToLower(target)
	return strings.Contains(lower, "localhost") || strings.Contains(lower, "127.0.0.1")
}

func lineAt(text string, offset int) int {
	return strings.Count(text[:offset], "\n") + 1
}
