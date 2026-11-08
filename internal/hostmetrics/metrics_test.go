package hostmetrics

import (
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func testSource(user, idle, rx, tx int) fstest.MapFS {
	return fstest.MapFS{
		"sys/kernel/random/boot_id": {Data: []byte("boot-one")},
		"stat":                      {Data: []byte(fmt.Sprintf("cpu %d 0 0 %d 0 0 0 0 80 0\ncpu0 %d 0 0 %d 0 0 0 0 80 0\ncpu1 50 0 0 150 0 0 0 0 0 0\n", user, idle, user-50, idle-150))},
		"meminfo":                   {Data: []byte("MemTotal: 8192 kB\nMemFree: 1024 kB\nMemAvailable: 3072 kB\nCached: 2048 kB\n")},
		"net/dev":                   {Data: []byte(fmt.Sprintf("Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\nlo: 999999 0 0 0 0 0 0 0 999999 0 0 0 0 0 0 0\neth0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\ndocker0: 2000 0 0 0 0 0 0 0 5000 0 0 0 0 0 0 0\n", rx, tx))},
		"net/route":                 {Data: []byte("Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\neth0 00000000 0100000A 0003 0 0 100 00000000 0 0 0\n")},
	}
}

func TestSamplerUsesDeltasAndAvailableMemory(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	sampler := &Sampler{source: testSource(100, 300, 1000, 2000), supported: true}
	first := sampler.Snapshot(now)
	if !first.Available || first.CPU.TotalPercent != nil || first.Network.Interfaces[1].DownloadBPS != nil {
		t.Fatalf("first sample must warm up rates: %+v", first)
	}
	sampler.source = testSource(120, 380, 5096, 4048)
	second := sampler.Snapshot(now.Add(time.Second))
	if second.CPU == nil || second.CPU.TotalPercent == nil || second.IntervalSeconds != 1 {
		t.Fatal("host metrics did not refresh after one second")
	}
	if second.CPU.TotalPercent == nil || math.Abs(*second.CPU.TotalPercent-20) > 0.001 {
		t.Fatalf("CPU guest counters were double counted: %+v", second.CPU)
	}
	if len(second.CPU.Cores) != 2 || *second.CPU.Cores[0].Percent != 20 || *second.CPU.Cores[1].Percent != 0 {
		t.Fatalf("per-core usage: %+v", second.CPU.Cores)
	}
	if second.Memory.TotalBytes != 8192*1024 || second.Memory.UsedBytes != 5120*1024 || second.Memory.Percent != 62.5 {
		t.Fatalf("memory must exclude reclaimable cache: %+v", second.Memory)
	}
	if second.Network.DefaultInterface != "eth0" || len(second.Network.Interfaces) != 2 {
		t.Fatalf("network selection or loopback exclusion failed: %+v", second.Network)
	}
	for _, iface := range second.Network.Interfaces {
		if iface.Name == "eth0" && (*iface.DownloadBPS != 4096 || *iface.UploadBPS != 2048) {
			t.Fatalf("incorrect byte rates: %+v", iface)
		}
	}
	if cached := sampler.Snapshot(now.Add(1500 * time.Millisecond)); !cached.SampledAt.Equal(second.SampledAt) {
		t.Fatal("multiple viewers caused an extra sample")
	}
}

func TestSamplerCounterResetAndNewInterfaceWarmUp(t *testing.T) {
	now := time.Now()
	sampler := &Sampler{source: testSource(100, 300, 5000, 4000), supported: true}
	sampler.Snapshot(now)
	source := testSource(90, 250, 10, 20)
	source["net/dev"].Data = append(source["net/dev"].Data, []byte("eth1: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n")...)
	sampler.source = source
	got := sampler.Snapshot(now.Add(2 * time.Second))
	if got.CPU.TotalPercent != nil {
		t.Fatal("reset CPU counters were presented as usage")
	}
	for _, iface := range got.Network.Interfaces {
		if (iface.Name == "eth0" || iface.Name == "eth1") && (iface.DownloadBPS != nil || iface.UploadBPS != nil) {
			t.Fatal("reset or newly added network counters produced a rate spike")
		}
	}
}

func TestSamplerUnavailableDoesNotInventZeros(t *testing.T) {
	for _, source := range []fstest.MapFS{
		{},
		{"stat": {Data: []byte("cpu invalid\n")}},
	} {
		sampler := &Sampler{source: source, supported: true}
		got := sampler.Snapshot(time.Now())
		if got.Available || got.Error == "" || got.CPU != nil || got.Memory != nil || got.Network != nil {
			t.Fatalf("failed sample was displayed as healthy: %+v", got)
		}
	}
	got := (&Sampler{}).Snapshot(time.Now())
	if got.Available || got.Error != "unsupported_platform" {
		t.Fatalf("unsupported platform status: %+v", got)
	}
}

func TestSamplerConcurrentReadersShareSnapshot(t *testing.T) {
	sampler := &Sampler{source: testSource(100, 300, 1000, 2000), supported: true}
	now := time.Now()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if got := sampler.Snapshot(now); !got.Available || !got.SampledAt.Equal(now) {
				t.Errorf("shared snapshot unavailable: %+v", got)
			}
		})
	}
	wg.Wait()
}

