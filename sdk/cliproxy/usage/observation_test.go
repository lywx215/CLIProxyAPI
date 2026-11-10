package usage

import (
	"context"
	"testing"
)

type observationPluginFunc func(context.Context, Record)

func (f observationPluginFunc) HandleUsage(ctx context.Context, r Record) { f(ctx, r) }

func TestObservationIsFrozenAcrossQueueAndPlugins(t *testing.T) {
	m := NewManager(1)
	defer m.Stop()
	entered, release := make(chan struct{}), make(chan struct{})
	received := make(chan *Observation, 1)
	m.Register(observationPluginFunc(func(_ context.Context, r Record) {
		close(entered)
		<-release
		r.Observation.ResponseModelSequence[0] = "plugin-mutated"
	}))
	m.Register(observationPluginFunc(func(_ context.Context, r Record) { received <- r.Observation }))
	o := &Observation{Version: 1, RequestModel: "m", ResponseModelSequence: []string{"original"}}
	m.Publish(context.Background(), Record{Observation: o})
	<-entered
	o.ResponseModelSequence[0] = "caller-mutated"
	close(release)
	if got := <-received; got.ResponseModelSequence[0] != "original" {
		t.Fatalf("mutable observation escaped: %+v", got)
	}
}
