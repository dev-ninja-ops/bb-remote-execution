package resourcepool

import (
	"context"
	"fmt"
	"sync"

	bb_worker "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_worker"
)

// Allocation is a set of resources reserved from a Pool for one action.
// CPUMillicores and MemoryBytes describe the reservation; GPUUUIDs are
// the concrete GPU identifiers that were handed out (subset of the
// configured pool).
type Allocation struct {
	CPUMillicores uint32
	MemoryBytes   uint64
	GPUUUIDs      []string
}

// Pool implements local admission control for CPU millicores, memory
// bytes, and a discrete set of GPU UUIDs. Acquire blocks until the
// request fits; Release returns the resources. A zero-valued request
// (no CPU, no memory, no GPUs) acquires immediately.
//
// Configured totals act as soft caps when the corresponding field is
// non-zero. If cpuMillicores is 0 in the config, CPU is unmetered (any
// request is admitted). Same for memoryBytes. For GPUs the configured
// UUID list also caps the total.
type Pool struct {
	mu        sync.Mutex
	cond      *sync.Cond
	cpuTotal  uint32
	cpuFree   uint32
	memTotal  uint64
	memFree   uint64
	gpusFree  []string
	gpusTotal int
}

// NewPool constructs a Pool from a ResourcePoolConfiguration. A nil
// config or a fully-zero config yields a Pool that admits every
// request immediately (back-compat: behaves as if there were no pool).
func NewPool(cfg *bb_worker.ResourcePoolConfiguration) *Pool {
	p := &Pool{}
	if cfg != nil {
		p.cpuTotal = cfg.CpuMillicores
		p.cpuFree = cfg.CpuMillicores
		p.memTotal = cfg.MemoryBytes
		p.memFree = cfg.MemoryBytes
		if len(cfg.GpuUuids) > 0 {
			p.gpusFree = append(p.gpusFree, cfg.GpuUuids...)
			p.gpusTotal = len(cfg.GpuUuids)
		}
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Acquire blocks until the requested resources can be reserved or the
// context is cancelled. The returned Allocation must be passed back to
// Release exactly once (typically via defer). When request is zero in
// every dimension, returns immediately with a no-op allocation.
//
// gpuCount asks for that many GPU UUIDs (popped from the head of the
// free list); the returned Allocation.GPUUUIDs records the assignment.
func (p *Pool) Acquire(ctx context.Context, cpuMillicores uint32, memBytes uint64, gpuCount int) (Allocation, error) {
	// Validate against configured totals before blocking forever.
	if p.cpuTotal > 0 && cpuMillicores > p.cpuTotal {
		return Allocation{}, fmt.Errorf("requested cpu %d millicores exceeds pool total %d", cpuMillicores, p.cpuTotal)
	}
	if p.memTotal > 0 && memBytes > p.memTotal {
		return Allocation{}, fmt.Errorf("requested memory %d bytes exceeds pool total %d", memBytes, p.memTotal)
	}
	if gpuCount > p.gpusTotal {
		return Allocation{}, fmt.Errorf("requested %d GPUs exceeds pool total %d", gpuCount, p.gpusTotal)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Set up a goroutine to wake us on context cancellation so we
	// can return promptly. Started only if we end up waiting.
	stopWaker := make(chan struct{})
	defer close(stopWaker)
	var wakerOnce sync.Once
	startWaker := func() {
		wakerOnce.Do(func() {
			go func() {
				select {
				case <-ctx.Done():
					p.mu.Lock()
					p.cond.Broadcast()
					p.mu.Unlock()
				case <-stopWaker:
				}
			}()
		})
	}

	for {
		if ctx.Err() != nil {
			return Allocation{}, ctx.Err()
		}
		cpuOK := p.cpuTotal == 0 || cpuMillicores <= p.cpuFree
		memOK := p.memTotal == 0 || memBytes <= p.memFree
		gpuOK := gpuCount <= len(p.gpusFree)
		if cpuOK && memOK && gpuOK {
			alloc := Allocation{
				CPUMillicores: cpuMillicores,
				MemoryBytes:   memBytes,
			}
			if p.cpuTotal > 0 {
				p.cpuFree -= cpuMillicores
			}
			if p.memTotal > 0 {
				p.memFree -= memBytes
			}
			if gpuCount > 0 {
				alloc.GPUUUIDs = append(alloc.GPUUUIDs, p.gpusFree[:gpuCount]...)
				p.gpusFree = p.gpusFree[gpuCount:]
			}
			return alloc, nil
		}
		startWaker()
		p.cond.Wait()
	}
}

// Release returns the resources held by an Allocation to the pool and
// wakes any waiters. Safe to call with the zero Allocation (no-op).
func (p *Pool) Release(a Allocation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cpuTotal > 0 {
		p.cpuFree += a.CPUMillicores
		if p.cpuFree > p.cpuTotal {
			p.cpuFree = p.cpuTotal
		}
	}
	if p.memTotal > 0 {
		p.memFree += a.MemoryBytes
		if p.memFree > p.memTotal {
			p.memFree = p.memTotal
		}
	}
	if len(a.GPUUUIDs) > 0 {
		p.gpusFree = append(p.gpusFree, a.GPUUUIDs...)
	}
	p.cond.Broadcast()
}
