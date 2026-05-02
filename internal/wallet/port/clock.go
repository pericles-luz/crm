package port

import "time"

// Clock isolates time so tests do not block on real time.Sleep / time.Now.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// SystemClock satisfies Clock with the real time package.
type SystemClock struct{}

func (SystemClock) Now() time.Time        { return time.Now() }
func (SystemClock) Sleep(d time.Duration) { time.Sleep(d) }
