package session

import "sync"

// outputSubscriber decouples PTY reads from a local attachment without
// discarding arbitrary chunks. Dropping a chunk can split an ANSI sequence and
// permanently corrupt the attached terminal's renderer. A client that falls
// more than one transcript window behind is disconnected cleanly instead.
type outputSubscriber struct {
	mu          sync.Mutex
	queue       [][]byte
	queuedBytes int
	wake        chan struct{}
	done        chan struct{}
	updates     chan []byte
	closed      bool
}

func newOutputSubscriber() *outputSubscriber {
	subscriber := &outputSubscriber{
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		updates: make(chan []byte),
	}
	go subscriber.run()
	return subscriber
}

func (s *outputSubscriber) enqueue(chunk []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.queuedBytes+len(chunk) > maxTranscriptBytes {
		return false
	}
	s.queue = append(s.queue, chunk)
	s.queuedBytes += len(chunk)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

func (s *outputSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
}

func (s *outputSubscriber) run() {
	defer close(s.updates)
	for {
		chunk, ok := s.dequeue()
		if ok {
			select {
			case s.updates <- chunk:
				s.delivered(len(chunk))
			case <-s.done:
				return
			}
			continue
		}
		select {
		case <-s.wake:
		case <-s.done:
			return
		}
	}
}

func (s *outputSubscriber) dequeue() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil, false
	}
	chunk := s.queue[0]
	s.queue[0] = nil
	s.queue = s.queue[1:]
	return chunk, true
}

func (s *outputSubscriber) delivered(size int) {
	s.mu.Lock()
	s.queuedBytes -= size
	s.mu.Unlock()
}
