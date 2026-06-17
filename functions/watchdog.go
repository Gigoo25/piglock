package functions

import (
	"net"
	"os"
	"strings"
)

// Watchdog speaks the systemd sd_notify protocol over $NOTIFY_SOCKET. It lets
// the service tell systemd it is alive; if the heartbeat stops, systemd kills
// and restarts the unit (Power-of-Ten threat: crash/hang auto-recovery).
//
// When not launched under systemd notify (no socket), every method is a no-op,
// so the binary still runs standalone.
type Watchdog struct {
	conn *net.UnixConn
}

// NewWatchdog connects to the systemd notify socket if one is present.
func NewWatchdog() (*Watchdog, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return &Watchdog{}, nil
	}
	name := addr
	if strings.HasPrefix(name, "@") {
		// Abstract namespace sockets start with a NUL byte.
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	return &Watchdog{conn: conn}, nil
}

// notify sends one sd_notify state line.
func (w *Watchdog) notify(state string) error {
	if w == nil || w.conn == nil {
		return nil
	}
	if err := Assert(state != "", "notify state non-empty"); err != nil {
		return err
	}
	_, err := w.conn.Write([]byte(state))
	return err
}

// Ready announces successful startup (required by Type=notify).
func (w *Watchdog) Ready() error { return w.notify("READY=1") }

// Alive sends a watchdog keep-alive ping.
func (w *Watchdog) Alive() error { return w.notify("WATCHDOG=1") }

// Stopping announces a clean shutdown.
func (w *Watchdog) Stopping() error { return w.notify("STOPPING=1") }

// Close releases the notify socket.
func (w *Watchdog) Close() {
	if w != nil && w.conn != nil {
		_ = w.conn.Close()
	}
}
