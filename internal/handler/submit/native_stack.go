package submit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.abhg.dev/gs/internal/forge"
	"go.abhg.dev/gs/internal/silog"
	"go.abhg.dev/gs/internal/spice"
	"go.abhg.dev/gs/internal/spice/state"
)

// syncNativeStack registers the stacks containing the submitted branches with
// the forge, for forges that model a stack as a first-class object.
//
// This is best effort in the same way navigation comments are: the changes have
// already been pushed and published by the time it runs, so a failure here is
// reported and never returned.
func syncNativeStack(
	ctx context.Context,
	svc Service,
	log *silog.Logger,
	enabled bool,
	submittedBranches []string,
	getRemote func(context.Context) (state.Remote, error),
	getRemoteRepo func(context.Context) (forge.Repository, error),
) {
	if !enabled || len(submittedBranches) == 0 {
		return
	}

	remote, err := getRemote(ctx)
	if err != nil {
		log.Warn("Could not register the stack with the forge", "error", err)
		return
	}

	// Everything from here reports at info when it cannot proceed:
	// reaching this function at all means the user asked for native stacks,
	// and staying silent would leave them with no way to tell the difference
	// between "it worked" and "it never ran".
	if remote.ForkMode() {
		log.Infof("Not registering a stack on the forge: " +
			"changes are pushed to a fork, which cannot hold a stack")
		return
	}

	remoteRepo, err := getRemoteRepo(ctx)
	if err != nil {
		log.Warn("Could not register the stack with the forge", "error", err)
		return
	}

	stacker, ok := remoteRepo.(forge.WithStacks)
	if !ok {
		log.Infof("Not registering a stack on the forge: %v does not have stacks",
			forge.GetDisplayName(remoteRepo.Forge()))
		return
	}

	graph, err := svc.BranchGraph(ctx, nil)
	if err != nil {
		log.Warn("Could not register the stack with the forge", "error", err)
		return
	}

	changesByBranch, err := submittedChanges(ctx, svc)
	if err != nil {
		log.Warn("Could not register the stack with the forge", "error", err)
		return
	}

	for _, branches := range nativeStackCandidates(graph, log, submittedBranches) {
		ids, ok := changeIDsFor(branches, changesByBranch, log)
		if !ok {
			continue
		}

		res, err := stacker.SyncStack(ctx, ids)
		switch {
		case errors.Is(err, forge.ErrStacksUnsupported):
			log.Infof("Not registering a stack on the forge: " +
				"stacks are not enabled for this repository")
			return
		case err != nil:
			log.Warn("Could not register the stack with the forge", "error", err)
			continue
		}

		reportStackSync(log, res)
	}
}

// nativeStackCandidates resolves the submitted branches into the chains to
// register on the forge, each ordered bottom to top.
//
// A forge stack is linear, but a git-spice stack is a tree: a branch may have
// several branches above it. A tree cannot be represented faithfully, because a
// change belongs to at most one stack on the forge, so two chains cannot share
// the changes below a fork. One chain is registered and the rest are reported.
func nativeStackCandidates(
	graph *spice.BranchGraph,
	log *silog.Logger,
	submittedBranches []string,
) [][]string {
	var (
		candidates [][]string
		bottoms    []string // stacks already resolved
	)
	for _, branch := range submittedBranches {
		// Every branch of one stack resolves to the same chain, so resolve each
		// stack once instead of once per submitted branch.
		bottom := graph.Bottom(branch)
		if bottom == "" || slices.Contains(bottoms, bottom) {
			continue
		}
		bottoms = append(bottoms, bottom)

		chain := bestChain(graph, bottom, submittedBranches)
		if len(chain) == 0 {
			continue
		}

		if left := submittedOutside(chain, submittedBranches, graph, bottom); len(left) > 0 {
			log.Infof("Not including %v in the stack on the forge: "+
				"a stack on the forge is linear, and they are on a separate branch of this stack",
				strings.Join(left, ", "))
		}
		candidates = append(candidates, chain)
	}
	return candidates
}

// chainScore ranks one candidate chain against the submitted branches.
type chainScore struct {
	// chain is the branches from the bottom of the stack to a topmost branch,
	// which is always linear.
	chain []string

	// submitted is how many of the submitted branches the chain covers.
	submitted int

	// tip is the topmost branch, used to break ties so the choice is stable
	// across runs rather than dependent on traversal order.
	tip string
}

