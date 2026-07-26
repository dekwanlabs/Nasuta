package retrieval

import (
	"regexp"
	"strings"
)

type QueryTerms struct {
	DomainTerms []string
	Identifiers []string
}

// helperMaxTokens bounds the pre-retrieval LLM helpers (preprocess / query terms).
// They return tiny JSON, so an unbounded budget only risks a reasoning model
// burning tokens on invisible thinking before visible output.
const helperMaxTokens = 1024

func (qt QueryTerms) normalize() QueryTerms {
	dedupe := func(in []string, keepCase bool) []string {
		seen := map[string]bool{}
		out := in[:0]
		for _, t := range in {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			key := strings.ToLower(t)
			if seen[key] || domainTermNoise[key] {
				continue
			}
			seen[key] = true
			if keepCase {
				out = append(out, t)
			} else {
				out = append(out, key)
			}
		}
		return out
	}
	domain := dedupe(qt.DomainTerms, false)
	idents := dedupe(qt.Identifiers, true)

	// Hard caps: the prompt already asks for limits, but LLMs are unreliable.
	// A broad domain term pollutes BM25/codegraph across the whole workspace;
	// fewer, more specific terms produce strictly better recall.
	if len(domain) > 5 {
		domain = domain[:5]
	}
	if len(idents) > 5 {
		idents = idents[:5]
	}
	return QueryTerms{domain, idents}
}

// domainTermNoise lists terms too generic to discriminate any subset of
// a codebase. They appear in so many files that matching them is noise.
var domainTermNoise = map[string]bool{
	"fan": true, "sensor": true, "switch": true, "climate": true, "light": true,
	"broker": true, "online": true, "offline": true, "status": true, "speed": true,
	"overview": true, "controller": true, "service": true, "api": true, "config": true,
	"topic": true, "payload": true, "device_type": true, "device_id": true,
	"state_topic": true, "command_topic": true, "availability_topic": true,
	"application_credentials": true, "manifest": true,
}

func (qt QueryTerms) allTerms() []string {
	out := make([]string, 0, len(qt.DomainTerms)+len(qt.Identifiers))
	out = append(out, qt.DomainTerms...)
	for _, id := range qt.Identifiers {
		if isCodeIdentifier(id) {
			out = append(out, strings.ToLower(id))
		}
	}
	return out
}

var enWordRe = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9_-]{2,}\b`)

var shortTechTerms = map[string]bool{
	"jwt": true, "api": true, "mq": true, "ota": true, "dao": true,
	"cron": true, "uri": true,
}

var codeloomStopwords = map[string]bool{
	"get_service": true, "search_code": true, "search_runbooks": true,
	"trace_deps": true, "list_apis": true, "check_docs": true,
	"get_symbol": true, "trace_calls": true, "index_stats": true,
	"traceid": true, "requestid": true, "runbook": true,
}

func ExtractTechTerms(question string) []string {
	var terms []string
	seen := map[string]bool{}
	for _, m := range enWordRe.FindAllString(question, -1) {
		low := strings.ToLower(m)
		if codeloomStopwords[low] {
			continue
		}
		if len(low) >= 4 || shortTechTerms[low] {
			if !seen[low] {
				seen[low] = true
				terms = append(terms, low)
			}
		}
	}
	return terms
}

var codeGraphIntentRe = regexp.MustCompile(`(?i)(调用链|调用关系|谁调用|被谁调用|调用方|被调用方|方法实现|函数实现|类定义|符号定义|写入路径|落库路径|call[ _-]?chain|caller|callee|callers|callees|method[ _-]?body|function[ _-]?body|symbol|implementation|write[ _-]?path)`)

var codeIdentifierRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:[.#:][A-Za-z_$][A-Za-z0-9_$]*)*(?:\(\))?$`)

func isCodeIdentifier(value string) bool {
	return codeIdentifierRe.MatchString(strings.TrimSpace(value))
}

func shouldExpandCodeGraph(question string, terms QueryTerms) bool {
	for _, identifier := range terms.Identifiers {
		if isCodeIdentifier(identifier) {
			return true
		}
	}
	return codeGraphIntentRe.MatchString(strings.TrimSpace(question))
}

// codegraphStopwords filters noisy tokens out of codegraph queries.
var codegraphStopwords = map[string]bool{
	"public": true, "private": true, "protected": true, "static": true, "final": true,
	"class": true, "interface": true, "enum": true, "void": true, "return": true,
	"new": true, "this": true, "super": true, "string": true, "int": true, "long": true,
	"boolean": true, "double": true, "float": true, "object": true, "value": true,
	"get": true, "set": true, "is": true, "has": true, "import": true, "package": true,
	"throws": true, "throw": true, "try": true, "catch": true, "null": true,
	"true": true, "false": true, "default": true, "override": true, "extends": true,
	"implements": true, "service": true, "impl": true, "controller": true, "manager": true,
	"config": true, "util": true, "helper": true, "base": true, "abstract": true, "api": true,
	"info": true, "error": true, "warn": true, "debug": true, "data": true, "type": true,
	"name": true, "id": true, "list": true, "map": true, "java": true, "lang": true,
	"com": true, "org": true, "io": true,
}

// buildCodeGraphKeywords assembles a cleaned, capped keyword list for codegraph
// from grounded retrieval terms only: anchored services plus query identifiers
// and domain terms. It does not derive extra keywords from service-name shape.
func (retrieve *Retriever) buildCodeGraphKeywords(services []string, terms QueryTerms) []string {
	const maxKeywords = 20
	add := func(out []string, seen map[string]bool, kw string) []string {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw == "" || len(kw) < 3 || codegraphStopwords[kw] || codeloomStopwords[kw] || seen[kw] {
			return out
		}
		seen[kw] = true
		return append(out, kw)
	}
	seen := map[string]bool{}
	out := make([]string, 0, maxKeywords)
	for _, s := range services {
		out = add(out, seen, s)
	}
	for _, id := range terms.Identifiers {
		if isCodeIdentifier(id) {
			out = add(out, seen, id)
		}
	}
	for _, d := range terms.DomainTerms {
		out = add(out, seen, d)
	}
	if len(out) > maxKeywords {
		out = out[:maxKeywords]
	}
	return out
}
