package prometheus

// IncRateLimited counts one refusal, or one failure to judge.
//
// Deliberately NOT labelled by client address. Vozko labels its equivalent with
// the IP because a shared-NAT office colliding on one budget is the signal it
// wants; here the limiters are keyed per account and per email as well as per
// address, so an address label would carry buyer-identifying data into a metric
// store that has no business holding it, and the cardinality would be every
// address that ever hit a limit. Which limiter and why is enough to act on.
func (s *Service) IncRateLimited(limiter, reason string) {
	if s == nil || s.rateLimited == nil {
		return
	}
	s.rateLimited.WithLabelValues(safeLabel(limiter), safeLabel(reason)).Inc()
}
