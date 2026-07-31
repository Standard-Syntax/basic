package publication

import (
	"reflect"
	"testing"
)

func TestRuntimePublicationPortsHaveNoCleanupOrMergeAuthority(t *testing.T) {
	for _, port := range []reflect.Type{
		reflect.TypeOf((*GitPublisher)(nil)).Elem(),
		reflect.TypeOf((*PullRequestClient)(nil)).Elem(),
	} {
		for _, forbidden := range []string{"Close", "Delete", "Merge", "Deploy"} {
			if _, ok := port.MethodByName(forbidden); ok {
				t.Fatalf("runtime port %s exposes forbidden %s authority", port.Name(), forbidden)
			}
		}
	}
}
