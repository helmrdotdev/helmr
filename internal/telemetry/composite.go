package telemetry

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
)

type CompositeReader struct {
	hot        *HotReader
	historical *HistoricalReader
}

func NewCompositeReader(hot *HotReader, historical *HistoricalReader) *CompositeReader {
	return &CompositeReader{hot: hot, historical: historical}
}

func (r *CompositeReader) ListEvents(ctx context.Context, q EventQuery) (EventPage, error) {
	watermark, err := r.hot.EventWatermark(ctx, q)
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{LastSeq: q.AfterSeq, Watermark: watermark}
	remaining := q.Limit
	if q.AfterSeq < watermark {
		if r.historical == nil {
			return EventPage{}, ErrHistoricalUnavailable
		}
		rows, last, err := r.historical.ListEvents(ctx, q, watermark)
		if err != nil {
			return EventPage{}, fmt.Errorf("%w: %v", ErrHistoricalUnavailable, err)
		}
		if err := r.verifyHistoricalEventCoverage(ctx, q, watermark, rows); err != nil {
			return EventPage{}, err
		}
		page.Events = append(page.Events, rows...)
		page.LastSeq = last
		page.Historical = len(rows)
		remaining -= int32(len(rows))
		if remaining <= 0 {
			return page, nil
		}
	}
	hotQuery := q
	hotQuery.AfterSeq = page.LastSeq
	hotQuery.Limit = remaining
	rows, last, err := r.hot.ListEventsAboveWatermark(ctx, hotQuery, watermark)
	if err != nil {
		return EventPage{}, err
	}
	page.Events = append(page.Events, rows...)
	if len(rows) > 0 {
		page.LastSeq = last
	}
	page.HotCount = len(rows)
	return page, nil
}

func (r *CompositeReader) ListRunLogChunks(ctx context.Context, q RunLogChunkQuery) (RunLogChunkPage, error) {
	watermark, err := r.hot.RunLogWatermark(ctx, q)
	if err != nil {
		return RunLogChunkPage{}, err
	}
	page := RunLogChunkPage{LastSeq: q.AfterSeq, Watermark: watermark}
	remaining := q.Limit
	if q.AfterSeq < watermark {
		if r.historical == nil {
			return RunLogChunkPage{}, ErrHistoricalUnavailable
		}
		rows, last, err := r.historical.ListRunLogChunks(ctx, q, watermark)
		if err != nil {
			return RunLogChunkPage{}, fmt.Errorf("%w: %v", ErrHistoricalUnavailable, err)
		}
		if err := r.verifyHistoricalRunLogCoverage(ctx, q, watermark, rows); err != nil {
			return RunLogChunkPage{}, err
		}
		page.Chunks = append(page.Chunks, rows...)
		page.LastSeq = last
		page.Historical = len(rows)
		remaining -= int32(len(rows))
		if remaining <= 0 {
			return page, nil
		}
	}
	hotQuery := q
	hotQuery.AfterSeq = page.LastSeq
	hotQuery.Limit = remaining
	rows, last, err := r.hot.ListRunLogChunksAboveWatermark(ctx, hotQuery, watermark)
	if err != nil {
		return RunLogChunkPage{}, err
	}
	page.Chunks = append(page.Chunks, rows...)
	if len(rows) > 0 {
		page.LastSeq = last
	}
	page.HotCount = len(rows)
	return page, nil
}

func (r *CompositeReader) ListTerminalOutput(ctx context.Context, q TerminalOutputQuery) (TerminalOutputPage, error) {
	watermark, err := r.hot.TerminalOutputWatermark(ctx, q)
	if err != nil {
		return TerminalOutputPage{}, err
	}
	page := TerminalOutputPage{LastOffset: q.AfterOffset, Watermark: watermark}
	remaining := q.Limit
	if q.AfterOffset < watermark {
		if r.historical == nil {
			return TerminalOutputPage{}, ErrHistoricalUnavailable
		}
		rows, last, err := r.historical.ListTerminalOutput(ctx, q, watermark)
		if err != nil {
			return TerminalOutputPage{}, fmt.Errorf("%w: %v", ErrHistoricalUnavailable, err)
		}
		if err := verifyHistoricalTerminalOutputCoverage(q.AfterOffset, watermark, q.Limit, rows); err != nil {
			return TerminalOutputPage{}, err
		}
		page.Chunks = append(page.Chunks, rows...)
		page.LastOffset = last
		page.Historical = len(rows)
		remaining -= int32(len(rows))
		if remaining <= 0 {
			return page, nil
		}
	}
	hotQuery := q
	hotQuery.AfterOffset = page.LastOffset
	hotQuery.Limit = remaining
	rows, last, err := r.hot.ListTerminalOutputAboveWatermark(ctx, hotQuery, watermark)
	if err != nil {
		return TerminalOutputPage{}, err
	}
	page.Chunks = append(page.Chunks, rows...)
	if len(rows) > 0 {
		page.LastOffset = last
	}
	page.HotCount = len(rows)
	return page, nil
}

