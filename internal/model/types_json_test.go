package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestItemKeyIsPrivateJSONState(t *testing.T) {
	payload, err := json.Marshal(Item{Key: "local-private", Id: "server-1", Title: "milk"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "local-private") || strings.Contains(string(payload), "Key") {
		t.Fatalf("private item key reached JSON: %s", payload)
	}
	if !strings.Contains(string(payload), "server-1") {
		t.Fatalf("public server id is missing from JSON: %s", payload)
	}
}
