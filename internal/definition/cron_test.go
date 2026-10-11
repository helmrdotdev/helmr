package definition

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCronContract(t *testing.T) {
	valid := []string{
		"0 9 * * *",
		"*/5 * * * *",
		"0,15,30,45 9-17 * * 1-5",
		"0 9 * JAN MON",
		"0 9 ? * *",
		"00 9 * * *",
		"0 9 * * 2,1",
		"0  9 * * *",
		" 0 9 * * * ",
	}
	for _, value := range valid {
		if err := ValidateCron(value); err != nil {
			t.Errorf("ValidateCron(%q): %v", value, err)
		}
	}
	invalid := []string{
		"0 0 30 2 *",
		"CRON_TZ=UTC 0 0 * * *",
		"@daily",
		"0 0 1 * 0-7",
		"0 9 * * 0,7",
		"0 9 * *",
		"0 9 * * * *",
	}
	for _, value := range invalid {
		if err := ValidateCron(value); err == nil {
			t.Errorf("ValidateCron(%q) succeeded", value)
		}
	}
}

func TestLoadLocationRejectsMissingRuntimeRules(t *testing.T) {
	if _, err := loadLocationFromRoot("Asia/Tokyo", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing runtime timezone rules were accepted")
	}
}

func TestNextCronTimeUsesExactTimezone(t *testing.T) {
	next, err := NextCronTime("0 9 * * *", "Asia/Tokyo", time.Date(2026, 6, 1, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	if _, err := NextCronTime("0 9 * * *", "asia/tokyo", time.Now()); err == nil {
		t.Fatal("lowercase timezone was accepted")
	}
	for _, name := range []string{"localtime", "posixrules", "posix/UTC", "right/UTC"} {
		if err := ValidateTimezone(name); err == nil {
			t.Fatalf("non-IANA tzfile %q was accepted", name)
		}
	}
}

func TestValidateTimezoneUsesProductManifest(t *testing.T) {
	for _, name := range []string{"Asia/Tokyo", "America/New_York", "UTC"} {
		if err := ValidateTimezone(name); err != nil {
			t.Fatalf("ValidateTimezone(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "Local", "asia/tokyo", "Not/AZone"} {
		if err := ValidateTimezone(name); err == nil {
			t.Fatalf("ValidateTimezone(%q) succeeded", name)
		}
	}
}

func TestCronSkipsSecondRepeatedLocalTimeAndNonexistentTimes(t *testing.T) {
	cases := []struct {
		name, zone, expression string
		anchor, want           time.Time
	}{
		{"first repeated occurrence", "America/New_York", "30 1 * * *", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{"second repeated occurrence omitted", "America/New_York", "30 1 * * *", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)},
		{"promotion inside repeated hour", "America/New_York", "30 1 * * *", time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)},
		{"spring gap", "America/New_York", "30 2 * * *", time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC)},
		{"half hour repeat", "Australia/Lord_Howe", "45 1 * * *", time.Date(2026, 4, 4, 14, 45, 0, 0, time.UTC), time.Date(2026, 4, 5, 15, 15, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NextCronTime(c.expression, c.zone, c.anchor)
			if err != nil || !got.Equal(c.want) {
				t.Fatalf("next=%s want %s: %v", got, c.want, err)
			}
		})
	}
	times, err := NextCronTimes("30 1 * * *", "America/New_York", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil || len(times) != 2 || !times[1].Equal(time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("range=%v: %v", times, err)
	}
}
