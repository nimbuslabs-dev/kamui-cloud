package worker

import (
	"fmt"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
)

// ValidatePolicy enforces the resource-sharing contract's invariants. Unknown
// values must stay unknown; the policy never invents capacity.
func ValidatePolicy(policy core.WorkerPolicy) error {
	if policy.CPUMillisMax < 0 || policy.CPUMillisMax > 1_024_000 {
		return fmt.Errorf("%w: CPU allocation must be between 0 and 1,024,000 millicores", ErrBadInput)
	}
	if policy.MemoryBytesMax < 0 || policy.MemoryBytesMax > 1<<40 {
		return fmt.Errorf("%w: memory allocation must be between 0 and 1 TiB", ErrBadInput)
	}
	if policy.StorageBytesMax < 0 || policy.StorageBytesMax > 64<<40 {
		return fmt.Errorf("%w: storage allocation must be between 0 and 64 TiB", ErrBadInput)
	}
	if policy.MaxConcurrent < 0 || policy.MaxConcurrent > 64 {
		return fmt.Errorf("%w: concurrent workload limit must be between 0 and 64", ErrBadInput)
	}
	for _, kind := range policy.WorkloadKinds {
		if kind != "job" && kind != "service" {
			return fmt.Errorf("%w: workload kinds must be job or service", ErrBadInput)
		}
	}
	if len(policy.Projects) > 64 || len(policy.Users) > 64 {
		return fmt.Errorf("%w: at most 64 projects and users are allowed", ErrBadInput)
	}
	for _, project := range policy.Projects {
		if project == "" || len(project) > 64 || strings.TrimSpace(project) != project {
			return fmt.Errorf("%w: project identifiers must be non-empty", ErrBadInput)
		}
	}
	for _, user := range policy.Users {
		if user == "" || len(user) > 64 || strings.TrimSpace(user) != user {
			return fmt.Errorf("%w: user identifiers must be non-empty", ErrBadInput)
		}
	}
	if len(policy.Schedule) > 32 {
		return fmt.Errorf("%w: at most 32 schedule windows are allowed", ErrBadInput)
	}
	if policy.Timezone != "" {
		if _, err := time.LoadLocation(policy.Timezone); err != nil {
			return fmt.Errorf("%w: timezone must be a valid IANA name", ErrBadInput)
		}
	}
	for _, window := range policy.Schedule {
		if len(window.Days) > 7 {
			return fmt.Errorf("%w: schedule windows accept at most 7 weekdays", ErrBadInput)
		}
		seen := map[int]bool{}
		for _, day := range window.Days {
			if day < 0 || day > 6 || seen[day] {
				return fmt.Errorf("%w: schedule days must be unique weekdays 0 (Sunday) through 6 (Saturday)", ErrBadInput)
			}
			seen[day] = true
		}
		if _, ok := core.ParseClock(window.Start); !ok {
			return fmt.Errorf("%w: schedule start must be HH:MM", ErrBadInput)
		}
		if _, ok := core.ParseClock(window.End); !ok {
			return fmt.Errorf("%w: schedule end must be HH:MM", ErrBadInput)
		}
	}
	return nil
}
