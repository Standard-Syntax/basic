package publication

import (
	"reflect"
	"strings"
	"testing"
)

func TestRuntimePublicationPortsHaveNoCleanupOrMergeAuthority(t *testing.T) {
	for _, port := range []reflect.Type{
		reflect.TypeOf((*GitPublisher)(nil)).Elem(),
		reflect.TypeOf((*PullRequestClient)(nil)).Elem(),
	} {
		for index := 0; index < port.NumMethod(); index++ {
			method := port.Method(index)
			for _, forbidden := range []string{"Close", "Delete", "Merge", "Deploy"} {
				if strings.HasPrefix(method.Name, forbidden) {
					t.Fatalf("runtime port %s exposes forbidden %s authority",
						port.Name(), method.Name)
				}
			}
		}
	}
}
