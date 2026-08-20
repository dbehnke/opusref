package main

import (
	"context"
	"testing"
	"time"
)

func TestSignalWaitsForRestrictedDrainCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runErrors := make(chan error)
	done := make(chan error, 1)
	go func() { done <- waitForTermination(ctx, cancel, nil, runErrors) }()
	cancel()
	select {
	case <-done:
		t.Fatal("returned before reflector drain completed")
	case <-time.After(25 * time.Millisecond):
	}
	runErrors <- nil
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not return after reflector drain completed")
	}
}
