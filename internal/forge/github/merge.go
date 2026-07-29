package github

import (
	"context"
	"fmt"

	"go.abhg.dev/gs/internal/forge"
	"go.abhg.dev/gs/internal/gateway/github"
)

// MergeChange merges an open pull request into its base branch.
//
// TODO(native-stacks): merge-async required; see D6 in
// plans/github-native-stacks.md.
//
// This uses GitHub's synchronous merge mutation, which GitHub rejects for a
// pull request that belongs to a native stack (see [Repository.SyncStack]).
// Such a pull request must be merged through GitHub's asynchronous merge API,
// which also merges everything below it in the stack, so supporting it is more
// than swapping the endpoint.
func (r *Repository) MergeChange(
	ctx context.Context, fid forge.ChangeID,
	opts forge.MergeChangeOptions,
) error {
	id := mustPR(fid)

	gqlID, err := r.graphQLID(ctx, id)
	if err != nil {
		return fmt.Errorf("resolve PR ID: %w", err)
	}

	return r.mergePullRequest(ctx, id, gqlID, opts)
}

func (r *Repository) mergePullRequest(
	ctx context.Context, id *PR, gqlID github.ID,
	opts forge.MergeChangeOptions,
) error {
	input := github.MergePullRequestInput{
		PullRequestID: gqlID,
	}
	if opts.HeadHash != "" {
		input.ExpectedHeadOID = new(opts.HeadHash.String())
	}
	switch opts.Method {
	case forge.MergeMethodDefault:
	case forge.MergeMethodMerge:
		input.MergeMethod = new(github.MergeMethodMerge)
	case forge.MergeMethodSquash:
		input.MergeMethod = new(github.MergeMethodSquash)
	case forge.MergeMethodRebase:
		input.MergeMethod = new(github.MergeMethodRebase)
	default:
		r.log.Warn(
			"Unsupported merge method; using forge default",
			"method", opts.Method,
		)
	}
	if err := r.gateway.MergePullRequest(ctx, &input); err != nil {
		return fmt.Errorf("merge pull request: %w", err)
	}

	r.log.Debug("Merged pull request", "pr", id.Number)
	return nil
}
