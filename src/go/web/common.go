package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/app"
	"phenix/internal/mm"
	"phenix/types"
	"phenix/util/notes"
	"phenix/web/broker"
	"phenix/web/cache"
	"phenix/web/util"
	"phenix/web/weberror"

	log "github.com/activeshadow/libminimega/minilog"
)

var (
	// Track context cancelers and wait groups for periodically running apps.
	cancelers = make(map[string][]context.CancelFunc)
	waiters   = make(map[string]*sync.WaitGroup)
)

func startExperiment(name string) ([]byte, error) {
	if err := cache.LockExperimentForStarting(name); err != nil {
		err := weberror.NewWebError(err, "unable to lock experiment %s for starting", name)
		return nil, err.SetStatus(http.StatusConflict)
	}

	defer cache.UnlockExperiment(name)

	broker.Broadcast(
		broker.NewRequestPolicy("experiments/start", "update", name),
		broker.NewResource("experiment", name, "starting"),
		nil,
	)

	type result struct {
		exp *types.Experiment
		err error
	}

	status := make(chan result)

	go func() {
		// We don't want to use the HTTP request's context here.
		ctx, cancel := context.WithCancel(context.Background())
		cancelers[name] = append(cancelers[name], cancel)

		ctx = notes.Context(ctx, false)

		ch := make(chan error)

		if err := experiment.Start(ctx, experiment.StartWithName(name), experiment.StartWithErrorChannel(ch)); err != nil {
			cancel() // avoid leakage
			delete(cancelers, name)

			status <- result{nil, err}
		} else {
			for _, note := range notes.Info(ctx, false) {
				log.Info(note)
			}

			done := make(chan struct{})

			// Goroutine to periodically print out logs generated by experiment while
			// starting.
			go func() {
				for {
					for _, note := range notes.Info(ctx, false) {
						log.Info(note)
					}

					select {
					case <-done:
						return
					default:
						time.Sleep(1 * time.Second)
					}
				}
			}()

			go func() {
				for err := range ch {
					log.Warn("delayed error starting experiment %s: %v", name, err)

					var delayErr experiment.DelayedVMError

					if errors.As(err, &delayErr) {
						broker.Broadcast(
							broker.NewRequestPolicy("experiments/start", "update", name),
							broker.NewResource("experiment/vm", fmt.Sprintf("%s/%s", name, delayErr.VM), "error"),
							json.RawMessage(fmt.Sprintf(`{"error": "unable to start delayed VM %s"}`, delayErr.VM)),
						)
					}
				}

				// Stop periodically printing out logs via previous Goroutine.
				close(done)
			}()
		}

		exp, err := experiment.Get(name)

		status <- result{exp, err}
	}()

	var progress float64
	count, _ := vm.Count(name)

	for {
		select {
		case s := <-status:
			if s.err != nil {
				broker.Broadcast(
					broker.NewRequestPolicy("experiments/start", "update", name),
					broker.NewResource("experiment", name, "errorStarting"),
					nil,
				)

				err := weberror.NewWebError(s.err, "unable to start experiment %s", name)
				return nil, err.SetStatus(http.StatusBadRequest)
			}

			// We don't want to use the HTTP request's context here.
			ctx, cancel := context.WithCancel(context.Background())
			cancelers[name] = append(cancelers[name], cancel)

			var wg sync.WaitGroup
			waiters[name] = &wg

			if err := app.PeriodicallyRunApps(ctx, &wg, s.exp); err != nil {
				cancel() // avoid leakage
				delete(cancelers, name)
				delete(waiters, name)

				fmt.Printf("Error scheduling experiment apps to run periodically: %v\n", err)
			}

			vms, err := vm.List(name)
			if err != nil {
				// TODO
				log.Error("listing VMs in experiment %s - %v", name, err)
			}

			body, err := marshaler.Marshal(util.ExperimentToProtobuf(*s.exp, "", vms))
			if err != nil {
				err := weberror.NewWebError(err, "unable to start experiment %s", name)
				return nil, err.SetStatus(http.StatusInternalServerError)
			}

			broker.Broadcast(
				broker.NewRequestPolicy("experiments/start", "update", name),
				broker.NewResource("experiment", name, "start"),
				body,
			)

			return body, nil
		default:
			p, err := mm.GetLaunchProgress(name, count)
			if err != nil {
				log.Error("getting progress for experiment %s - %v", name, err)
				continue
			}

			if p > progress {
				progress = p
			}

			log.Info("percent deployed: %v", progress*100.0)

			status := map[string]interface{}{
				"percent": progress,
			}

			marshalled, _ := json.Marshal(status)

			broker.Broadcast(
				broker.NewRequestPolicy("experiments/start", "update", name),
				broker.NewResource("experiment", name, "progress"),
				marshalled,
			)

			time.Sleep(2 * time.Second)
		}
	}
}

func stopExperiment(name string) ([]byte, error) {
	if err := cache.LockExperimentForStopping(name); err != nil {
		err := weberror.NewWebError(err, "unable to lock experiment %s for stopping", name)
		return nil, err.SetStatus(http.StatusConflict)
	}

	defer cache.UnlockExperiment(name)

	broker.Broadcast(
		broker.NewRequestPolicy("experiments/stop", "update", name),
		broker.NewResource("experiment", name, "stopping"),
		nil,
	)

	if cancels, ok := cancelers[name]; ok {
		for _, cancel := range cancels {
			cancel()
		}

		if wg, ok := waiters[name]; ok {
			wg.Wait()
		}
	}

	delete(cancelers, name)
	delete(waiters, name)

	if err := experiment.Stop(name); err != nil {
		broker.Broadcast(
			broker.NewRequestPolicy("experiments/stop", "update", name),
			broker.NewResource("experiment", name, "errorStopping"),
			nil,
		)

		err := weberror.NewWebError(err, "unable to stop experiment %s", name)
		return nil, err.SetStatus(http.StatusBadRequest)
	}

	exp, err := experiment.Get(name)
	if err != nil {
		// TODO
	}

	vms, err := vm.List(name)
	if err != nil {
		// TODO
	}

	body, err := marshaler.Marshal(util.ExperimentToProtobuf(*exp, "", vms))
	if err != nil {
		err := weberror.NewWebError(err, "unable to stop experiment %s", name)
		return nil, err.SetStatus(http.StatusInternalServerError)
	}

	broker.Broadcast(
		broker.NewRequestPolicy("experiments/stop", "update", name),
		broker.NewResource("experiment", name, "stop"),
		body,
	)

	return body, nil
}
