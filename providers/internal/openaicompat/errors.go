package openaicompat

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"

	"github.com/openai/openai-go/v3/packages/ssestream"
)

// StreamErrorBody extracts structured evidence without parsing the SDK's error
// text. The caller retains err as the cause, including the unmodified SSE event.
func StreamErrorBody(err error) jsontext.Value {
	event, ok := errors.AsType[*ssestream.StreamError](err)
	if !ok {
		return nil
	}
	var envelope struct {
		Error jsontext.Value `json:"error"`
	}
	if jsonv2.Unmarshal(event.Event.Data, &envelope) != nil {
		return nil
	}
	return envelope.Error
}
