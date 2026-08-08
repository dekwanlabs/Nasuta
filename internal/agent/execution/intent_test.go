package execution

import "testing"

func TestShortCircuitMetaRequiresExactPhrase(t *testing.T) {
	if !ShouldShortCircuitMeta("你能做什么？") {
		t.Fatal("exact platform capability question should short-circuit")
	}
	if ShouldShortCircuitMeta("你知道我叫什么名字吗") {
		t.Fatal("personal fact question must remain eligible for memory planning")
	}
	if ShouldShortCircuitMeta("这个项目叫什么名字") {
		t.Fatal("workspace fact question must remain eligible for internal planning")
	}
}
