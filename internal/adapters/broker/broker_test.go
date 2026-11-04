package broker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func jobUpdate(id domain.JobID, v int64) jobs.Update {
	return jobs.Update{JobID: id, Job: &jobs.Job{ID: id, Version: v}}
}

func progress(id domain.JobID, b int64) jobs.Update {
	return jobs.Update{JobID: id, Progress: &jobs.Sample{JobID: id, Stats: jobs.Stats{Bytes: b}}}
}

func describe(u jobs.Update) string {
	if u.Job != nil {
		return "job v" + string(rune('0'+u.Job.Version))
	}
	return "progress " + string(rune('0'+u.Progress.Stats.Bytes))
}

func TestBrokerOrderAndCoalescing(t *testing.T) {
	t.Parallel()
	b := broker.New()
	id, other := domain.NewJobID(), domain.NewJobID()
	b.Publish(jobUpdate(id, 1)) // before subscribing: not delivered
	sub := b.Subscribe(id)
	defer sub.Close()

	for _, u := range []jobs.Update{
		jobUpdate(id, 2), progress(id, 1), progress(id, 2), progress(other, 9),
		jobUpdate(id, 3), progress(id, 3),
	} {
		b.Publish(u)
	}
	want := []string{"job v2", "progress 2", "job v3", "progress 3"}
	ctx := context.Background()
	for _, w := range want {
		u, err := sub.Next(ctx)
		if err != nil || describe(u) != w {
			t.Fatalf("Next = (%s, %v), want %s", describe(u), err, w)
		}
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := sub.Next(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Next on an empty queue = %v, want DeadlineExceeded", err)
	}
}

func TestBrokerWakesBlockedReader(t *testing.T) {
	t.Parallel()
	b := broker.New()
	id := domain.NewJobID()
	sub := b.Subscribe(id)
	defer sub.Close()
	got := make(chan jobs.Update, 1)
	go func() {
		u, _ := sub.Next(context.Background())
		got <- u
	}()
	b.Publish(jobUpdate(id, 4))
	select {
	case u := <-got:
		if u.Job == nil || u.Job.Version != 4 {
			t.Errorf("got %+v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked reader was not woken")
	}
}

func TestBrokerClose(t *testing.T) {
	t.Parallel()
	b := broker.New()
	id := domain.NewJobID()
	sub := b.Subscribe(id)
	other := b.Subscribe(id)
	defer other.Close()
	if n := b.Subscribers(id); n != 2 {
		t.Fatalf("Subscribers = %d", n)
	}
	sub.Close()
	sub.Close() // idempotent
	if n := b.Subscribers(id); n != 1 {
		t.Errorf("Subscribers after Close = %d", n)
	}
	if _, err := sub.Next(context.Background()); err == nil {
		t.Error("Next after Close returned no error")
	}
	b.Publish(jobUpdate(id, 1)) // must not reach the closed subscription
}

func TestBrokerBoundsAStalledReader(t *testing.T) {
	t.Parallel()
	b := broker.New()
	id := domain.NewJobID()
	sub := b.Subscribe(id)
	defer sub.Close()
	for v := range int64(300) {
		b.Publish(jobUpdate(id, v))
	}
	u, err := sub.Next(context.Background())
	if err != nil || u.Job.Version != 300-256 {
		t.Errorf("first update = (%+v, %v), want version %d (oldest dropped)", u.Job, err, 300-256)
	}
}
