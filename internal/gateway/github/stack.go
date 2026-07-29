package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// stacksPerPage requests the largest page GitHub allows,
// so membership lookups across a repository take as few requests as possible.
const stacksPerPage = 100

// stackPageLimit bounds pagination so a server that keeps reporting full pages
// cannot loop forever.
// At the maximum page size this covers more stacks than a repository is
// expected to hold.
const stackPageLimit = 100

// Stack is a stack of pull requests in a repository.
//
// A stack is an ordered chain in which each pull request targets the head of
// the one below it.
// PullRequests is ordered bottom to top: the first entry is closest to the
// branch the whole stack targets.
type Stack struct {
	// ID is GitHub's global identifier for the stack.
	ID int

	// Number identifies the stack within its repository.
	// It is the value shown in GitHub's UI,
	// and the value that addresses the stack in this package's operations.
	Number int

	// BaseRef is the branch the whole stack targets.
	BaseRef string

	// Open reports whether any pull request in the stack is still open.
	Open bool

	// PullRequests are the stack's members, ordered bottom to top.
	PullRequests []StackPullRequest
}

// UnmarshalJSON decodes GitHub's stack representation,
// flattening its nested base object so callers work with a flat domain type.
func (s *Stack) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID     int  `json:"id"`
		Number int  `json:"number"`
		Open   bool `json:"open"`
		Base   struct {
			Ref string `json:"ref"`
		} `json:"base"`
		PullRequests []StackPullRequest `json:"pull_requests"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("unmarshal stack: %w", err)
	}

	*s = Stack{
		ID:           wire.ID,
		Number:       wire.Number,
		BaseRef:      wire.Base.Ref,
		Open:         wire.Open,
		PullRequests: wire.PullRequests,
	}
	return nil
}

// PRNumbers reports the stack's pull request numbers, bottom to top.
func (s *Stack) PRNumbers() []int {
	var numbers []int
	for _, pr := range s.PullRequests {
		numbers = append(numbers, pr.Number)
	}
	return numbers
}

// SplitLeadingMerged divides the stack into its leading run of merged pull
// requests and the active remainder, each as pull request numbers.
//
// GitHub keeps merged pull requests in a stack, so a stack whose lower changes
// have landed reports members that a caller tracking only unlanded work does
// not know about.
// Comparing such a stack against a desired list requires excluding the merged
// prefix first.
//
// ok is false when merged entries are not a contiguous leading run, which
// means the stack is not shaped like a partially landed chain.
// Callers should not attempt to reconcile such a stack.
func (s *Stack) SplitLeadingMerged() (mergedPrefix, active []int, ok bool) {
	for _, pr := range s.PullRequests {
		if !pr.Merged() {
			active = append(active, pr.Number)
			continue
		}

		// A merged pull request above an unmerged one cannot have arisen from
		// landing a chain from the bottom up.
		if len(active) > 0 {
			return nil, nil, false
		}
		mergedPrefix = append(mergedPrefix, pr.Number)
	}
	return mergedPrefix, active, true
}

// StackPullRequest is one pull request's membership in a stack.
type StackPullRequest struct {
	// Number is the pull request number.
	Number int

	// State is "open" or "closed".
	// A merged pull request is also reported as closed.
	State string

	// MergedAt is when the pull request landed, or nil if it has not.
	MergedAt *time.Time

	// HeadRef and HeadSHA identify the pull request's head branch.
	// They report the chain GitHub validated, which is what explains a
	// rejected stack update after a head branch is deleted.
	HeadRef string
	HeadSHA string
}

// UnmarshalJSON decodes GitHub's stack member representation,
// flattening its nested head object.
func (p *StackPullRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		MergedAt *time.Time `json:"merged_at"`
		Head     struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("unmarshal stack pull request: %w", err)
	}

	*p = StackPullRequest{
		Number:   wire.Number,
		State:    wire.State,
		MergedAt: wire.MergedAt,
		HeadRef:  wire.Head.Ref,
		HeadSHA:  wire.Head.SHA,
	}
	return nil
}

// Merged reports whether the pull request has landed.
func (p StackPullRequest) Merged() bool { return p.MergedAt != nil }

// Abandoned reports whether the pull request was closed without landing.
//
// Such a member may no longer have a head branch, so a chain that includes it
// is not necessarily reconstructible.
func (p StackPullRequest) Abandoned() bool { return p.State == "closed" && !p.Merged() }

// ListStacks reports every stack in the repository, newest stack number first.
//
// It follows pagination to completion so a caller can index pull request
// membership for a whole repository from one operation.
func (c *Gateway) ListStacks(ctx context.Context, owner, repo string) ([]*Stack, error) {
	var stacks []*Stack
	for page := 1; page <= stackPageLimit; page++ {
		query := url.Values{
			"per_page": {strconv.Itoa(stacksPerPage)},
			"page":     {strconv.Itoa(page)},
		}

		var pageStacks []*Stack
		if _, err := c.restRequest(
			ctx, http.MethodGet, stacksPath(owner, repo), query, nil, &pageStacks,
		); err != nil {
			return nil, fmt.Errorf("list stacks: %w", err)
		}

		stacks = append(stacks, pageStacks...)
		if len(pageStacks) < stacksPerPage {
			return stacks, nil
		}
	}
	return stacks, nil
}

// FindStackForPullRequest reports the stack containing the given pull request.
//
// It reports a nil stack and no error when the pull request is not in a stack.
func (c *Gateway) FindStackForPullRequest(
	ctx context.Context, owner, repo string, pr int,
) (*Stack, error) {
	query := url.Values{"pull_request": {strconv.Itoa(pr)}}

	var stacks []*Stack
	if _, err := c.restRequest(
		ctx, http.MethodGet, stacksPath(owner, repo), query, nil, &stacks,
	); err != nil {
		return nil, fmt.Errorf("find stack for #%d: %w", pr, err)
	}
	if len(stacks) == 0 {
		return nil, nil
	}
	return stacks[0], nil
}

// GetStack reports the stack with the given repository-scoped number.
func (c *Gateway) GetStack(
	ctx context.Context, owner, repo string, number int,
) (*Stack, error) {
	path := fmt.Sprintf("%s/%d", stacksPath(owner, repo), number)

	var stack Stack
	if _, err := c.restRequest(ctx, http.MethodGet, path, nil, nil, &stack); err != nil {
		return nil, fmt.Errorf("get stack %d: %w", number, err)
	}
	return &stack, nil
}

// stackMembers is the request body for the operations that take an ordered
// list of pull request numbers.
type stackMembers struct {
	PullRequests []int `json:"pull_requests"`
}

// CreateStack creates a stack from pull request numbers ordered bottom to top.
//
// GitHub accepts from 2 through 100 pull requests, and requires that each
// pull request's base ref match the previous one's head ref.
func (c *Gateway) CreateStack(
	ctx context.Context, owner, repo string, prs []int,
) (*Stack, error) {
	var stack Stack
	body := stackMembers{PullRequests: prs}
	if _, err := c.restRequest(
		ctx, http.MethodPost, stacksPath(owner, repo), nil, body, &stack,
	); err != nil {
		return nil, fmt.Errorf("create stack: %w", err)
	}
	return &stack, nil
}

// AddToStack appends pull requests to the top of an existing stack.
//
// prs is only the delta to append, ordered from the stack's current top
// upward, and its first entry's base ref must match the current top's head
// ref.
// GitHub accepts from 1 through 100 pull requests.
func (c *Gateway) AddToStack(
	ctx context.Context, owner, repo string, number int, prs []int,
) (*Stack, error) {
	path := fmt.Sprintf("%s/%d/add", stacksPath(owner, repo), number)

	var stack Stack
	body := stackMembers{PullRequests: prs}
	if _, err := c.restRequest(ctx, http.MethodPost, path, nil, body, &stack); err != nil {
		return nil, fmt.Errorf("add to stack %d: %w", number, err)
	}
	return &stack, nil
}

// UnstackStack removes a stack's unmerged pull requests.
//
// GitHub leaves behind every pull request it cannot unstack, which is those
// merged, merging, or queued for merge.
// dissolved reports that none remained and the stack no longer exists,
// in which case remaining is nil.
// A caller that needs the stack gone entirely must check dissolved: a stack
// with surviving members has been reduced, not removed.
func (c *Gateway) UnstackStack(
	ctx context.Context, owner, repo string, number int,
) (remaining *Stack, dissolved bool, err error) {
	path := fmt.Sprintf("%s/%d/unstack", stacksPath(owner, repo), number)

	var stack Stack
	status, err := c.restRequest(ctx, http.MethodPost, path, nil, nil, &stack)
	if err != nil {
		return nil, false, fmt.Errorf("unstack %d: %w", number, err)
	}
	if status == http.StatusNoContent {
		return nil, true, nil
	}
	return &stack, false, nil
}

// stacksPath builds the repository-scoped path that every stack operation
// extends.
func stacksPath(owner, repo string) string {
	return fmt.Sprintf("repos/%s/%s/stacks",
		url.PathEscape(owner), url.PathEscape(repo))
}