func (r *CompositeReader) GetRunLogSnapshot(ctx context.Context, q RunLogSnapshotQuery) (RunLogSnapshot, error) {
	var snapshot RunLogSnapshot
	cursor := int64(0)
	const pageLimit = int32(1000)
	for {
		page, err := r.ListRunLogChunks(ctx, RunLogChunkQuery{
			OrgID:    q.OrgID,
			CellID:   q.CellID,
			RunID:    q.RunID,
			AfterSeq: cursor,
			Limit:    pageLimit,
		})
		if err != nil {
			return RunLogSnapshot{}, err
		}
		for _, chunk := range page.Chunks {
			data, _ := base64.StdEncoding.DecodeString(chunk.ContentBase64)
			switch chunk.Stream {
			case "stdout":
				snapshot.StdoutBytes += int64(len(data))
				snapshot.Stdout = appendTail(snapshot.Stdout, data, q.StdoutLimit)
			case "stderr":
				snapshot.StderrBytes += int64(len(data))
				snapshot.Stderr = appendTail(snapshot.Stderr, data, q.StderrLimit)
			}
			if seq, err := ParseCursor(chunk.ID); err == nil && seq > snapshot.Cursor {
				snapshot.Cursor = seq
			}
			if chunk.At.After(snapshot.UpdatedAt) {
				snapshot.UpdatedAt = chunk.At
			}
		}
		if len(page.Chunks) < int(pageLimit) || page.LastSeq <= cursor {
			break
		}
		cursor = page.LastSeq
	}
	snapshot.Truncated = isTailTruncated(snapshot.StdoutBytes, q.StdoutLimit) || isTailTruncated(snapshot.StderrBytes, q.StderrLimit)
	return snapshot, nil
}

func (r *CompositeReader) verifyHistoricalEventCoverage(ctx context.Context, q EventQuery, watermark int64, rows []api.RunEvent) error {
	gaps, err := r.hot.DeadLetteredTelemetrySeqs(ctx, DeadLetteredTelemetryQuery{
		OrgID:      q.OrgID,
		CellID:     q.CellID,
		StreamKind: "event",
		SourceKind: q.SubjectType,
		SourceID:   q.SubjectID,
		AfterSeq:   q.AfterSeq,
		Watermark:  watermark,
	})
	if err != nil {
		return err
	}
	return verifyHistoricalSeqCoverage(q.AfterSeq, watermark, q.Limit, len(rows), gaps, func(idx int) int64 {
		seq, err := ParseCursor(rows[idx].ID)
		if err != nil {
			return -1
		}
		return seq
	})
}

func (r *CompositeReader) verifyHistoricalRunLogCoverage(ctx context.Context, q RunLogChunkQuery, watermark int64, rows []api.RunLogChunk) error {
	gaps, err := r.hot.DeadLetteredTelemetrySeqs(ctx, DeadLetteredTelemetryQuery{
		OrgID:      q.OrgID,
		CellID:     q.CellID,
		StreamKind: "run_log",
		SourceKind: "run",
		SourceID:   q.RunID,
		AfterSeq:   q.AfterSeq,
		Watermark:  watermark,
	})
	if err != nil {
		return err
	}
	return verifyHistoricalSeqCoverage(q.AfterSeq, watermark, q.Limit, len(rows), gaps, func(idx int) int64 {
		seq, err := ParseCursor(rows[idx].ID)
		if err != nil {
			return -1
		}
		return seq
	})
}

func appendTail(existing []byte, next []byte, limit int64) []byte {
	existing = append(existing, next...)
	if limit <= 0 || int64(len(existing)) <= limit {
		return existing
	}
	return existing[int64(len(existing))-limit:]
}

func isTailTruncated(total int64, limit int64) bool {
	if limit <= 0 {
		return false
	}
	return total > limit
}

func verifyHistoricalTerminalOutputCoverage(afterOffset int64, watermark int64, limit int32, rows []TerminalOutputChunk) error {
	expected := afterOffset
	for _, row := range rows {
		if row.OffsetEnd <= expected {
			continue
		}
		if row.OffsetStart > expected {
			return LaggingError{WatermarkSeq: watermark, WantSeq: row.OffsetStart}
		}
		expected = row.OffsetEnd
	}
	if len(rows) < int(limit) && expected < watermark {
		return LaggingError{WatermarkSeq: watermark, WantSeq: watermark}
	}
	return nil
}

func verifyHistoricalSeqCoverage(afterSeq int64, watermark int64, limit int32, rowCount int, deadLettered []int64, seqAt func(int) int64) error {
	allowed := make(map[int64]struct{}, len(deadLettered))
	for _, seq := range deadLettered {
		allowed[seq] = struct{}{}
	}
	expected := afterSeq + 1
	skipAllowed := func() {
		for {
			if _, ok := allowed[expected]; !ok {
				return
			}
			expected++
		}
	}
	for idx := 0; idx < rowCount; idx++ {
		skipAllowed()
		seq := seqAt(idx)
		if seq <= 0 {
			return LaggingError{WatermarkSeq: watermark, WantSeq: expected}
		}
		if seq < expected {
			continue
		}
		if seq > expected {
			return LaggingError{WatermarkSeq: watermark, WantSeq: expected}
		}
		expected = seq + 1
	}
	skipAllowed()
	if rowCount < int(limit) && expected <= watermark {
		return LaggingError{WatermarkSeq: watermark, WantSeq: expected}
	}
	return nil
}
