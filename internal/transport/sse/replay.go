package sse

import "context"

// Reader provides ordered, durable events for one authorized stream.
type Reader[T any] interface {
	List(context.Context, int64, int) ([]T, error)
}

// ReplayOptions describes the event-specific parts of stream replay.
type ReplayOptions[T any] struct {
	PageSize int
	Sequence func(T) int64
	Terminal func(T) bool
	Emit     func(T) error
}

// Replay emits all events after afterSeq, stopping at the first terminal event.
func Replay[T any](
	ctx context.Context,
	reader Reader[T],
	afterSeq int64,
	options ReplayOptions[T],
) (int64, bool, error) {
	lastSeq := afterSeq
	for {
		events, err := reader.List(ctx, lastSeq, options.PageSize)
		if err != nil {
			return lastSeq, false, err
		}
		for _, event := range events {
			seq := options.Sequence(event)
			if seq <= lastSeq {
				continue
			}
			if err := options.Emit(event); err != nil {
				return lastSeq, false, err
			}
			lastSeq = seq
			if options.Terminal(event) {
				return lastSeq, true, nil
			}
		}
		if len(events) < options.PageSize {
			return lastSeq, false, nil
		}
	}
}

// EmitLive emits event once its sequence gap has been filled from durable storage.
func EmitLive[T any](
	ctx context.Context,
	reader Reader[T],
	lastSeq int64,
	event T,
	options ReplayOptions[T],
) (int64, bool, error) {
	seq := options.Sequence(event)
	if seq <= lastSeq {
		return lastSeq, false, nil
	}
	if seq > lastSeq+1 {
		var terminal bool
		var err error
		lastSeq, terminal, err = Replay(ctx, reader, lastSeq, options)
		if err != nil || terminal || seq <= lastSeq {
			return lastSeq, terminal, err
		}
	}
	if err := options.Emit(event); err != nil {
		return lastSeq, false, err
	}
	return seq, options.Terminal(event), nil
}
