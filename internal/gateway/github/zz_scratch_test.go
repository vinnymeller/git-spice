package github

import (
	"encoding/json"
	"testing"
)

// Documented example payload from gh-stack's REST API reference.
const scratchListPayload = `[
  {
    "id": 9876543,
    "number": 42,
    "node_id": "S_kwDOABCDEF4AAAAA",
    "url": "https://api.github.com/repos/octocat/hello-world/stacks/42",
    "base": { "ref": "main" },
    "open": true,
    "created_at": "2026-04-15T10:00:00Z",
    "pull_requests": [
      {
        "number": 101,
        "state": "closed",
        "draft": false,
        "merged_at": "2026-04-16T10:00:00Z",
        "head": { "ref": "user-model", "sha": "aaa1111" }
      },
      {
        "number": 102,
        "state": "open",
        "draft": false,
        "merged_at": null,
        "head": { "ref": "user-api", "sha": "bbb2222" }
      },
      {
        "number": 103,
        "state": "closed",
        "draft": false,
        "merged_at": null,
        "head": { "ref": "user-ui", "sha": "ccc3333" }
      }
    ]
  }
]`

func TestScratchDecode(t *testing.T) {
	var stacks []*Stack
	if err := json.Unmarshal([]byte(scratchListPayload), &stacks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(stacks) != 1 {
		t.Fatalf("got %d stacks", len(stacks))
	}

	s := stacks[0]
	t.Logf("ID=%d Number=%d BaseRef=%q Open=%v", s.ID, s.Number, s.BaseRef, s.Open)
	if s.ID != 9876543 || s.Number != 42 || s.BaseRef != "main" || !s.Open {
		t.Errorf("stack header decoded wrong: %+v", s)
	}

	nums := s.PRNumbers()
	t.Logf("PRNumbers=%v", nums)
	if len(nums) != 3 || nums[0] != 101 || nums[2] != 103 {
		t.Errorf("PRNumbers wrong: %v", nums)
	}

	for _, pr := range s.PullRequests {
		t.Logf("#%d state=%q merged=%v abandoned=%v headRef=%q headSHA=%q",
			pr.Number, pr.State, pr.Merged(), pr.Abandoned(), pr.HeadRef, pr.HeadSHA)
	}
	if !s.PullRequests[0].Merged() {
		t.Error("#101 should be merged")
	}
	if s.PullRequests[1].Merged() || s.PullRequests[1].Abandoned() {
		t.Error("#102 should be open and not abandoned")
	}
	if !s.PullRequests[2].Abandoned() {
		t.Error("#103 is closed without merging, so abandoned")
	}
	if s.PullRequests[0].HeadRef != "user-model" || s.PullRequests[0].HeadSHA != "aaa1111" {
		t.Errorf("head not flattened: %+v", s.PullRequests[0])
	}

	merged, active, ok := s.SplitLeadingMerged()
	t.Logf("SplitLeadingMerged merged=%v active=%v ok=%v", merged, active, ok)
	if !ok {
		t.Fatal("expected a well-shaped partially landed stack")
	}
	if len(merged) != 1 || merged[0] != 101 {
		t.Errorf("merged prefix wrong: %v", merged)
	}
	if len(active) != 2 || active[0] != 102 || active[1] != 103 {
		t.Errorf("active tail wrong: %v", active)
	}
}

func TestScratchSplitInterleaved(t *testing.T) {
	now := "2026-04-16T10:00:00Z"
	payload := `{"id":1,"number":1,"base":{"ref":"main"},"open":true,"pull_requests":[
		{"number":1,"state":"open","merged_at":null,"head":{"ref":"a","sha":"1"}},
		{"number":2,"state":"closed","merged_at":"` + now + `","head":{"ref":"b","sha":"2"}}
	]}`

	var s Stack
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	merged, active, ok := s.SplitLeadingMerged()
	t.Logf("interleaved: merged=%v active=%v ok=%v", merged, active, ok)
	if ok {
		t.Error("a merged PR above an unmerged one must not be reconcilable")
	}
}

func TestScratchSplitAllActive(t *testing.T) {
	payload := `{"id":1,"number":7,"base":{"ref":"main"},"open":true,"pull_requests":[
		{"number":1,"state":"open","merged_at":null,"head":{"ref":"a","sha":"1"}},
		{"number":2,"state":"open","merged_at":null,"head":{"ref":"b","sha":"2"}}
	]}`

	var s Stack
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	merged, active, ok := s.SplitLeadingMerged()
	t.Logf("all active: merged=%v active=%v ok=%v", merged, active, ok)
	if !ok || merged != nil || len(active) != 2 {
		t.Errorf("fresh stack: merged=%v active=%v ok=%v", merged, active, ok)
	}
}
