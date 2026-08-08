package run

func toolResultPreview(content string) string {
	const maxRunes = 1200
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[:maxRunes]) + "..."
}
