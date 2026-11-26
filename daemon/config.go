package daemon

import (
	"time"

	"github.com/rolandpj1968/shunter/tier"
)

// Config bounds the daemon. Every wait has a deadline and a default
// (PROTOCOL.md §2.5: silence is safe). Zero values take the defaults below.
type Config struct {
	// Tiers are the rules of the gate's tier classifier (§5.4). With none,
	// every line is full.
	Tiers tier.Rules

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
	// Review is how long a hold may stand before the judge is asked to
	// look at it again, and again each time that long passes. A hold has
	// no deadline and is never released by a clock: this is an alert, not
	// a release. Default 24h.
	Review time.Duration
	// Owner is how long a charged line may stay with its owner, not offered
	// again, before the judge is told: the owner may have gone silent, and
	// the line may need handing over (§7). An alert only. Default 12h.
	Owner time.Duration
	// Heartbeats is a directory with one file a producer, named as the
	// producer offers lines, whose modification time is its last sign of
	// life (§7). An owner whose file is missing, or older than Alive, is
	// not live, and its line counts as having no owner. Empty: every
	// owner counts as live.
	Heartbeats string
	// Alive is how old a heartbeat may be. Default 30m.
	Alive time.Duration
	// Ownerless is how long a line may wait on an owner that is not live
	// before the judge is told, and again each Owner that passes. An alert
	// only. Default 1h.
	Ownerless time.Duration
	// Claim is how long a checker's claim on a line check stands (§5.4),
	// unless the claim names its own lease. At its end the line may be
	// claimed again, and the judge is told. Default 30m.
	Claim time.Duration
	// Unanswered is how long a line whose checker gave its claim up with no
	// verdict (checked --action infra) is not given to a checker that asks
	// for any line. Pacing only: a named claim takes it. Default 10m.
	Unanswered time.Duration

	// Drafts shows each train from assembly as a draft pull request,
	// promoted when its pre-flight passes (§5.4). It needs a forge that is
	// an adapter.Drafter; with any other, it does nothing.
	Drafts bool

	// StarveCycles and StarveAfter are the starvation thresholds
	// (docs/queue-as-bag.md §4): a waiting line whose count reaches
	// StarveCycles, or whose queue time reaches StarveAfter, is starving and
	// rides first, at the head of the next train. StarveAlert starving lines
	// at once are raised to the judge (J4). Zero turns each off.
	StarveCycles int
	StarveAfter  time.Duration
	StarveAlert  int
	// StarveAlone is how many failed trains since its last offer that
	// blame could not clear it from make a starving line ride alone
	// (Caps.StarveAlone). Zero is never.
	StarveAlone int

	// MaxLines caps the lines of one train (J4's bounds). Default 8.
	MaxLines int
	// MaxChanges caps the changes of one train, every change of every
	// line counted. Zero is no cap.
	MaxChanges int
	// Retries is how many times a transient failure of an effect is
	// retried before it is escalated to J3. Default 3. A train's own
	// effects (its build, its pull request, its rerun) are not bounded by
	// it: they are retried until the train's deadline (Build, Open, CI),
	// which then ends the train with no verdict (PROTOCOL.md §5.5).
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
	d(&c.Review, 24*time.Hour)
	d(&c.Owner, 12*time.Hour)
	d(&c.Alive, 30*time.Minute)
	d(&c.Ownerless, time.Hour)
	d(&c.Claim, 30*time.Minute)
	d(&c.Unanswered, 10*time.Minute)
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
