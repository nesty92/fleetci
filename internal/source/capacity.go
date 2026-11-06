package source

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
)

// Backend is what Capacity needs of fleet.Backend.
type Backend interface {
	Reserve(class string) (*fleet.Reservation, error)
	Create(ctx context.Context, class string) (fleet.VM, error)
	Delete(ctx context.Context, id string) error
}

// held is a *fleet.Reservation.
type held interface {
	Class() string
	Acquire(ctx context.Context) (fleet.VM, error)
	Release()
}

// backend is Backend with held in place of *fleet.Reservation, so tests
// can fake it.
type backend interface {
	Reserve(class string) (held, error)
	Create(ctx context.Context, class string) (fleet.VM, error)
	Delete(ctx context.Context, id string) error
}

type fleetBackend struct{ Backend }

func (b fleetBackend) Reserve(class string) (held, error) {
	r, err := b.Backend.Reserve(class)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// NewCapacity is the core's Capacity over a backend and its classes.
func NewCapacity(b Backend, classes map[string]fleet.Class, defaultClass string) Capacity {
	return newCapacity(fleetBackend{b}, classes, defaultClass)
}

func newCapacity(b backend, classes map[string]fleet.Class, defaultClass string) *capacity {
	cs := make([]Class, 0, len(classes))
	for name, c := range classes {
		cs = append(cs, Class{Name: name, Slots: c.Slots(), Overflow: c.Overflow})
	}
	slices.SortFunc(cs, func(a, b Class) int { return strings.Compare(a.Name, b.Name) })
	return &capacity{b: b, classes: cs, defaultClass: defaultClass,
		reservations: map[string]held{}, vms: map[string]fleet.VM{}}
}

type capacity struct {
	b            backend
	classes      []Class
	defaultClass string

	mu           sync.Mutex
	reservations map[string]held
	vms          map[string]fleet.VM
}

func (c *capacity) Classes(context.Context) ([]Class, string, error) {
	return slices.Clone(c.classes), c.defaultClass, nil
}

func (c *capacity) Reserve(_ context.Context, class string) (Reservation, error) {
	r, err := c.b.Reserve(class)
	if err != nil {
		return Reservation{}, err
	}
	id := "r-" + randomHex()
	c.mu.Lock()
	c.reservations[id] = r
	c.mu.Unlock()
	return Reservation{ID: id, Class: r.Class()}, nil
}

// take forgets a reservation and returns it.
func (c *capacity) take(id string) (held, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.reservations[id]
	if !ok {
		return nil, fmt.Errorf("no reservation %q", id)
	}
	delete(c.reservations, id)
	return r, nil
}

func (c *capacity) Acquire(ctx context.Context, res Reservation) (VM, error) {
	r, err := c.take(res.ID)
	if err != nil {
		return VM{}, err
	}
	return c.keep(r.Acquire(ctx))
}

func (c *capacity) Release(_ context.Context, res Reservation) error {
	r, err := c.take(res.ID)
	if err != nil {
		return err
	}
	r.Release()
	return nil
}

func (c *capacity) Create(ctx context.Context, class string) (VM, error) {
	return c.keep(c.b.Create(ctx, class))
}

// keep remembers a VM for Conn.
func (c *capacity) keep(vm fleet.VM, err error) (VM, error) {
	if err != nil {
		return VM{}, err
	}
	c.mu.Lock()
	c.vms[vm.ID] = vm
	c.mu.Unlock()
	return VM{ID: vm.ID, Class: vm.Class, Instance: vm.Instance}, nil
}

func (c *capacity) Conn(id string) (*proxy.Conn, error) {
	c.mu.Lock()
	vm, ok := c.vms[id]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no vm %q", id)
	}
	return vm.Conn, nil
}

func (c *capacity) Delete(ctx context.Context, id string) error {
	c.mu.Lock()
	delete(c.vms, id)
	c.mu.Unlock()
	return c.b.Delete(ctx, id)
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
