package loom

import "errors"

// UserFacing is the failure text meant for the end user: a stable
// machine-readable code plus a ready-to-render message. loom only carries it;
// the product decides what text a user should see.
type UserFacing struct {
	// Code is a machine-readable error code the product may route on.
	Code string
	// Message is the finished, user-ready text.
	Message string
}

// FailureError separates the three texts one failure needs:
//   - Cause, the raw error for operators: provider names, endpoints, and other
//     internals
//   - Model, the text fed back to the model, free of provider internals,
//     because tool results are visible to end users and the model echoes what
//     it sees
//   - User, the finished text the product shows the end user
//
// Error returns Cause's text (Model when Cause is nil), so logs and existing
// error reporting keep seeing the original. The other two texts travel
// separately: ItemError.Message prefers Model, while ItemError.User and
// CloseReason.User carry User. The raw text itself lands in ItemError.Detail,
// which JSON serialization never carries.
//
// Build one with Fail at the boundary that knows all three texts, and read it
// back with FailureOf, which finds the outermost FailureError through any
// number of fmt.Errorf("%w") wrappings.
type FailureError struct {
	Model string
	User  UserFacing
	Cause error
}

// Error returns the raw cause's text, so operator-facing reporting is
// unchanged; when Cause is nil it returns the model-facing text, since that is
// all this error carries.
func (e *FailureError) Error() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Model
}

// Unwrap exposes Cause to errors.Is and errors.As.
func (e *FailureError) Unwrap() error { return e.Cause }

// Fail wraps cause with the model-facing text and the user-facing pair. A nil
// cause is allowed for failures the product detects itself. The returned error
// reports cause's text through Error, keeps cause reachable through
// errors.Is/errors.As, and is found by FailureOf.
func Fail(cause error, model string, user UserFacing) error {
	return &FailureError{Model: model, User: user, Cause: cause}
}

// FailureOf reports the outermost FailureError in err's chain. When several
// are nested, the outermost is the one wrapped closest to where the caller
// sees the error, which is also the boundary that supplied the final texts.
func FailureOf(err error) (*FailureError, bool) {
	return errors.AsType[*FailureError](err)
}

// userFacingPtr returns uf as a pointer when it carries any content, and nil
// for the zero value, so a serialized error never carries an empty user block.
func userFacingPtr(uf UserFacing) *UserFacing {
	if uf.Code == "" && uf.Message == "" {
		return nil
	}
	return &uf
}

// failureUser returns the user-facing pair of the outermost FailureError in
// err's chain, or nil when there is none.
func failureUser(err error) *UserFacing {
	if fe, ok := FailureOf(err); ok {
		return userFacingPtr(fe.User)
	}
	return nil
}

// failureItemError builds the ItemError for a failed item whose underlying
// error is err. Detail always keeps the raw text. Message prefers the
// model-facing text of a FailureError, so provider internals stay out of what
// the model — and through it the end user — can see; User carries the
// user-facing pair when the product supplied one. Without a FailureError the
// ItemError is exactly what the raw error says, which is the behaviour loom
// had before this type existed.
func failureItemError(code string, err error) *ItemError {
	itemErr := &ItemError{Code: code, Message: err.Error(), Detail: err.Error()}
	if fe, ok := FailureOf(err); ok {
		if fe.Model != "" {
			itemErr.Message = fe.Model
		}
		if u := userFacingPtr(fe.User); u != nil {
			itemErr.User = u
		}
	}
	return itemErr
}