// betterThan reports whether s represents the submitted branches better than o.
//
// Covering more of what the user submitted wins first, so submitting one path
// of a tree registers that path. Length breaks the tie when the whole tree was
// submitted, which registers as much of the stack as a linear stack can hold.
func (s chainScore) betterThan(o chainScore) bool {
	if s.submitted != o.submitted {
		return s.submitted > o.submitted
	}
	if len(s.chain) != len(o.chain) {
		return len(s.chain) > len(o.chain)
	}
	return s.tip < o.tip
}

// bestChain reports the chain from the bottom of a stack to one of its topmost
// branches that best represents the submitted branches.
func bestChain(
	graph *spice.BranchGraph, bottom string, submittedBranches []string,
) []string {
	var best chainScore
	for tip := range graph.Tops(bottom) {
		chain := slices.Collect(graph.Downstack(tip))
		slices.Reverse(chain)

		score := chainScore{chain: chain, tip: tip}
		for _, branch := range chain {
			if slices.Contains(submittedBranches, branch) {
				score.submitted++
			}
		}

		if best.chain == nil || score.betterThan(best) {
			best = score
		}
	}
	return best.chain
}

// submittedOutside reports the submitted branches in this stack that the chosen
// chain does not cover.
//
// Only submitted branches are reported: a branch the user did not submit was
// never going to be registered, so naming it would be noise.
func submittedOutside(
	chain, submittedBranches []string,
	graph *spice.BranchGraph, bottom string,
) []string {
	var left []string
	for branch := range graph.Upstack(bottom) {
		if slices.Contains(chain, branch) {
			continue
		}
		if slices.Contains(submittedBranches, branch) {
			left = append(left, branch)
		}
	}
	return left
}

// submittedChanges maps each tracked branch to the change it was published as.
func submittedChanges(ctx context.Context, svc Service) (map[string]forge.ChangeID, error) {
	branches, err := svc.LoadBranches(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tracked branches: %w", err)
	}

	changes := make(map[string]forge.ChangeID, len(branches))
	for _, b := range branches {
		if b.Change != nil {
			changes[b.Name] = b.Change.ChangeID()
		}
	}
	return changes, nil
}

// changeIDsFor resolves a stack's branches to their published changes.
//
// ok is false when a branch in the middle of the stack has no change, because
// the forge validates that each change targets the one below it, and a stack
// with a hole in it does not describe a chain.
func changeIDsFor(
	branches []string,
	changesByBranch map[string]forge.ChangeID,
	log *silog.Logger,
) (ids []forge.ChangeID, ok bool) {
	for _, branch := range branches {
		change, published := changesByBranch[branch]
		if !published {
			if len(ids) > 0 {
				log.Infof("Not registering a stack on the forge: "+
					"%v has not been submitted", branch)
				return nil, false
			}
			// Branches below the submitted range have not been published yet;
			// the stack simply starts higher up.
			continue
		}
		ids = append(ids, change)
	}

	if len(ids) == 0 {
		log.Infof("Not registering a stack on the forge: " +
			"none of these branches have been submitted")
		return nil, false
	}

	// A single change is still worth reporting: if the forge holds a stack for
	// it, the requested state is that the stack no longer exists.
	return ids, true
}

// reportStackSync tells the user what happened to the forge's stack.
func reportStackSync(log *silog.Logger, res forge.SyncStackResult) {
	switch res.Action {
	case forge.StackSyncCreated:
		log.Infof("Created stack %d on the forge", res.Number)
	case forge.StackSyncExtended:
		log.Infof("Updated stack %d on the forge", res.Number)
	case forge.StackSyncRecreated:
		log.Infof("Recreated stack %d on the forge to match this stack", res.Number)
	case forge.StackSyncDissolved:
		log.Infof("Removed the stack on the forge: it no longer has multiple changes")
	case forge.StackSyncDiverged:
		log.Infof("Did not update the stack on the forge: %v", res.Detail)
	case forge.StackSyncPartiallyDissolved:
		log.Warnf("The stack on the forge is incomplete: %v", res.Detail)
	case forge.StackSyncUnchanged:
		log.Debug("Stack on the forge is already up to date", "stack", res.Number)
	}
}
