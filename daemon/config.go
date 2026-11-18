package daemon

import "time"

// Config bounds the daemon. Every wait has a deadline and a default
// (PROTOCOL.md §2.5: silence is safe). Zero values take the defaults below.
type Config struct {
	// Version is recorded in the store at every start (§7).
	Version string
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	// Tick is how often timers and CI are looked at. Default 5s.
	Tick time.Duration
	// Poll is how often a testing train's CI is read. Default 30s.
	Poll time.Duration
	// Call bounds one adapter call that is not a build or a push.
	// Default 2m.
	Call time.Duration

	// Build bounds assembly and pre-flight; past it the train is void with
	// no verdict (T2's default, "void at the builder's deadline").
	// Default 30m.
	Build time.Duration
	// Open bounds a built train waiting for its pull request (T6's
	// default). Default 10m.
	Open time.Duration
	// CI bounds a train in testing (T7's default, "no verdict at the
	// deadline"). Default 90m.
	CI time.Duration
	// Push bounds one push (§5.1 "landing": bounded by the push's
	// deadline). Default 5m.
	Push time.Duration
	// J1 is J1's deadline, after which its default applies. PROTOCOL.md
	// §6, J1: 10 minutes.
	J1 time.Duration
	// Park is the deadline of a park whose condition the runner cannot
	// test (§6.4): it is restored then, with an alert. Default 24h.
	Park time.Duration

	// MaxLines caps the lines of one train (J4's bounds). Default 8.
	MaxLines int
	// Retries is how many times a transient failure of an effect is
	// retried before it is escalated to J3. Default 3.
	Retries int
	// Backoff is the first retry's wait; it doubles, to at most 5m.
	// Default 5s.
	Backoff time.Duration
}

func (c Config) withDefaults() Config {
	d := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	d(&c.Tick, 5*time.Second)
	d(&c.Poll, 30*time.Second)
	d(&c.Call, 2*time.Minute)
	d(&c.Build, 30*time.Minute)
	d(&c.Open, 10*time.Minute)
	d(&c.CI, 90*time.Minute)
	d(&c.Push, 5*time.Minute)
	d(&c.J1, 10*time.Minute)
	d(&c.Park, 24*time.Hour)
	d(&c.Backoff, 5*time.Second)
	if c.MaxLines <= 0 {
		c.MaxLines = 8
	}
	if c.Retries <= 0 {
		c.Retries = 3
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
