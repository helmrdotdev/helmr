package telemetry

import "testing"

func TestVerifyHistoricalSeqCoverageRejectsUnknownGap(t *testing.T) {
	err := verifyHistoricalSeqCoverage(0, 3, 100, 2, nil, func(idx int) int64 {
		return []int64{1, 3}[idx]
	})
	if err == nil {
		t.Fatal("expected lagging error")
	}
	lag, ok := err.(LaggingError)
	if !ok {
		t.Fatalf("error = %T, want LaggingError", err)
	}
	if lag.WantSeq != 2 || lag.WatermarkSeq != 3 {
		t.Fatalf("lagging error = %+v, want seq 2 at watermark 3", lag)
	}
}

func TestVerifyHistoricalSeqCoverageAllowsDeadLetteredGap(t *testing.T) {
	err := verifyHistoricalSeqCoverage(0, 3, 100, 2, []int64{2}, func(idx int) int64 {
		return []int64{1, 3}[idx]
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyHistoricalSeqCoverageRejectsMissingTail(t *testing.T) {
	err := verifyHistoricalSeqCoverage(0, 3, 100, 1, nil, func(idx int) int64 {
		return []int64{1}[idx]
	})
	if err == nil {
		t.Fatal("expected lagging error")
	}
	lag, ok := err.(LaggingError)
	if !ok {
		t.Fatalf("error = %T, want LaggingError", err)
	}
	if lag.WantSeq != 2 || lag.WatermarkSeq != 3 {
		t.Fatalf("lagging error = %+v, want seq 2 at watermark 3", lag)
	}
}
