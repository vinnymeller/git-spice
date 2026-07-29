package forge

import (
	"context"
	"errors"
	"fmt"
)

// ErrStacksUnsupported indicates that the forge does not model stacks as
// first-class objects, or that the repository does not have the feature
// enabled.
//
// Callers treat this as a benign no-op: every forge git-spice supports already
// expresses a stack through change base branches, and a forge-level stack
// object is additional, optional metadata.
var ErrStacksUnsupported = errors.New("forge does not support native stacks")

// WithStacks is an optional interface for forges that model a stack of changes
// as a first-class object, beyond the base-branch chaining every forge
// supports.
//
// Registering a stack records on the forge that an already-valid chain of
// changes is one unit. It changes no Git state.
type WithStacks interface {
	Repository

	// SyncStack reconciles the forge's stack containing these changes with
	// ids, which is ordered bottom to top: ids[0] is the change closest to the
	// branch the whole stack targets, and each subsequent change is based on
	// its predecessor.
	//
	// Implementations converge the forge to ids by whatever means the forge
	// supports. That may include dissolving and recreating the forge's stack
	// when the requested order cannot be reached incrementally, so callers
	// must only pass a list they intend to be authoritative.
	//
	// Returns ErrStacksUnsupported when the forge or repository does not have
	// the feature enabled.
	SyncStack(ctx context.Context, ids []ChangeID) (SyncStackResult, error)
}

// SyncStackResult reports what a [WithStacks.SyncStack] call did.
type SyncStackResult struct {
	// Action is what the operation did to the forge's stack.
	Action StackSyncAction

	// Number identifies the resulting stack within the repository,
	// or zero when no stack exists after the operation.
	Number int

	// Detail explains an outcome that did not converge.
	//
	// It carries the forge's own description for reporting, and is empty for
	// outcomes that need no explanation.
	Detail string
}

// StackSyncAction is what a stack sync did to the forge's stack.
type StackSyncAction int

const (
	// StackSyncUnchanged indicates that the forge already matched the
	// requested stack, or that there was nothing for the forge to express.
	StackSyncUnchanged StackSyncAction = iota

	// StackSyncCreated indicates that a new stack was created on the forge.
	StackSyncCreated

	// StackSyncExtended indicates that changes were appended to the top of an
	// existing stack, leaving its identity intact.
	StackSyncExtended

	// StackSyncRecreated indicates that the forge's stack was dissolved and
	// recreated because the requested order could not be reached
	// incrementally.
	//
	// The resulting stack has a new identity.
	StackSyncRecreated

	// StackSyncDissolved indicates that the forge's stack was removed because
	// the requested stack no longer has enough changes to be one.
	StackSyncDissolved

	// StackSyncDiverged indicates that the forge's stack does not match the
	// request and was deliberately left untouched.
	//
	// This is the outcome when converging would require destroying a stack
	// that the caller does not own, or when the forge cannot express the
	// requested shape yet.
	StackSyncDiverged

	// StackSyncPartiallyDissolved indicates that a recreate removed the
	// changes it could, but the forge retained changes that cannot be
	// unstacked, so the requested stack could not be created.
	//
	// This leaves the forge in a state that matches neither the previous
	// stack nor the request, and always warrants reporting to the user.
	StackSyncPartiallyDissolved
)

func (a StackSyncAction) String() string {
	switch a {
	case StackSyncUnchanged:
		return "unchanged"
	case StackSyncCreated:
		return "created"
	case StackSyncExtended:
		return "extended"
	case StackSyncRecreated:
		return "recreated"
	case StackSyncDissolved:
		return "dissolved"
	case StackSyncDiverged:
		return "diverged"
	case StackSyncPartiallyDissolved:
		return "partially dissolved"
	default:
		return fmt.Sprintf("StackSyncAction(%d)", int(a))
	}
}
