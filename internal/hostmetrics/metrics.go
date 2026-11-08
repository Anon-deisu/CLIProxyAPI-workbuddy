// Package hostmetrics samples Linux host counters and daily interface traffic.
package hostmetrics

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const SampleInterval = time.Second

type CPUCore struct {
	Name    string   `json:"name"`
	Percent *float64 `json:"percent"`
}

type CPU struct {
	TotalPercent *float64  `json:"total_percent"`
	Cores        []CPUCore `json:"cores"`
}

type Memory struct {
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	Percent    float64 `json:"percent"`
}

type NetworkInterface struct {
	Name               string   `json:"name"`
	DownloadBPS        *float64 `json:"download_bps"`
	UploadBPS          *float64 `json:"upload_bps"`
	TodayDownloadBytes uint64   `json:"today_download_bytes"`
	TodayUploadBytes   uint64   `json:"today_upload_bytes"`
}

type Network struct {
	Day              string             `json:"day"`
	Timezone         string             `json:"timezone"`
	Since            time.Time          `json:"since"`
	Persisted        bool               `json:"persisted"`
	DefaultInterface string             `json:"default_interface"`
	Interfaces       []NetworkInterface `json:"interfaces"`
}

type Snapshot struct {
	Available       bool      `json:"available"`
	Error           string    `json:"error,omitempty"`
	SampledAt       time.Time `json:"sampled_at"`
	IntervalSeconds float64   `json:"interval_seconds"`
	CPU             *CPU      `json:"cpu"`
	Memory          *Memory   `json:"memory"`
	Network         *Network  `json:"network"`
}

type cpuCounter struct{ total, idle uint64 }
type networkCounter struct{ rx, tx uint64 }
type counters struct {
	bootID  string
	cpu     map[string]cpuCounter
	network map[string]networkCounter
	at      time.Time
}

type Sampler struct {
	mu         sync.Mutex
	source     fs.FS
	supported  bool
	previous   *counters
	cached     Snapshot
	traffic    dailyTraffic
	storePath  string
	lastSave   time.Time
	storeError error
}

func NewSampler() *Sampler {
	return &Sampler{source: os.DirFS("/proc"), supported: runtime.GOOS == "linux"}
}

// Snapshot shares a sample between viewers. Initial rates are null until two
// samples exist; counter resets and newly added interfaces also warm up again.
func (s *Sampler) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cached.SampledAt.IsZero() && now.Sub(s.cached.SampledAt) < SampleInterval {
		return s.cached
	}
	result := Snapshot{SampledAt: now, IntervalSeconds: SampleInterval.Seconds()}
	if !s.supported {
		result.Error = "unsupported_platform"
		s.cached = result
		return result
	}
	current, memory, primary, errRead := readCounters(s.source)
	if errRead != nil {
		result.Error = "sampling_failed"
		s.cached = result
		return result
	}
	current.at = now
	if s.previous != nil && s.previous.bootID != current.bootID {
		s.previous = nil
	}
	s.updateTraffic(current)
	if s.storePath != "" && (s.lastSave.IsZero() || now.Sub(s.lastSave) >= trafficSaveInterval) {
		s.storeError = s.flushLocked()
		s.lastSave = now
	}
	result.Available = true
	result.CPU = &CPU{Cores: []CPUCore{}}
	result.Memory = memory
	result.Network = &Network{Day: s.traffic.day, Timezone: "UTC+08:00", Since: s.traffic.since, Persisted: s.storePath != "" && s.storeError == nil, DefaultInterface: primary, Interfaces: []NetworkInterface{}}
	var seconds float64
	if s.previous != nil {
		seconds = now.Sub(s.previous.at).Seconds()
		result.IntervalSeconds = seconds
	}
	for name, counter := range current.cpu {
		var percent *float64
		if s.previous != nil {
			if previous, exists := s.previous.cpu[name]; exists {
				percent = cpuPercent(previous, counter)
			}
		}
		if name == "cpu" {
			result.CPU.TotalPercent = percent
		} else {
			result.CPU.Cores = append(result.CPU.Cores, CPUCore{Name: name, Percent: percent})
		}
	}
	sort.Slice(result.CPU.Cores, func(i, j int) bool {
		left, _ := strconv.Atoi(strings.TrimPrefix(result.CPU.Cores[i].Name, "cpu"))
		right, _ := strconv.Atoi(strings.TrimPrefix(result.CPU.Cores[j].Name, "cpu"))
		return left < right
	})
	for name, counter := range current.network {
		daily := s.traffic.interfaces[name]
		item := NetworkInterface{Name: name, TodayDownloadBytes: daily.Downloaded, TodayUploadBytes: daily.Uploaded}
		if s.previous != nil {
			previous, exists := s.previous.network[name]
			if exists && counter.rx >= previous.rx && counter.tx >= previous.tx {
				item.DownloadBPS = new(float64(counter.rx-previous.rx) / seconds)
				item.UploadBPS = new(float64(counter.tx-previous.tx) / seconds)
			}
		}
		result.Network.Interfaces = append(result.Network.Interfaces, item)
	}
	sort.Slice(result.Network.Interfaces, func(i, j int) bool { return result.Network.Interfaces[i].Name < result.Network.Interfaces[j].Name })
	if primary == "" && len(result.Network.Interfaces) > 0 {
		result.Network.DefaultInterface = result.Network.Interfaces[0].Name
	}
	s.previous, s.cached = current, result
	return result
}