func TestDailyTrafficPersistsAcrossRestartAndResetsAtChinaMidnight(t *testing.T) {
	now := time.Date(2026, 11, 7, 15, 59, 50, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "traffic.json")
	sampler := &Sampler{source: testSource(100, 300, 1000, 2000), supported: true}
	if err := sampler.SetStore(path); err != nil {
		t.Fatal(err)
	}
	sampler.Snapshot(now)
	sampler.source = testSource(120, 380, 5096, 4048)
	got := sampler.Snapshot(now.Add(2 * time.Second))
	if got.Network.Interfaces[1].TodayDownloadBytes != 4096 || got.Network.Interfaces[1].TodayUploadBytes != 2048 {
		t.Fatalf("daily traffic lost deltas: %+v", got.Network.Interfaces[1])
	}
	if err := sampler.Flush(); err != nil {
		t.Fatal(err)
	}
	restarted := &Sampler{source: testSource(140, 460, 9192, 6096), supported: true}
	if err := restarted.SetStore(path); err != nil {
		t.Fatal(err)
	}
	got = restarted.Snapshot(now.Add(4 * time.Second))
	if got.Network.Interfaces[1].TodayDownloadBytes != 8192 || got.Network.Interfaces[1].TodayUploadBytes != 4096 || got.Network.Day != "2026-11-07" || !got.Network.Since.Equal(now) {
		t.Fatalf("restart lost same-day traffic: %+v", got.Network)
	}
	restarted.source = testSource(160, 540, 20000, 10000)
	got = restarted.Snapshot(now.Add(12 * time.Second))
	if got.Network.Day != "2026-11-08" || got.Network.Interfaces[1].TodayDownloadBytes != 0 {
		t.Fatal("yesterday's traffic was attributed to today")
	}
	restarted.source = testSource(180, 620, 22000, 11000)
	got = restarted.Snapshot(now.Add(14 * time.Second))
	if got.Network.Interfaces[1].TodayDownloadBytes != 2000 || got.Network.Interfaces[1].TodayUploadBytes != 1000 {
		t.Fatal("new day did not resume counting")
	}
}

func TestDailyTrafficNewBootAndCounterReset(t *testing.T) {
	now := time.Now()
	sampler := &Sampler{source: testSource(100, 300, 1000, 2000), supported: true}
	sampler.Snapshot(now)
	sampler.source = testSource(120, 380, 2000, 4000)
	sampler.Snapshot(now.Add(2 * time.Second))
	source := testSource(100, 300, 5000, 6000)
	source["sys/kernel/random/boot_id"].Data = []byte("boot-two")
	sampler.source = source
	got := sampler.Snapshot(now.Add(4 * time.Second))
	if got.Network.Interfaces[1].TodayDownloadBytes != 1000 || got.Network.Interfaces[1].TodayUploadBytes != 2000 || got.Network.Interfaces[1].DownloadBPS != nil {
		t.Fatal("new boot counters were mistaken for continuous traffic")
	}
}
