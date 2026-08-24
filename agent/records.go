package agent

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

func appendRecord(ctx context.Context, journal Journal, kind RecordKind, payload any) (Record, error) {
	data, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return Record{}, fmt.Errorf("encode %s record: %w", kind, err)
	}
	record, err := journal.Append(ctx, PendingRecord{Kind: kind, Data: data})
	if err != nil {
		return Record{}, fmt.Errorf("append %s record: %w", kind, err)
	}
	return record, nil
}

func appendRecordAndApply(ctx context.Context, journal Journal, reducer *stateReducer, kind RecordKind, payload any) (Record, error) {
	record, err := appendRecord(ctx, journal, kind, payload)
	if err != nil {
		return Record{}, err
	}
	if err := reducer.apply(record); err != nil {
		return record, fmt.Errorf("apply appended %s record: %w", kind, err)
	}
	return record, nil
}

func (record Record) decode[T any]() (T, error) {
	var payload T
	if err := json.Unmarshal(record.Data, &payload); err != nil {
		return payload, fmt.Errorf("decode %s record at sequence %d: %w", record.Kind, record.Sequence, err)
	}
	return payload, nil
}
