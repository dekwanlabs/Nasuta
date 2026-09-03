package qa

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestPreferredToolsInstructionIsAdvisory(t *testing.T) {
	instruction := preferenceInstruction([]string{"runtime"}, false)
	for _, want := range []string{"runtime", "advisory, not mandatory", "answer directly", "Other registered tools remain available"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, instruction)
		}
	}
	if strings.Contains(instruction, "must call") || strings.Contains(instruction, "required") {
		t.Fatalf("preference was expressed as a requirement: %s", instruction)
	}
}

func TestPreferredToolsInstructionDefersToDelegation(t *testing.T) {
	instruction := preferenceInstruction([]string{"runtime"}, true)
	for _, want := range []string{"runtime", "isolate named subjects", "delegate_investigation"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, instruction)
		}
	}
	if strings.Contains(instruction, "answer directly") {
		t.Fatalf("delegation preference still licensed a direct answer: %s", instruction)
	}
}

func TestPrepareRunConversationRequiresDelegationOnParentDynamicRoute(t *testing.T) {
	svc := &Service{}
	prepared := &preparation{
		execution: executionRouteDecision{RouteReason: routeReasonParentDynamicDelegation},
		planning:  evidencePlanningOutput{RoutedToolIDs: []string{"runtime"}},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{
			ID: "delegate_investigation",
		}}},
	}
	conversation := svc.prepareRunConversation(prepared)
	if !hasDelegationContract(conversation) {
		t.Fatalf("instructions = %+v, want parent delegation contract", conversation.Instructions)
	}
	if !hasPreferenceWithoutDirectAnswer(conversation) {
		t.Fatalf("instructions = %+v, want preferred tools not to bypass delegation", conversation.Instructions)
	}
}

func TestPrepareRunConversationSkipsDelegationContractWhenRouteGatesFanout(t *testing.T) {
	svc := &Service{}
	prepared := &preparation{
		execution: executionRouteDecision{RouteReason: routeReasonMultiAgentNotWorthwhile},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{
			ID: "delegate_investigation",
		}}},
	}
	conversation := svc.prepareRunConversation(prepared)
	if hasDelegationContract(conversation) {
		t.Fatalf("instructions = %+v, want no contract for a gated route", conversation.Instructions)
	}
}

func TestPrepareRunConversationSkipsDelegationContractWhenToolMissing(t *testing.T) {
	svc := &Service{}
	prepared := &preparation{
		execution: executionRouteDecision{RouteReason: routeReasonParentDynamicDelegation},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{
			ID: "search_runbooks",
		}}},
	}
	conversation := svc.prepareRunConversation(prepared)
	if hasDelegationContract(conversation) {
		t.Fatalf("instructions = %+v, want no contract without the tool", conversation.Instructions)
	}
}

func hasDelegationContract(conversation ConversationContext) bool {
	for _, message := range conversation.Instructions {
		if strings.Contains(message.Content, "DELEGATION_CONTRACT") &&
			strings.Contains(message.Content, "delegate_investigation") {
			return true
		}
	}
	return false
}

func hasPreferenceWithoutDirectAnswer(conversation ConversationContext) bool {
	for _, message := range conversation.Instructions {
		if !strings.Contains(message.Content, "Tool routing preference") {
			continue
		}
		return strings.Contains(message.Content, "delegate_investigation") &&
			!strings.Contains(message.Content, "answer directly")
	}
	return false
}
