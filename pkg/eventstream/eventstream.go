package eventstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/khulnasoft/inngest/pkg/consts"
)

var (
	ErrInvalidRequestBody = fmt.Errorf("Request body must contain an event object or an array of event objects")
	ErrEventTooLarge      = fmt.Errorf("Event is over the max size")
)

type StreamItem struct {
	N    int
	Item json.RawMessage
}

// ParseStream parses a reader, publishing a stream of JSON-encoded events to the given channel,
// ensuring that no individual event is too large.
//
// This closes the given channel once the JSON stream in the given reader has been parsed.
//
// Usage:
//
//			var err error
//			go func() {
//			        err = ParseStream(ctx, r, stream)
//			()
//
//			for bytes := range stream {
//			        // consume event, transform event, etc
//			}
//
//	     if err != nil {
//	             // handle error
//	     }
func ParseStream(ctx context.Context, r io.Reader, stream chan StreamItem, maxSize int) error {
	defer func() {
		close(stream)
	}()
	d := json.NewDecoder(r)

	token, err := d.Token()
	if err == io.EOF {
		return nil
	}

	delim, ok := token.(json.Delim)
	if !ok {
		// Invalid type
		return ErrInvalidRequestBody
	}

	switch delim {
	case '{':
		// We've already peeked the first char.  Read all, then prepend a '{'
		byt, err := io.ReadAll(d.Buffered())
		if err != nil {
			return err
		}
		// d.Buffered() only returns a portion of the buffered stream;  read the rest
		// and concat.
		extra, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		data := append([]byte("{"), byt...)
		data = append(data, extra...)
		if len(data) > maxSize {
			return fmt.Errorf("%w: Max %d bytes / Size %d bytes", ErrEventTooLarge, maxSize, len(data))
		}

		select {
		case stream <- StreamItem{Item: data}:
			// Sent
		case <-ctx.Done():
			// Early exit; a problem somewhere else in the pipeline
			return nil
		}
	case '[':
		i := 0
		// Parse a stream of tokens
		for d.More() {
			if i == consts.MaxEvents {
				return &ErrEventCount{Max: consts.MaxEvents}
			}

			jsonEvt := json.RawMessage{}
			if err := d.Decode(&jsonEvt); err != nil {
				return err
			}
			if len(jsonEvt) > maxSize {
				return fmt.Errorf("%w: Max %d bytes / Size %d bytes", ErrEventTooLarge, maxSize, len(jsonEvt))
			}
			select {
			case stream <- StreamItem{N: i, Item: jsonEvt}:
				// Sent
				i++
			case <-ctx.Done():
				// Early exit; a problem somewhere else in the pipeline
				return nil
			}
		}
	default:
		return ErrInvalidRequestBody
	}
	return nil
}
