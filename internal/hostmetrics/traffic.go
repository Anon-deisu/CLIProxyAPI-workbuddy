package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

const trafficSaveInterval = 30 * time.Second

var trafficLocation = time.FixedZone("UTC+08:00", 8*60*60)

type dailyInterface struct {
	RX         uint64 `json:"rx"`
	TX         uint64 `json:"tx"`
	Downloaded uint64 `json:"downloaded"`
	Uploaded   uint64 `json:"uploaded"`
}

type dailyTraffic struct {
	day        string
	since      time.Time
	bootID     string
	interfaces map[string]dailyInterface
}

func (s *Sampler) updateTraffic(current *counters) {
	day := current.at.In(trafficLocation).Format(time.DateOnly)
	if s.traffic.day != day {
		// A cross-day gap cannot be assigned accurately from cumulative counters.
		// Begin at this sample instead of attributing yesterday's bytes to today.
		s.traffic = dailyTraffic{day: day, since: current.at, interfaces: make(map[string]dailyInterface)}
	}
	for name, counter := range current.network {
		previous, exists := s.traffic.interfaces[name]
		if exists && current.bootID == s.traffic.bootID {
			if counter.rx >= previous.RX {
				previous.Downloaded += counter.rx - previous.RX
			}
			if counter.tx >= previous.TX {
				previous.Uploaded += counter.tx - previous.TX
			}
		}
		previous.RX, previous.TX = counter.rx, counter.tx
		s.traffic.interfaces[name] = previous
	}
	s.traffic.bootID = current.bootID
}

// SetStore restores only this server's daily counters. It is called before Run.
func (s *Sampler) SetStore(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.supported {
		return nil
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil && !errors.Is(errRead, os.ErrNotExist) {
		return errRead
	}
	if errRead == nil {
		root := ojson.ParseBytes(data)
		since, errTime := time.Parse(time.RFC3339Nano, root.Get("since").String())
		if !ojson.ValidBytes(data) || errTime != nil || !root.Get("interfaces").IsObject() || root.Get("day").String() == "" {
			return fmt.Errorf("invalid daily traffic state")
		}
		state := dailyTraffic{day: root.Get("day").String(), since: since, bootID: root.Get("boot_id").String(), interfaces: make(map[string]dailyInterface)}
		root.Get("interfaces").ForEach(func(name, value ojson.Result) bool {
			state.interfaces[name.String()] = dailyInterface{RX: value.Get("rx").Uint(), TX: value.Get("tx").Uint(), Downloaded: value.Get("downloaded").Uint(), Uploaded: value.Get("uploaded").Uint()}
			return true
		})
		s.traffic = state
	}
	if errDirectory := os.MkdirAll(filepath.Dir(path), 0o700); errDirectory != nil {
		return errDirectory
	}
	s.storePath = path
	return nil
}

func (s *Sampler) flushLocked() error {
	if s.storePath == "" || s.traffic.day == "" {
		return nil
	}
	doc := ojson.NewDocString(`{}`)
	for name, value := range map[string]any{"day": s.traffic.day, "since": s.traffic.since, "boot_id": s.traffic.bootID, "interfaces": s.traffic.interfaces} {
		if errSet := doc.Set(name, value); errSet != nil {
			return errSet
		}
	}
	temporary := s.storePath + ".tmp"
	if errWrite := os.WriteFile(temporary, doc.Bytes(), 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(temporary, s.storePath)
}

func (s *Sampler) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeError = s.flushLocked()
	return s.storeError
}

// Run keeps daily traffic continuous even while no dashboard is open.
func (s *Sampler) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		if errFlush := s.Flush(); errFlush != nil {
			log.WithError(errFlush).Warn("performance: save daily network traffic")
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.Snapshot(time.Now())
			timer.Reset(SampleInterval)
		}
	}
}
