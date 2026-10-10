package vm

import (
	"context"
	"fmt"
	"sync"
)

// FakeVMM est une implémentation de VMM en mémoire, destinée aux tests.
//
// Elle permet d'exercer la machine d'état, la supervision et la temporisation
// des relances sans KVM ni Firecracker, donc aussi dans la CI, où /dev/kvm
// n'existe pas.
type FakeVMM struct {
	mu       sync.Mutex
	created  map[string]Spec
	running  map[string]chan struct{}
	pids     map[string]int
	nextPID  int
	starts   map[string]int
	stops    map[string]int
	StartErr error
}

// NewFakeVMM renvoie un hyperviseur simulé vide.
func NewFakeVMM() *FakeVMM {
	return &FakeVMM{
		created: map[string]Spec{},
		running: map[string]chan struct{}{},
		pids:    map[string]int{},
		starts:  map[string]int{},
		stops:   map[string]int{},
		nextPID: 1000,
	}
}

func (f *FakeVMM) Create(_ context.Context, spec Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.created[spec.Name]; ok {
		return fmt.Errorf("vm %s déjà présente", spec.Name)
	}
	f.created[spec.Name] = spec
	return nil
}

func (f *FakeVMM) Start(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.created[name]; !ok {
		return fmt.Errorf("vm %s inconnue", name)
	}
	if f.StartErr != nil {
		return f.StartErr
	}
	if _, ok := f.running[name]; ok {
		return fmt.Errorf("vm %s déjà démarrée", name)
	}
	f.running[name] = make(chan struct{})
	f.nextPID++
	f.pids[name] = f.nextPID
	f.starts[name]++
	return nil
}

func (f *FakeVMM) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops[name]++
	if ch, ok := f.running[name]; ok {
		close(ch)
		delete(f.running, name)
		delete(f.pids, name)
	}
	return nil
}

func (f *FakeVMM) Delete(ctx context.Context, name string) error {
	_ = f.Stop(ctx, name)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.created, name)
	return nil
}

func (f *FakeVMM) Metrics(_ context.Context, name string) (Metrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid, ok := f.pids[name]
	if !ok {
		return Metrics{}, fmt.Errorf("vm %s arrêtée", name)
	}
	return Metrics{PID: pid, RSSMiB: 12}, nil
}

func (f *FakeVMM) ConsoleLogPath(name string) string {
	return "/dev/null"
}

func (f *FakeVMM) Wait(ctx context.Context, name string) error {
	f.mu.Lock()
	ch, ok := f.running[name]
	f.mu.Unlock()
	if !ok {
		return nil // déjà arrêtée
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// Crash simule un arrêt que personne n'a demandé, pour exercer la relance
// automatique.
func (f *FakeVMM) Crash(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.running[name]; ok {
		close(ch)
		delete(f.running, name)
		delete(f.pids, name)
	}
}

// IsRunning indique si la VM simulée tourne.
func (f *FakeVMM) IsRunning(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.running[name]
	return ok
}

// StartCount renvoie le nombre de démarrages réussis d'une VM.
func (f *FakeVMM) StartCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[name]
}

// StopCount renvoie le nombre d'arrêts demandés pour une VM.
func (f *FakeVMM) StopCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops[name]
}
