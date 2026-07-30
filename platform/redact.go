package platform

import (
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const RedactedValue = "[REDACTED]"

var (
	privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`)
	uriPassword     = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/@\s:]+:)[^/@\s]+@`)
	sensitiveLine   = regexp.MustCompile(`(?im)^(\s*["']?[a-z0-9_.-]*(?:authorization|password|passwd|token|secret|credentials?|cookies?|private[-_.]?key|api[-_.]?key|access[-_.]?key(?:id)?|client[-_.]?secret)["']?\s*[:=]\s*).*$`)
	sensitiveText   = regexp.MustCompile(`(?i)((?:proxy-authorization|authorization|cookie|set-cookie|x-api-key|api[_-]?key|access[_-]?key(?:id)?|access[_-]?token|refresh[_-]?token|client[_-]?secret|password|passwd|credentials?|private[_-]?key|secret)\s*[:=]\s*["']?)(?:bearer\s+)?[^\s,"';}\]]+`)
)

// RedactSensitiveText removes credentials from structured or plain text.
func RedactSensitiveText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	if encoded, ok := redactJSONText(text); ok {
		return encoded
	}
	if encoded, ok := redactYAMLText(text); ok {
		return encoded
	}
	return redactPlainText(text)
}

// RedactConfigValue also treats a sensitive configuration key as authoritative.
func RedactConfigValue(key, value string) string {
	if isSensitiveKey(key) {
		return RedactedValue
	}
	return RedactSensitiveText(value)
}

// RedactSensitiveURL preserves routing fields while removing credentials.
func RedactSensitiveURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return RedactSensitiveText(rawURL)
	}
	changed := false
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), RedactedValue)
			changed = true
		}
	}
	query := parsed.Query()
	for key := range query {
		if isSensitiveKey(key) {
			query.Set(key, RedactedValue)
			changed = true
		}
	}
	if changed {
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return RedactSensitiveText(rawURL)
}

func redactJSONText(text string) (string, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return text, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return text, false
	}
	encoded, err := json.Marshal(redactJSONValue(value))
	if err != nil {
		return text, false
	}
	return string(encoded), true
}

func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if isSensitiveKey(key) {
				typed[key] = RedactedValue
				continue
			}
			typed[key] = redactJSONValue(item)
		}
	case []any:
		for i := range typed {
			typed[i] = redactJSONValue(typed[i])
		}
	case string:
		return RedactSensitiveText(typed)
	}
	return value
}

func redactYAMLText(text string) (string, bool) {
	var root yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&root); err != nil {
		return text, false
	}
	var trailing yaml.Node
	if decoder.Decode(&trailing) != io.EOF || !redactYAMLNode(&root) {
		return text, false
	}
	encoded, err := yaml.Marshal(&root)
	if err != nil {
		return text, false
	}
	return strings.TrimSuffix(string(encoded), "\n"), true
}

func redactYAMLNode(node *yaml.Node) bool {
	changed := false
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if isSensitiveKey(key.Value) {
				*value = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: RedactedValue}
				changed = true
				continue
			}
			changed = redactYAMLNode(value) || changed
		}
		return changed
	}
	for _, child := range node.Content {
		changed = redactYAMLNode(child) || changed
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		redacted := redactPlainText(node.Value)
		if redacted != node.Value {
			node.Value = redacted
			changed = true
		}
	}
	return changed
}

func redactPlainText(text string) string {
	text = privateKeyBlock.ReplaceAllString(text, RedactedValue)
	text = uriPassword.ReplaceAllString(text, `${1}`+RedactedValue+`@`)
	text = sensitiveText.ReplaceAllString(text, `${1}`+RedactedValue)
	return sensitiveLine.ReplaceAllString(text, `${1}`+RedactedValue)
}

func isSensitiveKey(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.ToLower(key))
	for _, suffix := range []string{
		"password", "passwd", "token", "secret", "credential", "credentials",
		"cookie", "cookies", "privatekey", "apikey", "accesskey", "accesskeyid",
		"authorization",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}
