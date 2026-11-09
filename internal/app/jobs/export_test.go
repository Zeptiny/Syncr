package jobs

// ThrottledJobs returns the number of jobs the sample rate limiter keeps
// an entry for (tests only).
func ThrottledJobs(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lastStored)
}
