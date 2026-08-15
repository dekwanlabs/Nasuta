package workflow

import (
	"context"
	"fmt"
	"strings"
)

func (service *Service) Publish(
	definitions []Definition,
	admin bool,
) error {
	return service.PublishAs(
		context.Background(), definitions, 0, admin,
	)
}

func (service *Service) PublishAs(
	ctx context.Context,
	definitions []Definition,
	actorUserID int64,
	admin bool,
) error {
	if service == nil || service.catalog == nil {
		return ErrUnavailable
	}
	if !admin {
		return ErrForbidden
	}
	if len(definitions) == 0 {
		return fmt.Errorf("workflow definitions are required: %w", ErrInvalid)
	}
	if err := service.catalog.PublishAs(ctx, definitions, actorUserID); err != nil {
		return err
	}
	return nil
}

func (service *Service) ListDefinitions() ([]Definition, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	return service.catalog.List(), nil
}

func (service *Service) ListRecords(
	ctx context.Context,
	cursor DefinitionCursor,
	limit int,
) ([]DefinitionRecord, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	return service.catalog.ListRecords(ctx, cursor, limit)
}

func (service *Service) SetDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
	admin bool,
) error {
	if service == nil || service.catalog == nil {
		return ErrUnavailable
	}
	if !admin {
		return ErrForbidden
	}
	return service.catalog.SetDefault(ctx, id, version, actorUserID)
}

func (service *Service) SetActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
	admin bool,
) error {
	if service == nil || service.catalog == nil {
		return ErrUnavailable
	}
	if !admin {
		return ErrForbidden
	}
	return service.catalog.SetActive(ctx, id, version, active, actorUserID)
}

func (service *Service) ListAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]DefinitionAuditEvent, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	if !admin {
		return nil, ErrForbidden
	}
	return service.catalog.ListAudit(ctx, id, afterSeq, limit)
}

func (service *Service) GetRollout(
	id string,
) (RolloutRule, bool, error) {
	if service == nil || service.catalog == nil {
		return RolloutRule{}, false, ErrUnavailable
	}
	rule, ok := service.catalog.GetRollout(strings.TrimSpace(id))
	return rule, ok, nil
}

func (service *Service) SetRollout(
	ctx context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
	admin bool,
) (RolloutRule, error) {
	if service == nil || service.catalog == nil {
		return RolloutRule{}, ErrUnavailable
	}
	if !admin {
		return RolloutRule{}, ErrForbidden
	}
	return service.catalog.SetRollout(
		ctx, id, candidateVersion, percentageBPS, salt, active, actorUserID,
	)
}

func (service *Service) ListRolloutAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]RolloutAuditEvent, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	if !admin {
		return nil, ErrForbidden
	}
	return service.catalog.ListRolloutAudit(ctx, id, afterSeq, limit)
}

func (service *Service) GetRun(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*RunRecord, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	run, err := service.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !admin && run.ActorUserID != userID {
		return nil, ErrForbidden
	}
	return run, nil
}

func (service *Service) ListNodeRuns(
	ctx context.Context,
	runID string,
	cursor NodeRunCursor,
	limit int,
	userID int64,
	admin bool,
) ([]NodeRunRecord, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListNodeRuns(ctx, runID, cursor, limit)
}

func (service *Service) ListEvents(
	ctx context.Context,
	runID string,
	afterSeq int64,
	limit int,
	userID int64,
	admin bool,
) ([]Event, error) {
	_, reader, err := service.OpenRunEvents(ctx, runID, userID, admin)
	if err != nil {
		return nil, err
	}
	return reader.List(ctx, afterSeq, limit)
}

func (service *Service) OpenRunEvents(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*RunRecord, *RunEventReader, error) {
	run, err := service.GetRun(ctx, runID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	return run, &RunEventReader{store: service.store, runID: runID}, nil
}

func (service *Service) SubscribeEvents(
	runID string,
) (<-chan Event, func(), error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return nil, nil, err
	}
	events, unsubscribe := service.store.SubscribeEvents(runID)
	return events, unsubscribe, nil
}

func (service *Service) ListHandoffs(
	ctx context.Context,
	runID string,
	cursor HandoffCursor,
	limit int,
	userID int64,
	admin bool,
) ([]Handoff, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListHandoffs(ctx, runID, cursor, limit)
}
