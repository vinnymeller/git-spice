package github

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.abhg.dev/gs/internal/forge"
	"go.abhg.dev/gs/internal/gateway/github"
)

// TODO(native-stacks): merge-async required; see D6 in
// plans/github-native-stacks.md.
//
// Registering a stack makes every pull request in it unmergeable through
// [Repository.MergeChange]: GitHub rejects stacked pull requests on the legacy
// synchronous merge mutation this repository uses, and requires its
// asynchronous merge API instead.
// Turning the feature off does not dissolve a stack that already exists, so the
// pull requests stay unmergeable until the stack is removed.
// This is a known, accepted limitation of the initial implementation, and must
// be resolved before native stacks are enabled by default.

const (
	// minStackSize is the smallest stack GitHub will create.
	// One change on its own is not a stack.
	minStackSize = 2

	// maxStackBatch is the most pull requests GitHub accepts in a single
	// create or add request.
	maxStackBatch = 100
)

var _ forge.WithStacks = (*Repository)(nil)

// SyncStack reconciles GitHub's stack for these pull requests with ids,
// ordered bottom to top.
//
// GitHub's stack API is additive: it can append to the top of a stack, but it
// cannot reorder, drop, or insert. Reaching any other shape requires
// dissolving the stack and creating it again, which SyncStack will do, because
// a caller asking to sync has declared ids authoritative.
//
// SyncStack converges GitHub to ids or reports why it did not.
// It refuses to mutate anything when converging would require destroying a
// stack that holds pull requests outside ids.
func (r *Repository) SyncStack(
	ctx context.Context, ids []forge.ChangeID,
) (forge.SyncStackResult, error) {
	desired := make([]int, len(ids))
	for i, id := range ids {
		desired[i] = mustPR(id).Number
	}

	if len(desired) == 0 {
		return forge.SyncStackResult{Action: forge.StackSyncUnchanged}, nil
	}

	// Resolve membership for every desired pull request, not just the bottom
	// one. A stack found from the bottom alone can be destroyed by a recreate
	// that then fails, because an upper pull request belongs to a different
	// stack and cannot be claimed.
	remote, err := r.findStack(ctx, desired)
	if err != nil {
		return forge.SyncStackResult{}, err
	}
	if remote != nil && remote.conflict {
		return forge.SyncStackResult{
			Action: forge.StackSyncDiverged,
			Detail: remote.detail,
		}, nil
	}

	if len(desired) < minStackSize {
		// Not enough changes to be a stack. If GitHub still records one, the
		// requested state is "no stack", which is expressible even though
		// creating a one-change stack is not.
		if remote == nil {
			return forge.SyncStackResult{Action: forge.StackSyncUnchanged}, nil
		}
		return r.dissolveStack(ctx, remote.stack)
	}

	if remote == nil {
		return r.createStack(ctx, desired)
	}

	// GitHub keeps merged pull requests in a stack; git-spice drops merged
	// branches from its local stack. Compare only the part of GitHub's stack
	// that is still in flight.
	mergedPrefix, active, ok := remote.stack.SplitLeadingMerged()
	if !ok {
		return forge.SyncStackResult{
			Action: forge.StackSyncDiverged,
			Number: remote.stack.Number,
			Detail: "stack has merged changes above unmerged ones",
		}, nil
	}

	if slices.Equal(active, desired) {
		return forge.SyncStackResult{
			Action: forge.StackSyncUnchanged,
			Number: remote.stack.Number,
		}, nil
	}

	// Appending preserves the stack's identity, and with it any links to the
	// stack, so prefer it whenever the request is a pure extension.
	if delta, ok := appendDelta(active, desired); ok {
		res, err := r.extendStack(ctx, remote.stack, delta)
		if err == nil {
			return res, nil
		}

		switch {
		// The stack was removed between reading it and appending to it, so
		// there is nothing left to reshape.
		case errors.Is(err, github.ErrNotFound):
			r.log.Debug("Stack no longer exists; creating a new one",
				"stack", remote.stack.Number)
			return r.createStack(ctx, desired)

		// A stack whose merged changes have had their branches deleted no
		// longer validates as a chain, so GitHub rejects the append.
		// Recreating is the only way to reach the requested shape.
		case errors.Is(err, github.ErrUnprocessable):
			r.log.Debug("Could not extend stack; recreating",
				"stack", remote.stack.Number, "error", err)

		default:
			return forge.SyncStackResult{}, err
		}
	}

	return r.recreateStack(ctx, remote.stack, mergedPrefix, active, desired)
}

// stackMembership is the result of resolving which GitHub stack, if any, owns
// the requested pull requests.
type stackMembership struct {
	// stack is the single stack that holds some or all of the pull requests,
	// or nil when none do.
	stack *github.Stack

	// conflict reports that more than one stack is implicated, which no
	// unilateral action can resolve.
	conflict bool

	// detail explains a conflict.
	detail string
}

