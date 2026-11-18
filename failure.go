package loom

import "errors"

// PublicFacing is the part of a failure that can go public: a stable
// machine-readable code plus a ready-to-render message. loom only carries it
// and does not care who the eventual reader is; the product decides what text
// becomes public.
type PublicFacing struct {
	// Code is a machine-readable error code the product may route on.
	Code string
	// Message is the finished, ready-to-render text.
	Message string
}

// FailureError separates the three texts one failure needs:
//   - Cause, the raw error for operators: provider names, endpoints, and other
//     internals
//   - Model, the text fed back to the model, free of provider internals,
//     because tool results can become public and the model echoes what it sees
//   - Public, the finished text the product may show to its eventual reader
//
// Error returns Cause's text (Model when Cause is nil), so logs and existing
// error reporting keep seeing the original. The other two texts travel
// separately: ItemError.Message prefers Model, while ItemError.Public and
// CloseReason.Public carry Public. The raw text itself lands in
// ItemError.Detail, which JSON serialization never carries.
//
// Build one with Fail at the boundary that knows all three texts, and read it
// back with FailureOf, which finds the outermost FailureError through any
// number of fmt.Errorf("%w") wrappings.
type FailureError struct {
	Model  string
	Public PublicFacing
	Cause  error
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

// Fail wraps cause with the model-facing text and the public pair. A nil cause
// is allowed for failures the product detects itself. The returned error
// reports cause's text through Error, keeps cause reachable through
// errors.Is/errors.As, and is found by FailureOf.
func Fail(cause error, model string, public PublicFacing) error {
	return &FailureError{Model: model, Public: public, Cause: cause}
}

// FailureOf reports the outermost FailureError in err's chain. When several
// are nested, the outermost is the one wrapped closest to where the caller
// sees the error, which is also the boundary that supplied the final texts.
func FailureOf(err error) (*FailureError, bool) {
	return errors.AsType[*FailureError](err)
}

// modelText returns the text the model should see for err: the model-facing
// text of the outermost FailureError when one carries a non-empty one,
// otherwise err's own text. Every place that feeds a failure back to the model
// goes through it, so what the model sees and what ItemError.Message stores
// can never drift apart.
func modelText(err error) string {
	if fe, ok := FailureOf(err); ok && fe.Model != "" {
		return fe.Model
	}
	return err.Error()
}

// publicFacingPtr returns pf as a pointer when it carries any content, and nil
// for the zero value, so a serialized error never carries an empty public
// block.
func publicFacingPtr(pf PublicFacing) *PublicFacing {
	if pf.Code == "" && pf.Message == "" {
		return nil
	}
	return &pf
}

// failurePublic returns the public pair of the outermost FailureError in err's
// chain, or nil when there is none.
func failurePublic(err error) *PublicFacing {
	if fe, ok := FailureOf(err); ok {
		return publicFacingPtr(fe.Public)
	}
	return nil
}

// failureItemError builds the ItemError for a failed item whose underlying
// error is err. Detail always keeps the raw text. Message is exactly the text
// the model sees, through modelText, so provider internals stay out of what
// the model — and through it the public — can see; Public carries the public
// pair when the product supplied one. Without a FailureError the ItemError is
// exactly what the raw error says, which is the behaviour loom had before this
// type existed.
func failureItemError(code string, err error) *ItemError {
	itemErr := &ItemError{Code: code, Message: modelText(err), Detail: err.Error()}
	if p := failurePublic(err); p != nil {
		itemErr.Public = p
	}
	return itemErr
}
