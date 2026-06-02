package models

// Status ID constants — these map to the statuses reference table.
const (
	StatusInQueue           = 1
	StatusProcessing        = 2
	StatusAccepted          = 3
	StatusWrongAnswer       = 4
	StatusTimeLimitExceeded = 5
	StatusCompilationError  = 6
	StatusRuntimeError      = 7
	StatusInternalError     = 8
)

// StatusDescriptions maps status IDs to human-readable descriptions.
var StatusDescriptions = map[int]string{
	StatusInQueue:           "In Queue",
	StatusProcessing:        "Processing",
	StatusAccepted:          "Accepted",
	StatusWrongAnswer:       "Wrong Answer",
	StatusTimeLimitExceeded: "Time Limit Exceeded",
	StatusCompilationError:  "Compilation Error",
	StatusRuntimeError:      "Runtime Error (SIGSEGV)",
	StatusInternalError:     "Internal Error",
}

// IsTerminalStatus returns true if the given status ID indicates a final state.
func IsTerminalStatus(statusID int) bool {
	return statusID >= StatusAccepted
}

// IsErrorStatus returns true if the status indicates an error condition.
func IsErrorStatus(statusID int) bool {
	return statusID >= StatusWrongAnswer
}
