package agent

import "testing"

func TestShortCircuitMetaRequiresExactPhrase(t *testing.T) {
	if !shouldShortCircuitMeta("你能做什么？") {
		t.Fatal("exact platform capability question should short-circuit")
	}
	if shouldShortCircuitMeta("你知道我叫什么名字吗") {
		t.Fatal("personal fact question must remain eligible for memory planning")
	}
	if shouldShortCircuitMeta("这个项目叫什么名字") {
		t.Fatal("workspace fact question must remain eligible for internal planning")
	}
}
