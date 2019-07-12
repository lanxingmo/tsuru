// Copyright 2019 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package applog

import (
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	appTypes "github.com/tsuru/tsuru/types/app"
	"github.com/tsuru/tsuru/types/auth"
)

const (
	maxAppBufferSize = 1 * 1024 * 1024 // 1 MiB
	watchBufferSize  = 1000
	sizeofTime       = unsafe.Sizeof(time.Time{})
)

var (
	logsMemoryReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tsuru_logs_memory_received_total",
		Help: "The number of in memory log entries received for processing.",
	}, []string{"app"})

	logsMemoryEvicted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tsuru_logs_memory_evicted_total",
		Help: "The number of in memory log entries removed due to full buffer.",
	}, []string{"app"})

	logsMemoryBlockedWatch = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tsuru_logs_memory_watch_blocked_seconds_total",
		Help: "The total time spent blocked trying to notify watchers of new logs.",
	}, []string{"app"})

	logsMemorySize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tsuru_logs_memory_size",
		Help: "The size in bytes for in memory log entries of a given app.",
	}, []string{"app"})

	logsMemoryLength = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tsuru_logs_memory_length",
		Help: "The number of in memory log entries for a given app.",
	}, []string{"app"})
)

func init() {
	prometheus.MustRegister(logsMemoryReceived)
	prometheus.MustRegister(logsMemoryEvicted)
	prometheus.MustRegister(logsMemoryBlockedWatch)
	prometheus.MustRegister(logsMemorySize)
	prometheus.MustRegister(logsMemoryLength)
}

type memoryLogService struct {
	bufferMap sync.Map
}

func memoryAppLogService() (appTypes.AppLogService, error) {
	return &memoryLogService{}, nil
}

func (s *memoryLogService) Enqueue(entry *appTypes.Applog) error {
	buffer := s.getAppBuffer(entry.AppName)
	buffer.add(entry)
	logsMemoryReceived.WithLabelValues(entry.AppName).Inc()
	return nil
}

func (s *memoryLogService) Add(appName, message, source, unit string) error {
	messages := strings.Split(message, "\n")
	logs := make([]*appTypes.Applog, 0, len(messages))
	for _, msg := range messages {
		if msg != "" {
			l := &appTypes.Applog{
				Date:    time.Now().In(time.UTC),
				Message: msg,
				Source:  source,
				AppName: appName,
				Unit:    unit,
			}
			logs = append(logs, l)
		}
	}
	if len(logs) == 0 {
		return nil
	}
	for _, log := range logs {
		err := s.Enqueue(log)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *memoryLogService) List(args appTypes.ListLogArgs) ([]appTypes.Applog, error) {
	if args.AppName == "" {
		return nil, errors.New("app name required to list logs")
	}
	buffer := s.getAppBuffer(args.AppName)
	if buffer.length == 0 {
		return []appTypes.Applog{}, nil
	}
	if args.Limit == 0 || buffer.length < args.Limit {
		args.Limit = buffer.length
	}
	logs := make([]appTypes.Applog, args.Limit)
	var count int
	for current := buffer.end; count < args.Limit; {
		if (args.Source == "" || (args.Source == current.log.Source) != args.InvertFilters) &&
			(args.Unit == "" || (args.Unit == current.log.Unit) != args.InvertFilters) {

			logs[len(logs)-count-1] = *current.log
			count++
		}
		current = current.prev
		if current == buffer.end {
			break
		}
	}
	return logs[len(logs)-count:], nil
}

func (s *memoryLogService) Watch(appName, source, unit string, t auth.Token) (appTypes.LogWatcher, error) {
	buffer := s.getAppBuffer(appName)
	watcher := &memoryWatcher{
		buffer: buffer,
		ch:     make(chan appTypes.Applog, watchBufferSize),
		source: source,
		unit:   unit,
	}
	buffer.addWatcher(watcher)
	return watcher, nil

}

func (s *memoryLogService) getAppBuffer(appName string) *appLogBuffer {
	buffer, _ := s.bufferMap.LoadOrStore(appName, &appLogBuffer{appName: appName})
	return buffer.(*appLogBuffer)
}

type ringEntry struct {
	log        *appTypes.Applog
	size       uint
	next, prev *ringEntry
}

type appLogBuffer struct {
	mu         sync.Mutex
	appName    string
	size       uint
	length     int
	start, end *ringEntry
	watchers   []*memoryWatcher
}

func (b *appLogBuffer) add(entry *appTypes.Applog) {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := &ringEntry{
		log:  entry,
		size: entrySize(entry),
	}
	if next.size > maxAppBufferSize {
		return
	}
	if b.start == nil {
		b.start = next
		b.end = next
	}
	next.next = b.start
	next.prev = b.end
	b.start.prev = next
	b.end.next = next
	b.end = b.end.next
	b.length++
	newFullSize := b.size + next.size
	evicted := logsMemoryEvicted.WithLabelValues(b.appName)
	for newFullSize > maxAppBufferSize {
		newFullSize -= b.start.size
		b.start = b.start.next
		b.start.prev = b.end
		b.length--
		evicted.Inc()
	}
	b.size = newFullSize
	logsMemorySize.WithLabelValues(b.appName).Set(float64(b.size))
	logsMemoryLength.WithLabelValues(b.appName).Set(float64(b.length))
	for _, w := range b.watchers {
		if w.source != "" && w.source != entry.Source {
			continue
		}
		if w.unit != "" && w.unit != entry.Unit {
			continue
		}
		select {
		case w.ch <- *entry:
		default:
			t0 := time.Now()
			w.ch <- *entry
			logsMemoryBlockedWatch.WithLabelValues(b.appName).Add(time.Since(t0).Seconds())
		}
	}
}

func (b *appLogBuffer) addWatcher(watcher *memoryWatcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.watchers = append(b.watchers, watcher)
}

func (b *appLogBuffer) removeWatcher(watcher *memoryWatcher) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.watchers {
		if b.watchers[i] == watcher {
			b.watchers[i] = b.watchers[len(b.watchers)-1]
			b.watchers = b.watchers[:len(b.watchers)-1]
			return true
		}
	}
	return false
}

func entrySize(entry *appTypes.Applog) uint {
	return uint(len(entry.AppName) +
		len(entry.Message) +
		len(entry.MongoID) +
		len(entry.Source) +
		len(entry.Unit) +
		int(sizeofTime))
}

type memoryWatcher struct {
	buffer *appLogBuffer
	ch     chan appTypes.Applog
	source string
	unit   string
}

func (w *memoryWatcher) Chan() <-chan appTypes.Applog {
	return w.ch
}

func (w *memoryWatcher) Close() {
	if w.buffer.removeWatcher(w) {
		close(w.ch)
	}
}