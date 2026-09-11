package store

import (
	"bytes"
	"testing"
)

func TestEmptyReplacementAllowsOmittedItemsOnlyForConfirmedTextV2(t *testing.T) {
	items := []FeatureItemSnapshot{}
	prior := []FeatureItemBinding{{Key: "removed", ID: "old"}}
	kind := "TEXT"
	note, checklist := "NOTE", "CHECKLIST"
	for _, tc := range []struct {
		name, raw string
		version   int
		kind      *string
		pass      bool
		nonempty  bool
	}{
		{"v2 cleared", `{"id":"t","projectId":"p","kind":"TEXT"}`, 2, &kind, true, false},
		{"v1 unchanged", `{"id":"t","projectId":"p","kind":"TEXT"}`, 1, &kind, false, false},
		{"v1 no kind intent", `{"id":"t","projectId":"p","kind":"TEXT"}`, 1, nil, false, false},
		{"explicit null", `{"id":"t","projectId":"p","kind":"TEXT","items":null}`, 2, &kind, false, false},
		{"null no kind intent", `{"id":"t","projectId":"p","kind":"TEXT","items":null}`, 2, nil, false, false},
		{"no kind intent", `{"id":"t","projectId":"p","kind":"TEXT"}`, 2, nil, true, false},
		{"nonempty no kind intent", `{"id":"t","projectId":"p","kind":"TEXT"}`, 2, nil, false, true},
		{"wrong kind", `{"id":"t","projectId":"p","kind":"CHECKLIST"}`, 2, &kind, false, false},
		{"wrong kind no intent", `{"id":"t","projectId":"p","kind":"CHECKLIST"}`, 2, nil, false, false},
		{"missing kind", `{"id":"t","projectId":"p"}`, 2, &kind, false, false},
		{"missing kind no intent", `{"id":"t","projectId":"p"}`, 2, nil, false, false},
		{"null kind no intent", `{"id":"t","projectId":"p","kind":null}`, 2, nil, false, false},
		{"different requested kind", `{"id":"t","projectId":"p","kind":"TEXT"}`, 2, &note, false, false},
		{"requested checklist cannot normalize", `{"id":"t","projectId":"p","kind":"TEXT"}`, 2, &checklist, false, false},
		{"wrong task address", `{"id":"other","projectId":"p","kind":"TEXT"}`, 2, nil, false, false},
		{"wrong project address", `{"id":"t","projectId":"other","kind":"TEXT"}`, 2, nil, false, false},
		{"explicit empty", `{"id":"t","projectId":"p","kind":"TEXT","items":[]}`, 2, &kind, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantItems := items
			if tc.nonempty {
				wantItems = []FeatureItemSnapshot{{Key: "kept", Title: "must remain", SortOrder: 1}}
			}
			send := FeatureSend{Metadata: FeaturePayloadMetadata{Version: tc.version, Fields: FeatureFields{Items: true, Kind: tc.kind != nil}, Snapshot: &FeatureSnapshot{Items: &wantItems, PriorBindings: &prior, Kind: tc.kind}}}
			got, err := ConfirmFeatureResponse(send, "t", "p", []byte(tc.raw))
			if (err == nil) != tc.pass || !bytes.Equal(got.Raw, []byte(tc.raw)) {
				t.Fatalf("confirmation %+v %v", got, err)
			}
		})
	}
}
