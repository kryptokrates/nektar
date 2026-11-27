package memory

import (
	"time"

	"github.com/kryptokrates/nektar/internal/policy"
)

// TouchActivity records the latest user/agent memory interaction timestamp for SleepCycle idle detection.
func (s *Service) TouchActivity() {
	if s == nil {
		return
	}
	s.lastActivityUnix.Store(time.Now().Unix())
}

// LastActivityUnix returns the Unix timestamp of the most recent memory write or recall (0 if none).
func (s *Service) LastActivityUnix() int64 {
	if s == nil {
		return 0
	}
	return s.lastActivityUnix.Load()
}

// IdleSeconds returns how many seconds have elapsed since the last memory activity.
func (s *Service) IdleSeconds() int64 {
	last := s.LastActivityUnix()
	if last <= 0 {
		return 0
	}
	diff := time.Now().Unix() - last
	if diff < 0 {
		return 0
	}
	return diff
}

// AuthorizeSell evaluates Action::"sell" under the active Cedar policy set, enforcing
// resource.distilled == true unless explicit operator allowRawOverride is set.
func (s *Service) AuthorizeSell(principal policy.Principal, resource policy.Resource, allowRawOverride bool) (policy.Decision, error) {
	s.mu.RLock()
	pol := s.policyText
	s.mu.RUnlock()
	return policy.AuthorizeSell(pol, principal, resource, allowRawOverride)
}
