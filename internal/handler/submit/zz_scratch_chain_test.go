package submit

import (
	"context"
	"testing"

	"go.abhg.dev/gs/internal/silog/silogtest"
	"go.abhg.dev/gs/internal/spice"
)

// scratchLoader builds a BranchGraph from a name->base map.
type scratchLoader struct {
	trunk string
	bases map[string]string
}

func (l scratchLoader) Trunk() string { return l.trunk }

func (l scratchLoader) LoadBranches(context.Context) ([]spice.LoadBranchItem, error) {
	var items []spice.LoadBranchItem
	for name, base := range l.bases {
		items = append(items, spice.LoadBranchItem{Name: name, Base: base})
	}
	return items, nil
}

func (scratchLoader) LookupWorktrees(context.Context, []string) (map[string]string, error) {
	return nil, nil
}

// The real tree that exposed the bug, from `gs ls -a`:
//
//	master
//	└── household-contact-entity-models        (#10460)
//	    └── make-agent-typing-fix              (#10517)
//	        └── agent-id-minimizer-validation  (#10518)
//	            ├── household-contact-entity-extraction   (#10461)
//	            │   └── add-household-contact-entity-tasks (#10462)
//	            │       └── household-contact-entity-api   (#10519)
//	            └── party-entity-extraction-refactor       (#10522)
func scratchGraph(t *testing.T) *spice.BranchGraph {
	t.Helper()
	loader := scratchLoader{
		trunk: "master",
		bases: map[string]string{
			"household-contact-entity-models":     "master",
			"make-agent-typing-fix":               "household-contact-entity-models",
			"agent-id-minimizer-validation":       "make-agent-typing-fix",
			"household-contact-entity-extraction": "agent-id-minimizer-validation",
			"add-household-contact-entity-tasks":  "household-contact-entity-extraction",
			"household-contact-entity-api":        "add-household-contact-entity-tasks",
			"party-entity-extraction-refactor":    "agent-id-minimizer-validation",
		},
	}
	graph, err := spice.NewBranchGraph(t.Context(), loader, nil)
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	return graph
}

var scratchAllBranches = []string{
	"household-contact-entity-models",
	"make-agent-typing-fix",
	"agent-id-minimizer-validation",
	"household-contact-entity-extraction",
	"add-household-contact-entity-tasks",
	"household-contact-entity-api",
	"party-entity-extraction-refactor",
}

// Whole tree submitted, as `gs ss` does: expect the 6-branch chain.
func TestScratchChain_wholeTree(t *testing.T) {
	graph := scratchGraph(t)
	got := nativeStackCandidates(graph, silogtest.New(t), scratchAllBranches)

	if len(got) != 1 {
		t.Fatalf("expected one chain, got %d: %v", len(got), got)
	}
	t.Logf("chain = %v", got[0])

	want := []string{
		"household-contact-entity-models",
		"make-agent-typing-fix",
		"agent-id-minimizer-validation",
		"household-contact-entity-extraction",
		"add-household-contact-entity-tasks",
		"household-contact-entity-api",
	}
	if len(got[0]) != len(want) {
		t.Fatalf("chain length %d, want %d: %v", len(got[0]), len(want), got[0])
	}
	for i := range want {
		if got[0][i] != want[i] {
			t.Errorf("chain[%d] = %q, want %q", i, got[0][i], want[i])
		}
	}

	left := submittedOutside(got[0], scratchAllBranches, graph, "household-contact-entity-models")
	t.Logf("left out = %v", left)
	if len(left) != 1 || left[0] != "party-entity-extraction-refactor" {
		t.Errorf("left out = %v, want [party-entity-extraction-refactor]", left)
	}
}

// Only the short path submitted, as `gs downstack submit` from the side tip
// does: the chain covering what was submitted must win over the longer one.
func TestScratchChain_submittedShortPath(t *testing.T) {
	graph := scratchGraph(t)
	submitted := []string{
		"household-contact-entity-models",
		"make-agent-typing-fix",
		"agent-id-minimizer-validation",
		"party-entity-extraction-refactor",
	}

	got := nativeStackCandidates(graph, silogtest.New(t), submitted)
	if len(got) != 1 {
		t.Fatalf("expected one chain, got %d: %v", len(got), got)
	}
	t.Logf("chain = %v", got[0])

	if last := got[0][len(got[0])-1]; last != "party-entity-extraction-refactor" {
		t.Errorf("chain tip = %q, want party-entity-extraction-refactor", last)
	}
	if len(got[0]) != 4 {
		t.Errorf("chain length = %d, want 4: %v", len(got[0]), got[0])
	}
}

// One tip submitted alone, as `gs branch submit` does.
func TestScratchChain_singleTip(t *testing.T) {
	graph := scratchGraph(t)
	got := nativeStackCandidates(graph, silogtest.New(t),
		[]string{"household-contact-entity-api"})

	if len(got) != 1 {
		t.Fatalf("expected one chain, got %d: %v", len(got), got)
	}
	t.Logf("chain = %v", got[0])
	if len(got[0]) != 6 {
		t.Errorf("chain length = %d, want 6: %v", len(got[0]), got[0])
	}
}

// A purely linear stack must be unaffected.
func TestScratchChain_linear(t *testing.T) {
	loader := scratchLoader{
		trunk: "main",
		bases: map[string]string{"a": "main", "b": "a", "c": "b"},
	}
	graph, err := spice.NewBranchGraph(t.Context(), loader, nil)
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	got := nativeStackCandidates(graph, silogtest.New(t), []string{"a", "b", "c"})
	if len(got) != 1 {
		t.Fatalf("expected one chain, got %d: %v", len(got), got)
	}
	t.Logf("chain = %v", got[0])
	if len(got[0]) != 3 || got[0][0] != "a" || got[0][2] != "c" {
		t.Errorf("chain = %v, want [a b c]", got[0])
	}
}
