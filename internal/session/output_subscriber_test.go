package session

import (
	"bytes"
	"testing"
	"time"
)

func TestOutputSubscriberPreservesChunkOrderAndBytes(t *testing.T) {
	subscriber := newOutputSubscriber()
	defer subscriber.close()
	chunks := [][]byte{
		[]byte("\x1b["),
		[]byte("31mred"),
		[]byte("\x1b[0m"),
	}
	for _, chunk := range chunks {
		if !subscriber.enqueue(chunk) {
			t.Fatal("subscriber rejected output")
		}
	}

	for index, expected := range chunks {
		select {
		case got := <-subscriber.updates:
			if !bytes.Equal(got, expected) {
				t.Fatalf("chunk %d = %q, want %q", index, got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for chunk %d", index)
		}
	}
}

func TestOutputSubscriberRejectsLagInsteadOfDroppingAChunk(t *testing.T) {
	subscriber := newOutputSubscriber()
	defer subscriber.close()
	if !subscriber.enqueue(make([]byte, maxTranscriptBytes)) {
		t.Fatal("subscriber rejected output within its backlog limit")
	}
	if subscriber.enqueue([]byte("would be silently dropped")) {
		t.Fatal("subscriber accepted output beyond its backlog limit")
	}
}