func cpuPercent(previous, current cpuCounter) *float64 {
	if current.total < previous.total || current.idle < previous.idle {
		return nil
	}
	total, idle := current.total-previous.total, current.idle-previous.idle
	if total == 0 {
		return new(float64)
	}
	return new(float64(total-min(idle, total)) * 100 / float64(total))
}

func readCounters(source fs.FS) (*counters, *Memory, string, error) {
	stat, errStat := fs.ReadFile(source, "stat")
	if errStat != nil {
		return nil, nil, "", errStat
	}
	cpus, errCPU := parseCPU(string(stat))
	if errCPU != nil {
		return nil, nil, "", errCPU
	}
	meminfo, errMemory := fs.ReadFile(source, "meminfo")
	if errMemory != nil {
		return nil, nil, "", errMemory
	}
	memory, errParse := parseMemory(string(meminfo))
	if errParse != nil {
		return nil, nil, "", errParse
	}
	devices, errDevices := fs.ReadFile(source, "net/dev")
	if errDevices != nil {
		return nil, nil, "", errDevices
	}
	network, errNetwork := parseNetwork(string(devices))
	if errNetwork != nil {
		return nil, nil, "", errNetwork
	}
	routes, errRoutes := fs.ReadFile(source, "net/route")
	if errRoutes != nil {
		return nil, nil, "", errRoutes
	}
	bootID, errBoot := fs.ReadFile(source, "sys/kernel/random/boot_id")
	if errBoot != nil {
		return nil, nil, "", errBoot
	}
	return &counters{cpu: cpus, network: network, bootID: strings.TrimSpace(string(bootID))}, memory, defaultInterface(string(routes)), nil
}

func parseCPU(data string) (map[string]cpuCounter, error) {
	result := make(map[string]cpuCounter)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if len(fields) < 5 {
			return nil, fmt.Errorf("incomplete CPU counters")
		}
		var counter cpuCounter
		// Guest and guest_nice are already included in user and nice.
		for index := 1; index < min(len(fields), 9); index++ {
			value, errParse := strconv.ParseUint(fields[index], 10, 64)
			if errParse != nil {
				return nil, errParse
			}
			counter.total += value
			if index == 4 || index == 5 {
				counter.idle += value
			}
		}
		result[fields[0]] = counter
	}
	if _, exists := result["cpu"]; !exists {
		return nil, fmt.Errorf("missing aggregate CPU counters")
	}
	return result, nil
}

func parseMemory(data string) (*Memory, error) {
	values := make(map[string]uint64)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "MemTotal:" && fields[0] != "MemAvailable:") {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return nil, fmt.Errorf("invalid memory counter")
		}
		value, errParse := strconv.ParseUint(fields[1], 10, 64)
		if errParse != nil {
			return nil, errParse
		}
		values[fields[0]] = value * 1024
	}
	total := values["MemTotal:"]
	available, exists := values["MemAvailable:"]
	if !exists || total == 0 || available > total {
		return nil, fmt.Errorf("missing or invalid memory availability")
	}
	return &Memory{TotalBytes: total, UsedBytes: total - available, Percent: float64(total-available) * 100 / float64(total)}, nil
}

func parseNetwork(data string) (map[string]networkCounter, error) {
	result := make(map[string]networkCounter)
	for _, line := range strings.Split(data, "\n") {
		name, counters, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !found || name == "lo" {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 16 {
			return nil, fmt.Errorf("incomplete interface counters")
		}
		rx, errRX := strconv.ParseUint(fields[0], 10, 64)
		tx, errTX := strconv.ParseUint(fields[8], 10, 64)
		if errRX != nil || errTX != nil {
			return nil, fmt.Errorf("invalid interface counters")
		}
		result[name] = networkCounter{rx: rx, tx: tx}
	}
	return result, nil
}

func defaultInterface(data string) string {
	var name string
	var lowest uint64
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, errFlags := strconv.ParseUint(fields[3], 16, 64)
		metric, errMetric := strconv.ParseUint(fields[6], 10, 64)
		if errFlags == nil && errMetric == nil && flags&1 != 0 && (name == "" || metric < lowest) {
			name, lowest = fields[0], metric
		}
	}
	return name
}
