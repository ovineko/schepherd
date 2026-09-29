package cache

import "time"

// transientBudget bounds how long an operation is retried while it fails
// with a transient error.
const transientBudget = 2 * time.Second

// retryTransient runs op until it succeeds, fails with an error that is not
// transient, or transientBudget has passed.
func retryTransient(op func() error) error {
	deadline := time.Now().Add(transientBudget)

	for delay := time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
		err := op()
		if err == nil || !transient(err) || time.Now().After(deadline) {
			return err
		}

		time.Sleep(delay)
	}
}
