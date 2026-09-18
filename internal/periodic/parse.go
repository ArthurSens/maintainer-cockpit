// Package periodic coordinates cron-based external operations.
package periodic

import (
	"errors"
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Parse accepts exactly the standard five-field cron form. Schedules are
// evaluated in UTC by callers.
func Parse(expression string) (cron.Schedule, error) {
	if strings.TrimSpace(expression) != expression || expression == "" ||
		len(strings.Fields(expression)) != 5 {
		return nil, errors.New("must be a standard five-field UTC cron expression")
	}
	schedule, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("must be a standard five-field UTC cron expression: %w", err)
	}
	return schedule, nil
}
