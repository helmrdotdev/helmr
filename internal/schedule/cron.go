package schedule

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const CronContractVersion = "helmr.cron.v0"

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

var (
	locationCache   sync.Map
	zoneNamesOnce   sync.Once
	zoneNames       map[string]struct{}
	zoneManifestErr error
)

const tzdbRoot = "/usr/share/zoneinfo"

type fieldBounds struct {
	min int
	max int
	dow bool
}

var cronFieldBounds = [...]fieldBounds{
	{min: 0, max: 59},
	{min: 0, max: 23},
	{min: 1, max: 31},
	{min: 1, max: 12},
	{min: 0, max: 7, dow: true},
}

func ValidateCron(expression string) error {
	fields := strings.Split(expression, " ")
	if len(fields) != len(cronFieldBounds) || strings.Join(fields, " ") != expression {
		return errors.New("cron must be a valid 5-field expression separated by one ASCII space")
	}
	for i, field := range fields {
		if err := validateCronField(field, cronFieldBounds[i]); err != nil {
			return fmt.Errorf("cron field %d: %w", i+1, err)
		}
	}
	if _, err := cronParser.Parse(normalizeCron(expression)); err != nil {
		return fmt.Errorf("cron is invalid: %w", err)
	}
	return nil
}

func ValidateTimezone(name string) error {
	if name == "" || name != strings.TrimSpace(name) || name == "Local" {
		return errors.New("timezone must be an exact IANA timezone identifier")
	}
	if _, err := loadLocation(name); err != nil {
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
	loc, _ := loadLocation(timezone)
	spec, _ := cronParser.Parse(normalizeCron(expression))
	next := spec.Next(anchor.In(loc)).UTC()
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
	loc, _ := loadLocation(timezone)
	spec, _ := cronParser.Parse(normalizeCron(expression))
	result := make([]time.Time, 0, count)
	cursor := anchor.In(loc)
	for len(result) < count {
		next := spec.Next(cursor)
		if next.IsZero() {
			return nil, errors.New("cron has no future occurrences")
		}
		result = append(result, next.UTC())
		cursor = next
	}
	return result, nil
}

func validateCronField(field string, bounds fieldBounds) error {
	if field == "" {
		return errors.New("field cannot be empty")
	}
	clauses := strings.Split(field, ",")
	seen := make(map[int]struct{}, bounds.max-bounds.min+1)
	previousMax := -1
	for _, clause := range clauses {
		values, contributedMin, contributedMax, err := expandCronClause(clause, bounds)
		if err != nil {
			return err
		}
		if previousMax >= 0 && contributedMin <= previousMax {
			return errors.New("list clauses must be strictly increasing")
		}
		previousMax = contributedMax
		clauseValues := make(map[int]struct{}, len(values))
		for _, value := range values {
			if bounds.dow && value == 7 {
				value = 0
			}
			clauseValues[value] = struct{}{}
		}
		for value := range clauseValues {
			if _, exists := seen[value]; exists {
				return errors.New("list clauses must not overlap")
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func normalizeCron(expression string) string {
	fields := strings.Split(expression, " ")
	if fields[4] == "*" {
		return expression
	}
	values, _, _, _ := expandCronField(fields[4], cronFieldBounds[4])
	normalized := make([]int, 0, len(values))
	seen := make(map[int]struct{}, len(values))
	for _, value := range values {
		if value == 7 {
			value = 0
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	slices.Sort(normalized)
	if len(normalized) == 7 {
		fields[4] = "0-6"
	} else {
		parts := make([]string, 0, len(normalized))
		for _, value := range normalized {
			parts = append(parts, strconv.Itoa(value))
		}
		fields[4] = strings.Join(parts, ",")
	}
	return strings.Join(fields, " ")
}

func expandCronField(field string, bounds fieldBounds) ([]int, int, int, error) {
	clauses := strings.Split(field, ",")
	values := make([]int, 0, bounds.max-bounds.min+1)
	numericMin, numericMax := bounds.max, bounds.min
	for _, clause := range clauses {
		expanded, clauseMin, clauseMax, err := expandCronClause(clause, bounds)
		if err != nil {
			return nil, 0, 0, err
		}
		values = append(values, expanded...)
		numericMin = min(numericMin, clauseMin)
		numericMax = max(numericMax, clauseMax)
	}
	return values, numericMin, numericMax, nil
}

func expandCronClause(clause string, bounds fieldBounds) ([]int, int, int, error) {
	if clause == "" {
		return nil, 0, 0, errors.New("list clause cannot be empty")
	}
	base := clause
	step := 1
	if strings.Contains(clause, "/") {
		parts := strings.Split(clause, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, 0, 0, errors.New("step must have one base and one positive integer")
		}
		base = parts[0]
		if base != "*" && !strings.Contains(base, "-") {
			return nil, 0, 0, errors.New("step applies only to a wildcard or range")
		}
		var err error
		step, err = parseCronInteger(parts[1])
		if err != nil || step <= 0 {
			return nil, 0, 0, errors.New("step must be a positive decimal integer")
		}
	}

	start, end := bounds.min, bounds.max
	switch {
	case base == "*":
	case strings.Contains(base, "-"):
		parts := strings.Split(base, "-")
		if len(parts) != 2 {
			return nil, 0, 0, errors.New("range must contain one hyphen")
		}
		var err error
		start, err = parseCronInteger(parts[0])
		if err != nil {
			return nil, 0, 0, errors.New("range start must be a decimal integer")
		}
		end, err = parseCronInteger(parts[1])
		if err != nil {
			return nil, 0, 0, errors.New("range end must be a decimal integer")
		}
	default:
		value, err := parseCronInteger(base)
		if err != nil {
			return nil, 0, 0, errors.New("value must be a decimal integer, wildcard, or range")
		}
		start, end = value, value
	}
	if start < bounds.min || end > bounds.max || start > end {
		return nil, 0, 0, fmt.Errorf("value must be between %d and %d", bounds.min, bounds.max)
	}
	values := make([]int, 0, (end-start)/step+1)
	for value := start; value <= end; value += step {
		values = append(values, value)
	}
	return values, values[0], values[len(values)-1], nil
}

func parseCronInteger(value string) (int, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("invalid decimal integer")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid decimal integer")
		}
	}
	return strconv.Atoi(value)
}

func loadLocation(name string) (*time.Location, error) {
	if cached, ok := locationCache.Load(name); ok {
		return cached.(*time.Location), nil
	}
	if err := validateZoneName(name); err != nil {
		return nil, err
	}
	if filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return nil, errors.New("timezone path must be relative")
	}
	parts := strings.Split(name, "/")
	current := tzdbRoot
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
	actual, _ := locationCache.LoadOrStore(name, location)
	return actual.(*time.Location), nil
}

func validateZoneName(name string) error {
	zoneNamesOnce.Do(func() {
		zoneNames = make(map[string]struct{})
		file, err := os.Open(filepath.Join(tzdbRoot, "tzdata.zi"))
		if err != nil {
			zoneManifestErr = err
			return
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 2 && (fields[0] == "Z" || fields[0] == "Zone") {
				zoneNames[fields[1]] = struct{}{}
			}
			if len(fields) >= 3 && (fields[0] == "L" || fields[0] == "Link") {
				zoneNames[fields[2]] = struct{}{}
			}
		}
		zoneManifestErr = scanner.Err()
	})
	if zoneManifestErr != nil {
		return zoneManifestErr
	}
	if _, ok := zoneNames[name]; !ok {
		return errors.New("timezone identifier is absent from the pinned tzdb manifest")
	}
	return nil
}
