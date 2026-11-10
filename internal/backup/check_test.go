package backup

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

func TestASubsetIsAGroupOfTheDataPacks(t *testing.T) {
	for subset, want := range map[string][2]int{
		"":        {0, 0},
		"1/1":     {1, 1},
		"3/20":    {3, 20},
		"20/20":   {20, 20},
		"256/256": {256, 256},
		"1/256":   {1, 256},
	} {
		n, total, err := ParseSubset(subset)
		if err != nil || n != want[0] || total != want[1] {
			t.Errorf("ParseSubset(%q) = %d, %d, %v", subset, n, total, err)
		}
	}
	// restic's random forms, and groups out of range
	for _, subset := range []string{
		"5%", "2.5%", "500M", "1G", "100", "0/20", "21/20", "1/257", "257/257", "1/0",
		"01/20", "3/020", "-1/20", "3/20/1", " 3/20", "3/20 ", "3 / 20", "n/t", "1000/1000",
	} {
		if n, total, err := ParseSubset(subset); err == nil {
			t.Errorf("ParseSubset(%q) = %d, %d, no error", subset, n, total)
		}
	}
}

func TestASubsetIsSaidFromAGroup(t *testing.T) {
	for _, c := range []struct {
		n, t int
		want string
	}{
		{1, 20, "1/20"}, {20, 20, "20/20"}, {0, 20, "1/20"}, {21, 20, "1/20"}, {-3, 20, "1/20"},
		{3, 0, ""}, {0, 0, ""}, {1, -1, ""}, {256, 256, "256/256"},
	} {
		if got := Subset(c.n, c.t); got != c.want {
			t.Errorf("Subset(%d, %d) = %q, want %q", c.n, c.t, got, c.want)
		}
		if got := Subset(c.n, c.t); got != "" {
			if _, _, err := ParseSubset(got); err != nil {
				t.Errorf("Subset(%d, %d) = %q, which ParseSubset refuses: %v", c.n, c.t, got, err)
			}
		}
	}
}

// jsonKeys are the JSON keys of a value.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestTheCheckAnswersSayEveryField(t *testing.T) {
	want := []string{"confirmed", "error", "errors", "errors_more", "lock", "log", "notes", "packs_read", "packs_total",
		"repair", "restic_code", "status", "subset", "unconfirmed"}
	if got := jsonKeys(t, CheckResult{}); !slices.Equal(got, want) {
		t.Errorf("a check's answer has %v, want %v", got, want)
	}
	want = []string{"error", "errors", "errors_more", "lock", "notes", "outcome", "packs_read", "packs_total", "pass",
		"repair", "restic_code", "subset"}
	if got := jsonKeys(t, CheckPass{}); !slices.Equal(got, want) {
		t.Errorf("a pass's line has %v, want %v", got, want)
	}
	if got := jsonKeys(t, CheckRepair{}); !slices.Equal(got, []string{"index", "other", "packs", "snapshots"}) {
		t.Errorf("a repair has %v", got)
	}
	if got := jsonKeys(t, RunFailure{}); len(got) != 0 {
		t.Errorf("a run that saved a snapshot says %v", got)
	}
	if got := jsonKeys(t, RunFailure{Failure: FailureLocked, ResticCode: 11, Lock: &Lock{PID: 1, Host: "nas", User: "root"}}); !slices.Equal(got, []string{"failure", "lock", "restic_code"}) {
		t.Errorf("a failed run says %v", got)
	}
}

// The agent's answer reaches the panel as a structpb.Struct: the agent puts
// the JSON of these types into a map, and the panel reads the result back.
func TestTheCheckAnswerCrossesTheStream(t *testing.T) {
	created := time.Date(2026, 11, 10, 13, 18, 55, 321000000, time.UTC)
	answer := CheckResult{
		Status: CheckStatusSuspect, Subset: "3/20", PacksRead: 118, PacksTotal: 2361,
		Errors:     []CheckError{{Kind: CheckErrorPack, ID: strings.Repeat("b6", 32), Text: "pack b659… contains 2 errors"}, {Kind: CheckErrorTree, ID: "07055eee", Text: "error for tree 07055eee:\n  decrypting blob failed"}},
		ErrorsMore: 4, Repair: CheckRepair{Packs: []string{strings.Repeat("b6", 32)}, Snapshots: true},
		Notes:      []string{"1 additional files were found in the repo, which likely contain duplicate data."},
		Lock:       &Lock{PID: 22562, Host: "pc-k3j9x2m4q8w1z7v", User: "root", Exclusive: true, Created: created, AgeS: 750, StorageID: "957548b2"},
		ResticCode: 11, Error: "the repository is locked", Log: "load indexes\n",
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	result, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb takes the answer as a map: %v", err)
	}
	back, err := result.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got CheckResult
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatalf("the panel reads the answer: %v", err)
	}
	if !reflect.DeepEqual(got, answer) {
		t.Errorf("the answer came back as %+v, want %+v", got, answer)
	}
	// the typed struct itself does not go: hence the map
	if _, err := structpb.NewValue(answer.Errors); err == nil {
		t.Error("structpb took a typed slice")
	}
}

func TestEveryFailureClassIsListed(t *testing.T) {
	want := []string{"locked", "lock_invalid", "repository", "password", "partial", "source", "restic", "interrupted", "timeout"}
	if !slices.Equal(Failures, want) {
		t.Errorf("failures = %v, want %v", Failures, want)
	}
}
