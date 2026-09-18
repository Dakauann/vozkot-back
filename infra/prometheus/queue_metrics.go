package prometheus

func (s *Service) SetQueueJobs(status string, count int64) {
	if s == nil || s.queueJobs == nil {
		return
	}
	s.queueJobs.WithLabelValues(safeLabel(status)).Set(float64(count))
}

func (s *Service) IncJobParked(jobType string) {
	if s == nil || s.jobsParked == nil {
		return
	}
	s.jobsParked.WithLabelValues(safeLabel(jobType)).Inc()
}

func (s *Service) SetOrdersByStatus(status string, count int64) {
	if s == nil || s.ordersByStatus == nil {
		return
	}
	s.ordersByStatus.WithLabelValues(safeLabel(status)).Set(float64(count))
}
