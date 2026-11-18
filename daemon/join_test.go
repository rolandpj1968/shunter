package daemon_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
)

// waiting is a state whose waiting lines have these many changes each, in
// order, named l1, l2, ...; a negative count is a line that must ride
// alone.
func waiting(t *testing.T, sizes ...int) state.State {
	t.Helper()
	s := state.New("T0", state.Config{Depth: 1})
	for i, n := range sizes {
		id := state.LineID(fmt.Sprintf("l%d", i+1))
		o := state.Offer{Line: id, Alone: n < 0}
		for j := 0; j < max(n, -n); j++ {
			o.Changes = append(o.Changes, state.Change{ID: state.ChangeID(fmt.Sprintf("c%d-%d", i+1, j)), Head: "h"})
		}
		for _, ev := range []state.Event{o, state.GateAccept{Line: id}} {
			next, _, err := state.Next(s, ev)
			if err != nil {
				t.Fatal(err)
			}
			s = next
		}
	}
	return s
}

func TestJoinCapsChangesAsWellAsLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes []int
		caps  daemon.Caps
		want  []state.LineID
	}{
		{"lines only", []int{1, 3, 1, 1}, daemon.Caps{Lines: 3}, []state.LineID{"l1", "l2", "l3"}},
		{"no cap", []int{1, 3, 1, 1}, daemon.Caps{}, []state.LineID{"l1", "l2", "l3", "l4"}},
		// 1 + 3 = 4; l3 (2) would make 6: skipped; l4 (1) makes 5.
		{"changes cap skips a line past it", []int{1, 3, 2, 1}, daemon.Caps{Lines: 8, Changes: 5}, []state.LineID{"l1", "l2", "l4"}},
		{"changes cap exactly", []int{2, 3, 1}, daemon.Caps{Lines: 8, Changes: 5}, []state.LineID{"l1", "l2"}},
		{"both caps, lines first", []int{1, 1, 1, 1}, daemon.Caps{Lines: 2, Changes: 10}, []state.LineID{"l1", "l2"}},
		{"a line bigger than the cap rides alone", []int{6, 1}, daemon.Caps{Lines: 8, Changes: 5}, []state.LineID{"l1"}},
		{"a line at the cap rides alone", []int{5, 1}, daemon.Caps{Lines: 8, Changes: 5}, []state.LineID{"l1"}},
		{"a big line later is skipped", []int{1, 6, 1}, daemon.Caps{Lines: 8, Changes: 5}, []state.LineID{"l1", "l3"}},
		{"alone still first", []int{-1, 1}, daemon.Caps{Lines: 8, Changes: 5}, []state.LineID{"l1"}},
	} {
		if got := daemon.Join(waiting(t, tc.sizes...), tc.caps); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
