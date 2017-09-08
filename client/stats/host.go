package stats

import (
	"log"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/mem"

	shelpers "github.com/hashicorp/nomad/helper/stats"
)

// HostStats represents resource usage stats of the host running a Nomad client
type HostStats struct {
	Memory           *MemoryStats
	CPU              []*CPUStats
	DiskStats        []*DiskStats
	AllocDirStats    *DiskStats
	Uptime           uint64
	Timestamp        int64
	CPUTicksConsumed float64
}

// MemoryStats represnts stats related to virtual memory usage
type MemoryStats struct {
	Total     uint64
	Available uint64
	Used      uint64
	Free      uint64
}

// CPUStats represents stats related to cpu usage
type CPUStats struct {
	CPU    string
	User   float64
	System float64
	Idle   float64
	Total  float64
}

// DiskStats represents stats related to disk usage
type DiskStats struct {
	Device            string
	Mountpoint        string
	Size              uint64
	Used              uint64
	Available         uint64
	UsedPercent       float64
	InodesUsedPercent float64
}

// NodeStatsCollector is an interface which is used for the puproses of mocking
// the HostStatsCollector in the tests
type NodeStatsCollector interface {
	Collect() error
	Stats() *HostStats
}

// HostStatsCollector collects host resource usage stats
type HostStatsCollector struct {
	clkSpeed        float64
	numCores        int
	statsCalculator map[string]*HostCpuStatsCalculator
	logger          *log.Logger
	hostStats       *HostStats
	hostStatsLock   sync.RWMutex
	allocDir        string

	// badParts is a set of partitions whose usage cannot be read; used to
	// squelch logspam.
	badParts map[string]struct{}
}

// NewHostStatsCollector returns a HostStatsCollector. The allocDir is passed in
// so that we can present the disk related statistics for the mountpoint where
// the allocation directory lives
func NewHostStatsCollector(logger *log.Logger, allocDir string) *HostStatsCollector {
	numCores := runtime.NumCPU()
	statsCalculator := make(map[string]*HostCpuStatsCalculator)
	collector := &HostStatsCollector{
		statsCalculator: statsCalculator,
		numCores:        numCores,
		logger:          logger,
		allocDir:        allocDir,
		badParts:        make(map[string]struct{}),
	}
	return collector
}

// Collect collects stats related to resource usage of a host
func (h *HostStatsCollector) Collect() error {
	hs := &HostStats{Timestamp: time.Now().UTC().UnixNano()}
	memStats, err := mem.VirtualMemory()
	if err != nil {
		return err
	}
	hs.Memory = &MemoryStats{
		Total:     memStats.Total,
		Available: memStats.Available,
		Used:      memStats.Used,
		Free:      memStats.Free,
	}

	ticksConsumed := 0.0
	cpuStats, err := cpu.Times(true)
	if err != nil {
		return err
	}
	cs := make([]*CPUStats, len(cpuStats))
	for idx, cpuStat := range cpuStats {
		percentCalculator, ok := h.statsCalculator[cpuStat.CPU]
		if !ok {
			percentCalculator = NewHostCpuStatsCalculator()
			h.statsCalculator[cpuStat.CPU] = percentCalculator
		}
		idle, user, system, total := percentCalculator.Calculate(cpuStat)
		cs[idx] = &CPUStats{
			CPU:    cpuStat.CPU,
			User:   user,
			System: system,
			Idle:   idle,
			Total:  total,
		}
		ticksConsumed += (total / 100) * (shelpers.TotalTicksAvailable() / float64(len(cpuStats)))
	}
	hs.CPU = cs
	hs.CPUTicksConsumed = ticksConsumed

	partitions, err := disk.Partitions(false)
	if err != nil {
		return err
	}
	var diskStats []*DiskStats
	for _, partition := range partitions {
		usage, err := disk.Usage(partition.Mountpoint)
		if err != nil {
			if _, ok := h.badParts[partition.Mountpoint]; ok {
				// already known bad, don't log again
				continue
			}

			h.badParts[partition.Mountpoint] = struct{}{}
			h.logger.Printf("[WARN] client: error fetching host disk usage stats for %v: %v", partition.Mountpoint, err)
			continue
		}
		delete(h.badParts, partition.Mountpoint)

		ds := h.toDiskStats(usage, &partition)
		diskStats = append(diskStats, ds)
	}
	hs.DiskStats = diskStats

	// Getting the disk stats for the allocation directory
	usage, err := disk.Usage(h.allocDir)
	if err != nil {
		return err
	}
	hs.AllocDirStats = h.toDiskStats(usage, nil)

	uptime, err := host.Uptime()
	if err != nil {
		return err
	}
	hs.Uptime = uptime

	h.hostStatsLock.Lock()
	h.hostStats = hs
	h.hostStatsLock.Unlock()
	return nil
}

// Stats returns the host stats that has been collected
func (h *HostStatsCollector) Stats() *HostStats {
	h.hostStatsLock.RLock()
	defer h.hostStatsLock.RUnlock()
	return h.hostStats
}

// toDiskStats merges UsageStat and PartitionStat to create a DiskStat
func (h *HostStatsCollector) toDiskStats(usage *disk.UsageStat, partitionStat *disk.PartitionStat) *DiskStats {
	if usage == nil {
		return nil
	}
	ds := DiskStats{
		Size:              usage.Total,
		Used:              usage.Used,
		Available:         usage.Free,
		UsedPercent:       usage.UsedPercent,
		InodesUsedPercent: usage.InodesUsedPercent,
	}
	if math.IsNaN(ds.UsedPercent) {
		ds.UsedPercent = 0.0
	}
	if math.IsNaN(ds.InodesUsedPercent) {
		ds.InodesUsedPercent = 0.0
	}

	if partitionStat != nil {
		ds.Device = partitionStat.Device
		ds.Mountpoint = partitionStat.Mountpoint
	}

	return &ds
}

// HostCpuStatsCalculator calculates cpu usage percentages
type HostCpuStatsCalculator struct {
	prevIdle   float64
	prevUser   float64
	prevSystem float64
	prevBusy   float64
	prevTotal  float64
}

// NewHostCpuStatsCalculator returns a HostCpuStatsCalculator
func NewHostCpuStatsCalculator() *HostCpuStatsCalculator {
	return &HostCpuStatsCalculator{}
}

// Calculate calculates the current cpu usage percentages
func (h *HostCpuStatsCalculator) Calculate(times cpu.TimesStat) (idle float64, user float64, system float64, total float64) {
	currentIdle := times.Idle
	currentUser := times.User
	currentSystem := times.System
	currentTotal := times.Total()

	deltaTotal := currentTotal - h.prevTotal
	idle = ((currentIdle - h.prevIdle) / deltaTotal) * 100
	if math.IsNaN(idle) {
		idle = 100.0
	}

	user = ((currentUser - h.prevUser) / deltaTotal) * 100
	if math.IsNaN(user) {
		user = 0.0
	}

	system = ((currentSystem - h.prevSystem) / deltaTotal) * 100
	if math.IsNaN(system) {
		system = 0.0
	}

	currentBusy := times.User + times.System + times.Nice + times.Iowait + times.Irq +
		times.Softirq + times.Steal + times.Guest + times.GuestNice + times.Stolen

	total = ((currentBusy - h.prevBusy) / deltaTotal) * 100
	if math.IsNaN(total) {
		total = 0.0
	}

	h.prevIdle = currentIdle
	h.prevUser = currentUser
	h.prevSystem = currentSystem
	h.prevTotal = currentTotal
	h.prevBusy = currentBusy

	return
}
