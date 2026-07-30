package format_test

import (
	"encoding/json"
	"testing"

	"github.com/ajthom90/breakwater/pkg/format"
)

func TestTreeObjectJSON(t *testing.T) {
	tree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "hello.txt", Type: format.EntryFile, Size: 5, ObjectID: "abc"},
			{Name: "sub", Type: format.EntryDir, ObjectID: "def"},
		},
	}
	b, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var got format.TreeObject
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].Name != "hello.txt" {
		t.Fatalf("%+v", got)
	}
}
