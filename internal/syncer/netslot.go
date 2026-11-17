package syncer

import (
	"context"
	"sync"
)

// NetSlot lets one network transfer run at a time. Priority waiters (the streamer) are served
// before normal ones (the sync runner, which takes it per file).
type NetSlot struct {
	mu         sync.Mutex
	held       bool
	prio, norm []chan struct{}
}

func NewNetSlot() *NetSlot { return &NetSlot{} }

// Net is the slot the sync runner and the streamer share.
var Net = NewNetSlot()

func (s *NetSlot) Acquire(ctx context.Context, priority bool) error {
	s.mu.Lock()
	if !s.held {
		s.held = true
		s.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	if priority {
		s.prio = append(s.prio, ch)
	} else {
		s.norm = append(s.norm, ch)
	}
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		if !drop(&s.prio, ch) && !drop(&s.norm, ch) {
			s.handOff() // granted while giving up: pass it on
		}
		return ctx.Err()
	}
}

// PriorityWaiting reports whether a priority acquirer is queued.
func (s *NetSlot) PriorityWaiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prio) > 0
}

func (s *NetSlot) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handOff()
}

func (s *NetSlot) handOff() {
	for _, q := range []*[]chan struct{}{&s.prio, &s.norm} {
		if len(*q) > 0 {
			close((*q)[0])
			*q = (*q)[1:]
			return
		}
	}
	s.held = false
}

func drop(q *[]chan struct{}, ch chan struct{}) bool {
	for i, c := range *q {
		if c == ch {
			*q = append((*q)[:i], (*q)[i+1:]...)
			return true
		}
	}
	return false
}
