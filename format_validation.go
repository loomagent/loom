package loom

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The format helpers check the part of a format that a shape pattern cannot express: whether a
// date exists in the calendar, whether a clock's components are in range, and where a leap second
// is allowed.
//
// They run as contract validators, so a model that sends 2024-02-30 is told to fix it and can.
// The schema keeps `format` as the specification's annotation and the projected pattern as the
// shape; ValidateSchema, which follows the specification, deliberately asserts neither. The
// assertions live here, and testdata/format holds the specification's own cases for them.

var (
	calendarDatePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	clockTimePattern    = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|([+-])(\d{2}):(\d{2}))$`)
)

// validateCalendarDate reports a date the calendar does not have. The shape pattern accepts
// 2024-02-30 and 2021-02-29; only the calendar knows they do not exist.
func validateCalendarDate(_ context.Context, value string) error {
	match := calendarDatePattern.FindStringSubmatch(value)
	if match == nil {
		return Invalid("%q is not a date; write it as YYYY-MM-DD", value)
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return Invalid("%q is not a day the calendar has", value)
	}
	return nil
}

// validateClockTime reports a time the clock does not have, including a leap second claimed at
// the wrong moment: a second of 60 only happens at the end of a UTC day, so an offset has to put
// it at 23:59:60 UTC.
func validateClockTime(_ context.Context, value string) error {
	match := clockTimePattern.FindStringSubmatch(value)
	if match == nil {
		return Invalid("%q is not an RFC 3339 time; write it as HH:MM:SS with an offset, such as 08:30:06Z", value)
	}
	hour, minute, second := matchNumber(match[1]), matchNumber(match[2]), matchNumber(match[3])
	offset, err := offsetMinutes(match[4], match[5], match[6])
	if err != nil {
		return Invalid("%q is not a time; %v", value, err)
	}
	if hour > 23 || minute > 59 || second > 60 {
		return Invalid("%q is not a time the clock has", value)
	}
	if second == 60 && wrapMinutes(hour*60+minute-offset) != endOfDayMinutes {
		return Invalid("%q claims a leap second at a moment the clock does not have one; a second of 60 belongs to 23:59:60 UTC", value)
	}
	return nil
}

// validateOffsetDateTime reports a timestamp whose date or clock the calendar does not have. The
// shape pattern already required an offset, so this only decides what the numbers mean.
func validateOffsetDateTime(_ context.Context, value string) error {
	date, clock, found := strings.Cut(value, "T")
	if !found {
		date, clock, found = strings.Cut(value, "t")
	}
	if !found {
		return Invalid("%q is not a timestamp; write it as RFC 3339, such as 1963-06-19T08:30:06Z", value)
	}
	if err := validateCalendarDate(context.Background(), date); err != nil {
		return Invalid("%q is not a timestamp; the date part is not a day the calendar has", value)
	}
	if err := validateClockTime(context.Background(), clock); err != nil {
		return Invalid("%q is not a timestamp; the time part is not a time the clock has", value)
	}
	return nil
}

// endOfDayMinutes is 23:59 as minutes past midnight, where a leap second belongs.
const endOfDayMinutes = 23*60 + 59

// matchNumber reads a two-digit group the pattern already matched, so a failure here is
// unreachable and a zero would be a silent lie.
func matchNumber(group string) int {
	number, err := strconv.Atoi(group)
	if err != nil {
		return 0
	}
	return number
}

// offsetMinutes converts the offset the pattern matched, in minutes east of UTC, and reports an
// offset no zone can have.
func offsetMinutes(sign, hours, minutes string) (int, error) {
	if sign == "" {
		return 0, nil // Z, which the pattern matched without a numeric offset
	}
	hour, err := strconv.Atoi(hours)
	if err != nil {
		return 0, err
	}
	minute, err := strconv.Atoi(minutes)
	if err != nil {
		return 0, err
	}
	if hour > 23 || minute > 59 {
		return 0, Invalid("its offset is outside the range a zone can have")
	}
	if sign == "-" {
		return -(hour*60 + minute), nil
	}
	return hour*60 + minute, nil
}

// wrapMinutes keeps a minute-of-day sum inside one day.
func wrapMinutes(minutes int) int {
	minutes %= 24 * 60
	if minutes < 0 {
		minutes += 24 * 60
	}
	return minutes
}
