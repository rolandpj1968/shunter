package store

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/rolandpj1968/shunter/state"
)

// Envelope is an event as it is written: its name and its fields.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// events lists every event of the state machine, by the name state.Name gives
// it. An event missing here cannot be written; TestCodecCoversEveryEvent
// keeps the list whole.
var events = map[string]reflect.Type{}

func init() {
	for _, e := range []state.Event{
		state.Offer{}, state.GateAccept{}, state.GateRefuse{}, state.Hold{}, state.Release{},
		state.ParkLines{}, state.Restore{}, state.Withdraw{}, state.Assemble{}, state.LeaveOut{},
		state.PreflightFailed{}, state.Built{}, state.Opened{}, state.CIPassed{}, state.CIRerun{},
		state.CINoVerdict{}, state.CIFailed{}, state.Judge{}, state.JudgeTimeout{}, state.HeadMoved{},
		state.Abort{}, state.LandingVerified{}, state.PushOK{}, state.PushRefused{}, state.Pause{},
		state.Resume{}, state.ExpireBudget{}, state.Stale{}, state.Fresh{}, state.LineChecked{}, state.HandOver{}, state.RebaseFailed{},
		state.CheckClaimed{}, state.ClaimExpired{}, state.ClaimReleased{}, state.Prioritise{}, state.PriorityEnded{}, state.DraftOpened{}, state.Starving{}, state.CheckerChanged{},
	} {
		events[state.Name(e)] = reflect.TypeOf(e)
	}
}

// Encode writes an event as an envelope.
func Encode(e state.Event) (Envelope, error) {
	name := state.Name(e)
	if t, ok := events[name]; !ok || t != reflect.TypeOf(e) {
		return Envelope{}, fmt.Errorf("store: event %T has no codec", e)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: name, Data: b}, nil
}

// Decode reads an envelope back into its event.
func Decode(env Envelope) (state.Event, error) {
	t, ok := events[env.Type]
	if !ok {
		return nil, fmt.Errorf("store: unknown event %q", env.Type)
	}
	v := reflect.New(t)
	if err := json.Unmarshal(env.Data, v.Interface()); err != nil {
		return nil, fmt.Errorf("store: event %q: %w", env.Type, err)
	}
	return v.Elem().Interface().(state.Event), nil
}
