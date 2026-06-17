package functions

import (
	"fmt"
	"time"

	"github.com/beevik/ntp"
)

// NTP Constants
const (
	DEFAULT_NTP_SERVER          = "pool.ntp.org" // Default NTP server
	NTP_SYNC_INTERVAL_MINUTES   = 60             // Sync with NTP every hour
	NTP_RETRY_INTERVAL_SECONDS  = 30             // Retry after 30 seconds if NTP sync fails
	NTP_MAX_RETRIES             = 5              // Maximum number of retries before giving up
	NTP_TIME_DIFF_THRESHOLD_SEC = 2              // Threshold for adjusting time (in seconds)
)

// ntpFallbackServers are tried in order so one dead server cannot block a sync.
var ntpFallbackServers = []string{
	"pool.ntp.org",
	"time.google.com",
	"time.cloudflare.com",
	"time.nist.gov",
}

// NTPSyncer handles time synchronization with NTP servers
type NTPSyncer struct {
	server        string
	lastSyncTime  time.Time
	syncInterval  time.Duration
	retryInterval time.Duration
	maxRetries    int
	diffThreshold time.Duration
	retryCount    int
	isSyncing     bool
	debugMode     bool
}

// NewNTPSyncer creates a new NTP synchronization handler
func NewNTPSyncer(server string) *NTPSyncer {
	if server == "" {
		server = DEFAULT_NTP_SERVER
	}

	return &NTPSyncer{
		server:        server,
		lastSyncTime:  time.Time{}, // Zero time means never synced
		syncInterval:  time.Duration(NTP_SYNC_INTERVAL_MINUTES) * time.Minute,
		retryInterval: time.Duration(NTP_RETRY_INTERVAL_SECONDS) * time.Second,
		maxRetries:    NTP_MAX_RETRIES,
		diffThreshold: time.Duration(NTP_TIME_DIFF_THRESHOLD_SEC) * time.Second,
		retryCount:    0,
		isSyncing:     false,
		debugMode:     false,
	}
}

// EnableDebug turns on verbose logging
func (n *NTPSyncer) EnableDebug(debug bool) {
	n.debugMode = debug
}

// GetCurrentTime returns the current time from the first NTP server that
// answers. The configured server is tried first, then the fallback list. The
// loop has a fixed upper bound (Power-of-Ten rule 2).
func (n *NTPSyncer) GetCurrentTime() (time.Time, error) {
	servers := append([]string{n.server}, ntpFallbackServers...)
	var lastErr error
	for _, server := range servers {
		if server == "" {
			continue
		}
		response, err := ntp.Query(server)
		if err != nil {
			lastErr = err
			continue
		}
		if err := response.Validate(); err != nil {
			lastErr = err
			continue
		}
		return time.Now().Add(response.ClockOffset), nil
	}
	return time.Time{}, fmt.Errorf("all NTP servers failed: %v", lastErr)
}

// ShouldSync returns true if it's time to sync with NTP
func (n *NTPSyncer) ShouldSync() bool {
	// If we've never synced or if the sync interval has elapsed
	return n.lastSyncTime.IsZero() || time.Since(n.lastSyncTime) >= n.syncInterval
}

// SyncTime synchronizes the time with an NTP server and returns the hours, minutes, seconds
func (n *NTPSyncer) SyncTime() (int, int, int, error) {
	if n.isSyncing {
		return 0, 0, 0, fmt.Errorf("NTP sync already in progress")
	}

	n.isSyncing = true
	defer func() { n.isSyncing = false }()

	ntpTime, err := n.GetCurrentTime()
	if err != nil {
		n.retryCount++
		if n.retryCount > n.maxRetries {
			n.retryCount = 0
			return 0, 0, 0, fmt.Errorf("NTP sync failed after %d retries: %v", n.maxRetries, err)
		}
		return 0, 0, 0, fmt.Errorf("NTP sync error (retry %d/%d): %v",
			n.retryCount, n.maxRetries, err)
	}

	// Reset retry count on successful sync
	n.retryCount = 0
	n.lastSyncTime = time.Now()

	// Convert to 12-hour format
	hour := ntpTime.Hour() % 12
	minute := ntpTime.Minute()
	second := ntpTime.Second()

	if n.debugMode {
		fmt.Printf("NTP Sync Complete - Time: %d:%02d:%02d\n", hour, minute, second)
	}

	return hour, minute, second, nil
}

// GetSyncStatus returns information about the NTP sync status
func (n *NTPSyncer) GetSyncStatus() string {
	if n.lastSyncTime.IsZero() {
		return "Never synced"
	}

	return fmt.Sprintf("Last sync: %s (%s ago)",
		n.lastSyncTime.Format("15:04:05"),
		time.Since(n.lastSyncTime).Round(time.Second))
}
