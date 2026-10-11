package definition

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const CronSemanticsVersion = "helmr-cron-v1/standard-5-field/first-local-occurrence"

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

var (
	locationCache sync.Map
	zoneNamesOnce sync.Once
	zoneNames     map[string]struct{}
)

const tzdbRoot = "/usr/share/zoneinfo"

//go:embed tzdb_names.txt
var zoneNamesManifest string

func ValidateCron(expression string) error {
	if len(strings.Fields(expression)) != 5 {
		return errors.New("cron must contain exactly five fields")
	}
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return fmt.Errorf("cron is invalid: %w", err)
	}
	// With no year field, every possible calendar combination recurs within the
	// parser's five-year horizon. Use a fixed UTC anchor, independent of admission.
	if schedule.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return errors.New("cron has no calendar occurrences")
	}
	return nil
}

func ValidateTimezone(name string) error {
	if name == "" || name != strings.TrimSpace(name) || name == "Local" {
		return errors.New("timezone must be an exact IANA timezone identifier")
	}
	if err := validateZoneName(name); err != nil {
		return errors.New("timezone must be an exact IANA timezone identifier")
	}
	return nil
}

func NextCronTime(expression string, timezone string, anchor time.Time) (time.Time, error) {
	if err := ValidateCron(expression); err != nil {
		return time.Time{}, err
	}
	if err := ValidateTimezone(timezone); err != nil {
		return time.Time{}, err
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone rules: %w", err)
	}
	spec, _ := cronParser.Parse(expression)
	next := nextCronOccurrence(spec, anchor.In(loc)).UTC()
	if next.IsZero() {
		return time.Time{}, errors.New("cron has no future occurrences")
	}
	return next, nil
}

func NextCronTimes(expression string, timezone string, anchor time.Time, count int) ([]time.Time, error) {
	if count < 0 {
		return nil, errors.New("cron occurrence count cannot be negative")
	}
	if err := ValidateCron(expression); err != nil {
		return nil, err
	}
	if err := ValidateTimezone(timezone); err != nil {
		return nil, err
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone rules: %w", err)
	}
	spec, _ := cronParser.Parse(expression)
	result := make([]time.Time, 0, count)
	cursor := anchor.In(loc)
	for len(result) < count {
		next := nextCronOccurrence(spec, cursor)
		if next.IsZero() {
			return nil, errors.New("cron has no future occurrences")
		}
		result = append(result, next.UTC())
		cursor = next
	}
	return result, nil
}

// nextCronOccurrence omits the second instance of local times repeated by a
// backwards timezone transition. This also handles non-hour offset changes.
func nextCronOccurrence(spec cron.Schedule, anchor time.Time) time.Time {
	for {
		next := spec.Next(anchor)
		if next.IsZero() {
			return next
		}
		start, _ := next.ZoneBounds()
		if start.IsZero() {
			return next
		}
		_, before := start.Add(-time.Nanosecond).Zone()
		_, after := next.Zone()
		repeated := time.Duration(before-after) * time.Second
		if repeated <= 0 || !next.Before(start.Add(repeated)) {
			return next
		}
		anchor = next
	}
}

func loadLocation(name string) (*time.Location, error) {
	if cached, ok := locationCache.Load(name); ok {
		return cached.(*time.Location), nil
	}
	if err := validateZoneName(name); err != nil {
		return nil, err
	}
	location, err := loadLocationFromRoot(name, tzdbRoot)
	if err != nil {
		return nil, err
	}
	actual, _ := locationCache.LoadOrStore(name, location)
	return actual.(*time.Location), nil
}

func loadLocationFromRoot(name string, root string) (*time.Location, error) {
	if filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return nil, errors.New("timezone path must be relative")
	}
	parts := strings.Split(name, "/")
	current := root
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("timezone path is invalid")
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return nil, err
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == part {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("timezone identifier does not exist")
		}
		current = filepath.Join(current, part)
	}
	data, err := os.ReadFile(current)
	if err != nil {
		return nil, err
	}
	location, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, err
	}
	return location, nil
}

func validateZoneName(name string) error {
	zoneNamesOnce.Do(func() {
		zoneNames = make(map[string]struct{})
		for zoneName := range strings.FieldsSeq(zoneNamesManifest) {
			zoneNames[zoneName] = struct{}{}
		}
	})
	if _, ok := zoneNames[name]; !ok {
		return errors.New("timezone identifier is absent from the pinned tzdb manifest")
	}
	return nil
}
