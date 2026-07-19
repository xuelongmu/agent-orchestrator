package domain

import (
	"reflect"
	"testing"
)

func TestSessionDependencyIDEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []SessionID
	}{
		{name: "ordinary", ids: []SessionID{"ao-1", "ao-2"}},
		{name: "embedded nul remains one id", ids: []SessionID{"ao-1\x00ao-2"}},
		{name: "duplicates round trip for store normalization", ids: []SessionID{"ao-1", "ao-1"}},
		{name: "whitespace round trips for store validation", ids: []SessionID{" ao-1 "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := EncodeSessionDependencyIDs(tc.ids)
			got, err := DecodeSessionDependencyIDs(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.ids) {
				t.Fatalf("round trip = %#v, want %#v (encoded %q)", got, tc.ids, encoded)
			}
		})
	}
	if encoded := EncodeSessionDependencyIDs(nil); encoded != "" {
		t.Fatalf("empty encoding = %q, want empty string", encoded)
	}
	if got, err := DecodeSessionDependencyIDs(""); err != nil || got != nil {
		t.Fatalf("empty decode = %#v, %v", got, err)
	}
	for _, malformed := range []string{"not-json", `{"ao-1":true}`, "null", `["ao-1"`} {
		if _, err := DecodeSessionDependencyIDs(malformed); err == nil {
			t.Fatalf("malformed encoding %q accepted", malformed)
		}
	}
}
