package agent

import (
	"testing"
	"time"
)

func TestScheduleEvaluationBoundsRecoveryAndActivation(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		name, cron, zone, next, now, until, want, wantNext string
		missed                                             bool
	}{
		{"latest only", "* * * * *", "UTC", "2026-10-01T00:00:00Z", "2026-10-07T12:03:20Z", "", "2026-10-07T12:03:00Z", "2026-10-07T12:04:00Z", true},
		{"late daily", "0 9 * * *", "UTC", "2026-10-06T09:00:00Z", "2026-10-07T09:06:00Z", "", "", "2026-10-08T09:00:00Z", true},
		{"cutover exclusive", "* * * * *", "UTC", "2026-10-07T12:00:00Z", "2026-10-07T12:03:20Z", "2026-10-07T12:02:00Z", "2026-10-07T12:01:00Z", "2026-10-07T12:02:00Z", true},
		{"at grace boundary", "0 9 * * *", "UTC", "2026-10-07T09:00:00Z", "2026-10-07T09:05:00Z", "", "2026-10-07T09:00:00Z", "2026-10-08T09:00:00Z", false},
		{"second repeated local time", "30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:31:00Z", "", "", "2026-11-02T06:30:00Z", true},
		{"spring gap", "30 2 * * *", "America/New_York", "2026-03-07T07:30:00Z", "2026-03-08T07:31:00Z", "", "", "2026-03-09T06:30:00Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scheduleActivation{cron: tc.cron, timezone: tc.zone, next: at(tc.next), graceMS: 300000}
			if tc.until != "" {
				v := at(tc.until)
				s.until = &v
			}
			got, err := evaluateSchedule(s, at(tc.now))
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if got.latest != nil {
				want = got.latest.Format(time.RFC3339)
			}
			if want != tc.want || !got.next.Equal(at(tc.wantNext)) || (got.missed != nil) != tc.missed {
				t.Fatalf("evaluation=%+v latest=%s", got, want)
			}
		})
	}
}
