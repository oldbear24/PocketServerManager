package panel

import (
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/tools/cron"
)

// cronRule is a schedule read once: PocketBase's, or, when both the day of
// the month and the day of the week are restricted, two of them, either of
// which is enough, as cron has it.
type cronRule []*cron.Schedule

// parseCronRule reads a schedule; nil when it is not one.
func parseCronRule(expr string) cronRule {
	fields := strings.Fields(expr)
	if len(fields) == 5 && fields[2] != "*" && fields[4] != "*" {
		byDay, err1 := cron.NewSchedule(strings.Join([]string{fields[0], fields[1], fields[2], fields[3], "*"}, " "))
		byWeekday, err2 := cron.NewSchedule(strings.Join([]string{fields[0], fields[1], "*", fields[3], fields[4]}, " "))
		if err1 != nil || err2 != nil {
			return nil
		}
		return cronRule{byDay, byWeekday}
	}
	schedule, err := cron.NewSchedule(expr)
	if err != nil {
		return nil
	}
	return cronRule{schedule}
}

// due says whether the rule is due at a moment.
func (r cronRule) due(moment *cron.Moment) bool {
	for _, schedule := range r {
		if schedule.IsDue(moment) {
			return true
		}
	}
	return false
}

// any says whether one of the rule's schedules has a value in a field.
func (r cronRule) any(field func(*cron.Schedule) map[int]struct{}, value int) bool {
	for _, schedule := range r {
		if _, ok := field(schedule)[value]; ok {
			return true
		}
	}
	return false
}

// dayDue says whether one of the rule's schedules runs on a day.
func (r cronRule) dayDue(moment *cron.Moment) bool {
	for _, schedule := range r {
		_, day := schedule.Days[moment.Day]
		_, weekday := schedule.DaysOfWeek[moment.DayOfWeek]
		if day && weekday {
			return true
		}
	}
	return false
}

// nextRunHorizon is how far ahead the next run of a schedule is looked for:
// one that never comes, as on the 30th of February, has none.
const nextRunHorizon = 366 * 24 * time.Hour

// next is the first minute after a moment that the rule is due, in UTC; zero
// when there is none within nextRunHorizon. A month, a day or an hour that
// the rule never runs in is passed by whole.
func (r cronRule) next(after time.Time) time.Time {
	if len(r) == 0 {
		return time.Time{}
	}
	at := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := at.Add(nextRunHorizon)
	for at.Before(limit) {
		moment := cron.NewMoment(at)
		switch {
		case !r.any(func(s *cron.Schedule) map[int]struct{} { return s.Months }, moment.Month):
			at = time.Date(at.Year(), at.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !r.dayDue(moment):
			at = time.Date(at.Year(), at.Month(), at.Day()+1, 0, 0, 0, 0, time.UTC)
		case !r.any(func(s *cron.Schedule) map[int]struct{} { return s.Hours }, moment.Hour):
			at = at.Truncate(time.Hour).Add(time.Hour)
		case r.due(moment):
			return at
		default:
			at = at.Add(time.Minute)
		}
	}
	return time.Time{}
}
