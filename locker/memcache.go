package locker

import (
	"encoding/json"
	"fmt"
	"github.com/Automattic/cron-control-runner/logger"
	"github.com/bradfitz/gomemcache/memcache"
	"os"
	"sort"
	"time"
)

var _ Locker = &memcacheLocker{}

type memcacheLocker struct {
	Log             logger.Logger
	KeyPrefix       string
	ConfigPath      string
	ServerList      *memcache.ServerList
	Client          *memcache.Client
	LeaseInterval   time.Duration
	RefreshInterval time.Duration
	CloseChan       chan struct{}
}

func NewMemcache(log logger.Logger, keyPrefix, configPath string, leaseInterval, refreshInterval time.Duration) Locker {
	res := &memcacheLocker{
		KeyPrefix:       keyPrefix,
		Log:             log,
		ConfigPath:      configPath,
		ServerList:      &memcache.ServerList{},
		LeaseInterval:   leaseInterval,
		RefreshInterval: refreshInterval,
		CloseChan:       make(chan struct{}),
	}
	res.Client = memcache.NewFromSelector(res.ServerList)
	res.tryReloadConfig()
	go res.runConfigReloader()
	return res
}

func (m *memcacheLocker) Close() error {
	close(m.CloseChan)
	return nil
}

var myHostname = []byte((func() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
})())

type memcacheLock struct {
	Owner   *memcacheLocker
	Key     string
	Expires time.Time
}

func (m *memcacheLocker) Lock(k string) (Lock, error) {
	key := fmt.Sprintf("%s%s", m.KeyPrefix, k)
	if err := m.Client.Add(&memcache.Item{
		Key:        key,
		Value:      myHostname, // we put the hostname of the lock owner as the value of the key
		Flags:      0,
		Expiration: int32(m.LeaseInterval.Seconds()),
	}); err == nil {
		return &memcacheLock{
			Owner:   m,
			Key:     key,
			Expires: time.Now().Add(m.LeaseInterval - (1 * time.Second)),
		}, nil
	} else if err == memcache.ErrNotStored {
		return nil, nil
	} else {
		return nil, fmt.Errorf("could not add key=%q: %v", key, err)
	}
}

func (m *memcacheLock) Unlock() error {
	// do NOT delete a lock that might have expired:
	if time.Until(m.Expires) > (1 * time.Second) {
		if err := m.Owner.Client.Delete(m.Key); err != nil && err != memcache.ErrCacheMiss {
			return err
		}
	}
	return nil
}

func (m *memcacheLocker) runConfigReloader() {
	ticker := time.NewTicker(m.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.CloseChan:
			return
		case <-ticker.C:
			m.tryReloadConfig()
		}
	}
}

func (m *memcacheLocker) tryReloadConfig() {
	m.Log.Debugf("reloading memcache config")
	if err := m.reloadConfig(); err != nil {
		m.Log.Errorf("failed to reload memcache config: %v", err)
	}
}

func (m *memcacheLocker) reloadConfig() error {
	f, err := os.Open(m.ConfigPath)
	if err != nil {
		return err
	}
	defer (func() { _ = f.Close() })()
	var dc DataConfig
	if err = json.NewDecoder(f).Decode(&dc); err != nil {
		return err
	}
	servers := make([]string, len(dc.Memcache))
	for i, mcs := range dc.Memcache {
		servers[i] = fmt.Sprintf("%s:%d", mcs.Host, mcs.Port)
	}
	sort.Strings(servers)
	if err = m.ServerList.SetServers(servers...); err != nil {
		return err
	}
	return nil
}

type DataConfig struct {
	Memcache []MemcacheClientConfig `json:"memcache"`
}

type MemcacheClientConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}