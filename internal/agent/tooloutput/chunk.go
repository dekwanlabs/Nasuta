package tooloutput

import "encoding/json"

type chunk struct {
	ref              string
	kind             string
	raw              json.RawMessage
	text             string
	contextRefs      []string
	ordinal          int
	itemRef          string
	contentTruncated bool
}

func (c chunk) searchableText() string {
	if c.text != "" {
		return c.ref + "\n" + c.text
	}
	return c.ref + "\n" + string(c.raw)
}

func (c chunk) envelopeContent() any {
	if c.kind == "json" {
		return c.raw
	}
	return c.text
}

type ancestorContext struct {
	ref    string
	fields map[string]any
}
