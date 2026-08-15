package memory

import (
	"reflect"
	"testing"
)

func TestCanonicalQuestionMetadataUsesDomainEntityIdentity(t *testing.T) {
	_, entities, _ := CanonicalQuestionMetadata(
		"继续检查 PaymentHandler.handle() 和 HS-USER-SERVICE",
	)
	want := []string{"paymenthandler.handle", "hs-user-service"}
	if !reflect.DeepEqual(entities, want) {
		t.Fatalf("entities = %v, want %v", entities, want)
	}
}
