package models

// DefaultStatuses returns the seed rows for the statuses reference table.
func DefaultStatuses() []*Status {
	return []*Status{
		{ID: StatusInQueue, Description: "In Queue"},
		{ID: StatusProcessing, Description: "Processing"},
		{ID: StatusAccepted, Description: "Accepted"},
		{ID: StatusWrongAnswer, Description: "Wrong Answer"},
		{ID: StatusTimeLimitExceeded, Description: "Time Limit Exceeded"},
		{ID: StatusCompilationError, Description: "Compilation Error"},
		{ID: StatusRuntimeError, Description: "Runtime Error (SIGSEGV)"},
		{ID: StatusInternalError, Description: "Internal Error"},
	}
}
