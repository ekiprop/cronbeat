package main

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo

	"github.com/robfig/cron/v3"
)

// Standard 5-field crontab syntax plus descriptors like @daily and @hourly.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func parseSchedule(expr, tz string) (cron.Schedule, *time.Location, error) {
	loc := time.UTC
	if tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, nil, fmt.Errorf("unknown timezone %q", tz)
		}
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	if sched.Next(time.Now().In(loc)).IsZero() {
		return nil, nil, errors.New("cron expression never fires")
	}
	return sched, loc, nil
}

// expectedAfter is when the next ping is due, given a ping at t.
func (c Check) expectedAfter(t time.Time) time.Time {
	if c.Schedule == "" {
		return t.Add(c.Period)
	}
	sched, loc, err := parseSchedule(c.Schedule, c.TZ)
	if err != nil {
		// Schedules are validated on save; fall back rather than never alerting.
		return t.Add(c.Period)
	}
	return sched.Next(t.In(loc))
}

// Deadline is when an up check flips to down if no ping arrives.
func (c Check) Deadline() time.Time {
	if c.Status != "up" || c.LastPing.IsZero() {
		return time.Time{}
	}
	return c.expectedAfter(c.LastPing).Add(c.Grace)
}

// Describe renders the schedule for humans, e.g. "every 6h" or "0 3 * * * (Europe/Berlin)".
func (c Check) Describe() string {
	if c.Schedule == "" {
		return "every " + fmtDur(c.Period)
	}
	tz := c.TZ
	if tz == "" {
		tz = "UTC"
	}
	return fmt.Sprintf("%s (%s)", c.Schedule, tz)
}
