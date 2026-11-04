package metrics

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/platform"
)

// Instance is a running instance, as far as sampling goes.
type Instance struct {
	EnvID, ID string
	// Part is its process and index, such as web.1, which stays the same across releases.
	Part string
	PID  int
}

// Usage is what an instance is using.
type Usage struct {
	CPU    float64 // percent of one core
	Memory uint64  // bytes
}

// Collector samples what is running, every so often, and keeps the store's tiers moving.
type Collector struct {
	Store *Store
	Log   *slog.Logger
	// Instances lists what is running. Containers says what the container engine knows of those
	// it runs, by instance ID: they have no process of ours to look at.
	Instances  func(ctx context.Context) []Instance
	Containers func(ctx context.Context) map[string]Usage
	// DataDir is the volume whose space is watched.
	DataDir string

	last map[string]cpuReading
}

type cpuReading struct {
	seconds float64
	at      time.Time
}

// Run samples until ctx ends.
func (c *Collector) Run(ctx context.Context, every time.Duration) {
	if every == 0 {
		every = SampleEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	pruned := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.Sample(ctx, now)
			if err := c.Store.Flush(ctx, now); err != nil && ctx.Err() == nil {
				c.Log.Warn("write metrics", "err", err)
			}
			if now.Sub(pruned) > 24*time.Hour {
				pruned = now
				if err := c.Store.Prune(ctx, now); err != nil && ctx.Err() == nil {
					c.Log.Warn("let go of old metrics", "err", err)
				}
			}
		}
	}
}

// Sample records a moment: what requests left behind, what instances use, and the server itself.
func (c *Collector) Sample(ctx context.Context, now time.Time) {
	c.Store.Tick(now)
	if c.Instances != nil {
		c.sampleInstances(ctx, now)
	}
	c.sampleHost(now)
}

func (c *Collector) sampleInstances(ctx context.Context, now time.Time) {
	list := c.Instances(ctx)
	if len(list) == 0 {
		return
	}
	procs, err := readProcesses(ctx)
	if err != nil {
		c.Log.Debug("read the processes", "err", err)
	}
	var engine map[string]Usage
	if c.Containers != nil {
		engine = c.Containers(ctx)
	}
	tree := childrenOf(procs)
	next := make(map[string]cpuReading, len(list))
	for _, in := range list {
		if u, ok := engine[in.ID]; ok {
			c.Store.Gauge(in.EnvID, CPU, in.Part, u.CPU, now)
			c.Store.Gauge(in.EnvID, Memory, in.Part, float64(u.Memory), now)
			continue
		}
		if in.PID == 0 {
			continue
		}
		seconds, rss, found := sumTree(procs, tree, in.PID)
		if !found {
			continue
		}
		c.Store.Gauge(in.EnvID, Memory, in.Part, float64(rss), now)
		reading := cpuReading{seconds, now}
		next[in.ID] = reading
		if prev, ok := c.last[in.ID]; ok && now.After(prev.at) {
			used := seconds - prev.seconds
			if used < 0 { // what used it has gone
				used = 0
			}
			c.Store.Gauge(in.EnvID, CPU, in.Part, used/now.Sub(prev.at).Seconds()*100, now)
		}
	}
	c.last = next
}

func (c *Collector) sampleHost(now time.Time) {
	if load, ok := hostLoad(); ok {
		c.Store.Gauge("", HostLoad, "", load, now)
	}
	if used, ok := hostMemoryUsed(); ok {
		c.Store.Gauge("", HostMemory, "", float64(used), now)
	}
	if c.DataDir != "" {
		if free, total, err := platform.DiskSpace(c.DataDir); err == nil && total > 0 {
			c.Store.Gauge("", HostDisk, "", float64(total-free)/float64(total)*100, now)
		}
	}
}

// proc is a process, as far as sampling goes.
type proc struct {
	ppid    int
	seconds float64 // of CPU, all it has ever used
	rss     uint64  // bytes
}

func childrenOf(procs map[int]proc) map[int][]int {
	tree := make(map[int][]int, len(procs))
	for pid, p := range procs {
		tree[p.ppid] = append(tree[p.ppid], pid)
	}
	return tree
}

// sumTree adds up a process and all that descends from it.
func sumTree(procs map[int]proc, tree map[int][]int, pid int) (seconds float64, rss uint64, found bool) {
	root, ok := procs[pid]
	if !ok {
		return 0, 0, false
	}
	seconds, rss = root.seconds, root.rss
	stack := append([]int(nil), tree[pid]...)
	seen := map[int]bool{pid: true}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		p := procs[next]
		seconds += p.seconds
		rss += p.rss
		stack = append(stack, tree[next]...)
	}
	return seconds, rss, true
}

// ProcessOf is the process of a part such as web.1.
func ProcessOf(part string) string {
	name, _, _ := strings.Cut(part, ".")
	return name
}