// findStack resolves the GitHub stack that owns any of the given pull requests.
//
// It reports ErrStacksUnsupported when the repository has no stacks API, which
// is how GitHub reports a repository without the feature enabled.
//
// Listing every stack once costs less than a filtered lookup per pull request,
// and is what makes it possible to see a conflicting membership before
// mutating anything.
func (r *Repository) findStack(
	ctx context.Context, desired []int,
) (*stackMembership, error) {
	stacks, err := r.gateway.ListStacks(ctx, r.owner, r.repo)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			return nil, forge.ErrStacksUnsupported
		}
		return nil, err
	}

	byPR := make(map[int]*github.Stack)
	for _, stack := range stacks {
		for _, pr := range stack.PullRequests {
			byPR[pr.Number] = stack
		}
	}

	var found []*github.Stack
	for _, pr := range desired {
		stack, ok := byPR[pr]
		if !ok {
			continue
		}
		if !slices.ContainsFunc(found, func(s *github.Stack) bool {
			return s.Number == stack.Number
		}) {
			found = append(found, stack)
		}
	}

	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &stackMembership{stack: found[0]}, nil
	default:
		numbers := make([]int, len(found))
		for i, stack := range found {
			numbers[i] = stack.Number
		}
		return &stackMembership{
			conflict: true,
			detail: fmt.Sprintf(
				"these changes span %d stacks on GitHub (%v); resolve it there first",
				len(found), numbers),
		}, nil
	}
}

// createStack creates a stack for desired, batching to GitHub's per-request
// limit.
func (r *Repository) createStack(
	ctx context.Context, desired []int,
) (forge.SyncStackResult, error) {
	head, tail := splitBatch(desired)

	stack, err := r.gateway.CreateStack(ctx, r.owner, r.repo, head)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			return forge.SyncStackResult{}, forge.ErrStacksUnsupported
		}
		if errors.Is(err, github.ErrUnprocessable) {
			return r.classifyCreateRejection(ctx, desired, err)
		}
		return forge.SyncStackResult{}, err
	}

	if err := r.appendBatches(ctx, stack.Number, tail); err != nil {
		return forge.SyncStackResult{
			Action: forge.StackSyncCreated,
			Number: stack.Number,
			Detail: fmt.Sprintf("only the first %d changes were stacked: %v", len(head), err),
		}, nil
	}

	return forge.SyncStackResult{
		Action: forge.StackSyncCreated,
		Number: stack.Number,
	}, nil
}

// classifyCreateRejection explains a rejected create by re-reading membership
// rather than by interpreting GitHub's message text, whose phrasing is not a
// stable contract.
//
// The common benign case is a stack that already holds exactly these changes,
// which makes the create redundant rather than wrong.
func (r *Repository) classifyCreateRejection(
	ctx context.Context, desired []int, cause error,
) (forge.SyncStackResult, error) {
	remote, err := r.findStack(ctx, desired)
	if err != nil {
		return forge.SyncStackResult{}, err
	}
	if remote != nil && !remote.conflict {
		if _, active, ok := remote.stack.SplitLeadingMerged(); ok && slices.Equal(active, desired) {
			return forge.SyncStackResult{
				Action: forge.StackSyncUnchanged,
				Number: remote.stack.Number,
			}, nil
		}
	}

	return forge.SyncStackResult{
		Action: forge.StackSyncDiverged,
		Detail: cause.Error(),
	}, nil
}

// extendStack appends delta to the top of an existing stack.
func (r *Repository) extendStack(
	ctx context.Context, stack *github.Stack, delta []int,
) (forge.SyncStackResult, error) {
	if err := r.appendBatches(ctx, stack.Number, delta); err != nil {
		return forge.SyncStackResult{}, err
	}
	return forge.SyncStackResult{
		Action: forge.StackSyncExtended,
		Number: stack.Number,
	}, nil
}

