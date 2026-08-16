package delegation

import (
	"reflect"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestDelegationAdoptionContractIncludesOnlyAdoptableReports(t *testing.T) {
	contract := delegationAdoptionContract(agentapi.DelegationBatchResult{
		DelegationID: "del-1",
		Results: []agentapi.DelegationReport{
			{ReportID: "report-complete", Status: agentapi.DelegationCompleted},
			{ReportID: "report-rejected", Status: agentapi.DelegationRejected},
			{Status: agentapi.DelegationCompleted},
			{ReportID: "report-complete", Status: agentapi.DelegationCompleted},
			{ReportID: "report-partial", Status: agentapi.DelegationPartial},
		},
		Verification: &agentapi.DelegationVerification{
			VerificationID: "verification-1",
			Status:         agentapi.DelegationCompleted,
		},
	})
	if len(contract.Delegations) != 1 {
		t.Fatalf("contract = %#v", contract)
	}
	want := []string{"report-complete", "report-partial"}
	if got := contract.Delegations[0].ReportIDs; !reflect.DeepEqual(got, want) {
		t.Fatalf("report IDs = %#v, want %#v", got, want)
	}
}
