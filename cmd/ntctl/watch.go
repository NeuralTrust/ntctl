package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/doctor"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/report"
)

// Defaults for watching an install or upgrade while Helm waits.
const (
	watchEvery  = 20 * time.Second
	watchGiveUp = 4 * time.Minute
)

// waitWatch reports what an install or upgrade is waiting for, while Helm
// waits, and can stop the wait once a failure that waiting does not fix has
// lasted giveUp, instead of letting it run to the timeout.
type waitWatch struct {
	cs      kubernetes.Interface
	cluster *detect.Cluster
	ns      string
	release string
	out     io.Writer
	every   time.Duration
	giveUp  time.Duration
	// stopEarly cancels stop (Helm's wait) on a lasting hopeless failure.
	stopEarly bool
}

// watching is a running waitWatch.
type watching struct {
	// stop ends Helm's wait when cancelled; pass it to helm.Install.
	stop     context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	findings []preflight.Result
}

// start begins watching; finish ends it.
func (w waitWatch) start(ctx context.Context) *watching {
	stop, stopWait := context.WithCancel(ctx)
	watchCtx, cancel := context.WithCancel(ctx)
	wt := &watching{stop: stop, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(wt.done)
		defer stopWait()
		t := time.NewTicker(w.every)
		defer t.Stop()
		since := map[string]time.Time{}
		last := ""
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
			}
			if st, err := report.Collect(watchCtx, w.cs, nil, w.ns); err == nil {
				if line := progress(st); line != last {
					fmt.Fprintf(w.out, "  %s\n", line)
					last = line
				}
			}
			if !w.stopEarly {
				continue
			}
			found, err := doctor.Run(watchCtx, doctor.Input{Clientset: w.cs, Namespace: w.ns, Release: w.release, Cluster: w.cluster})
			if err != nil {
				continue
			}
			if lasting := hopelessFor(found, since, time.Now(), w.giveUp); len(lasting) > 0 {
				wt.findings = lasting
				fmt.Fprintf(w.out, "  Stopping the wait: %s has lasted %s, and waiting will not fix it.\n", lasting[0].Message, w.giveUp)
				stopWait()
				return
			}
		}
	}()
	return wt
}

// finish stops watching and returns the findings that stopped the wait, if
// any did.
func (wt *watching) finish() []preflight.Result {
	wt.cancel()
	<-wt.done
	return wt.findings
}

// hopelessFor returns the hopeless findings that have lasted at least giveUp,
// tracking in since when each was first seen. A finding that clears is
// forgotten, so a flapping one starts over.
func hopelessFor(found []preflight.Result, since map[string]time.Time, now time.Time, giveUp time.Duration) []preflight.Result {
	current := map[string]bool{}
	var lasting []preflight.Result
	for _, r := range found {
		if !doctor.Hopeless(r) {
			continue
		}
		// The pod count in the message changes as pods come and go.
		k := r.Code + "|" + strings.TrimSpace(strings.Split(r.Message, " [")[0])
		current[k] = true
		first, ok := since[k]
		if !ok {
			since[k] = now
			continue
		}
		if now.Sub(first) >= giveUp {
			lasting = append(lasting, r)
		}
	}
	for k := range since {
		if !current[k] {
			delete(since, k)
		}
	}
	return lasting
}

// progress is one line on what is ready and what is still awaited.
func progress(st *report.Status) string {
	ready, waiting := 0, []string{}
	for _, w := range st.Workloads {
		if w.Ready >= w.Desired {
			ready++
		} else {
			waiting = append(waiting, w.Name)
		}
	}
	sort.Strings(waiting)
	line := fmt.Sprintf("%d/%d workloads ready", ready, len(st.Workloads))
	if len(waiting) == 0 {
		return line
	}
	if len(waiting) > 4 {
		waiting = append(waiting[:4], fmt.Sprintf("and %d more", len(waiting)-4))
	}
	return line + "; waiting for " + strings.Join(waiting, ", ")
}

// printFailures runs doctor once after an install or upgrade that did not
// become ready, and prints what it finds wrong, so the cause comes with the
// failure instead of with a second command.
func (o *opts) printFailures(ctx context.Context, out io.Writer) {
	d, err := o.diagnose(ctx, 10)
	if err != nil {
		return
	}
	var bad []preflight.Result
	for _, r := range d.findings {
		if r.Severity != preflight.OK && r.Code != "NT-DR-001" {
			bad = append(bad, r)
		}
	}
	if len(bad) == 0 {
		return
	}
	fmt.Fprintln(out, "\nWhat doctor finds wrong:")
	_ = preflight.Print(out, bad, false) // printing to the terminal; a failure here changes nothing
}