// recreateStack dissolves the stack and creates it again in the requested
// order.
//
// mergedPrefix and active describe the stack as it was read: GitHub retains
// merged changes through an unstack, so only active can be removed.
func (r *Repository) recreateStack(
	ctx context.Context,
	stack *github.Stack,
	mergedPrefix, active, desired []int,
) (forge.SyncStackResult, error) {
	// A stack holding only landed changes is history, not something to reshape.
	// The requested changes belong in a new stack rooted at their own base.
	if len(active) == 0 {
		r.log.Debug("Existing stack holds only merged changes; creating a new one",
			"stack", stack.Number)
		return r.createStack(ctx, desired)
	}

	if dropped := missingFrom(active, desired); len(dropped) > 0 {
		r.log.Warn("Removing changes from the stack on GitHub that are not in this stack locally",
			"stack", stack.Number, "changes", dropped)
	}

	// Confirm nothing moved between reading the stack and destroying it. This
	// does not close the race, but it stops a concurrent submit's work from
	// being silently discarded.
	current, err := r.gateway.GetStack(ctx, r.owner, r.repo, stack.Number)
	if err != nil {
		return forge.SyncStackResult{}, err
	}
	if !slices.Equal(current.PRNumbers(), stack.PRNumbers()) {
		return forge.SyncStackResult{
			Action: forge.StackSyncDiverged,
			Number: stack.Number,
			Detail: "the stack changed on GitHub while syncing; run submit again",
		}, nil
	}

	remaining, dissolved, err := r.gateway.UnstackStack(ctx, r.owner, r.repo, stack.Number)
	if err != nil {
		return forge.SyncStackResult{}, err
	}

	if dissolved {
		return r.createStack(ctx, desired)
	}

	// GitHub kept some changes. When those are exactly the merged ones, this is
	// the expected outcome for a partially landed stack: what remains is
	// immutable history, and the requested changes form a new stack.
	survivors := remaining.PRNumbers()
	if slices.Equal(survivors, mergedPrefix) {
		return r.createStack(ctx, desired)
	}

	return r.recoverPartialUnstack(ctx, stack, survivors, desired)
}

// recoverPartialUnstack salvages a stack that GitHub would not fully dissolve
// because a change was merging or queued for merge.
//
// Those states are not visible in the stack resource, so this case can only be
// discovered after the unstack has already removed what it could. Recovery is
// best effort: prefer the requested shape, fall back to the previous one, and
// otherwise report honestly that GitHub is in neither.
func (r *Repository) recoverPartialUnstack(
	ctx context.Context,
	stack *github.Stack,
	survivors, desired []int,
) (forge.SyncStackResult, error) {
	if delta, ok := appendDelta(survivors, desired); ok {
		if err := r.appendBatches(ctx, stack.Number, delta); err == nil {
			return forge.SyncStackResult{
				Action: forge.StackSyncRecreated,
				Number: stack.Number,
			}, nil
		}
	}

	if delta, ok := appendDelta(survivors, stack.PRNumbers()); ok {
		if err := r.appendBatches(ctx, stack.Number, delta); err == nil {
			return forge.SyncStackResult{
				Action: forge.StackSyncDiverged,
				Number: stack.Number,
				Detail: "GitHub would not unstack a change that is merging or queued, so the stack was restored unchanged",
			}, nil
		}
	}

	return forge.SyncStackResult{
		Action: forge.StackSyncPartiallyDissolved,
		Number: stack.Number,
		Detail: fmt.Sprintf(
			"GitHub kept changes %v because they are merging or queued for merge, and the stack could not be rebuilt",
			survivors),
	}, nil
}

// dissolveStack removes the stack from GitHub.
func (r *Repository) dissolveStack(
	ctx context.Context, stack *github.Stack,
) (forge.SyncStackResult, error) {
	remaining, dissolved, err := r.gateway.UnstackStack(ctx, r.owner, r.repo, stack.Number)
	if err != nil {
		return forge.SyncStackResult{}, err
	}
	if dissolved {
		return forge.SyncStackResult{Action: forge.StackSyncDissolved}, nil
	}

	return forge.SyncStackResult{
		Action: forge.StackSyncDiverged,
		Number: stack.Number,
		Detail: fmt.Sprintf(
			"GitHub kept changes %v in the stack because they are merged, merging, or queued for merge",
			remaining.PRNumbers()),
	}, nil
}

// appendBatches appends prs to a stack, respecting GitHub's per-request limit.
func (r *Repository) appendBatches(ctx context.Context, number int, prs []int) error {
	for len(prs) > 0 {
		batch, rest := splitBatch(prs)
		if _, err := r.gateway.AddToStack(ctx, r.owner, r.repo, number, batch); err != nil {
			return err
		}
		prs = rest
	}
	return nil
}

// splitBatch divides prs into a batch GitHub will accept and the remainder.
func splitBatch(prs []int) (batch, rest []int) {
	if len(prs) <= maxStackBatch {
		return prs, nil
	}
	return prs[:maxStackBatch], prs[maxStackBatch:]
}

// appendDelta reports the changes that would extend current into desired.
//
// ok is false unless current is a prefix of desired, because appending is the
// only incremental change GitHub's stack API can express.
func appendDelta(current, desired []int) (delta []int, ok bool) {
	if len(current) > len(desired) {
		return nil, false
	}
	for i, pr := range current {
		if desired[i] != pr {
			return nil, false
		}
	}
	delta = desired[len(current):]
	return delta, len(delta) > 0
}

// missingFrom reports the entries of have that want does not contain,
// preserving the order in have.
func missingFrom(have, want []int) []int {
	var missing []int
	for _, pr := range have {
		if !slices.Contains(want, pr) {
			missing = append(missing, pr)
		}
	}
	return missing
}
