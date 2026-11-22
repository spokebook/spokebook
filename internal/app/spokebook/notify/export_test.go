package notify

import "time"

// SetSendTimeout sets how long a check may take sending and returns a
// function that restores it.
func SetSendTimeout(d time.Duration) func() {
	old := sendTimeout
	sendTimeout = d
	return func() { sendTimeout = old }
}
