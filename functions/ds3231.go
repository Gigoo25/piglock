package functions

import (
	"fmt"
	"sync"
	"time"

	i2c "github.com/d2r2/go-i2c"
	logger "github.com/d2r2/go-logger"
)

// DS3231 Constants
const (
	DS3231_ADDRESS    = 0x68 // I2C address for DS3231
	DS3231_REG_SECOND = 0x00 // Seconds register
	DS3231_REG_MINUTE = 0x01 // Minutes register
	DS3231_REG_HOUR   = 0x02 // Hours register
	DS3231_REG_DAY    = 0x03 // Day of week register
	DS3231_REG_DATE   = 0x04 // Date register
	DS3231_REG_MONTH  = 0x05 // Month register
	DS3231_REG_YEAR   = 0x06 // Year register
	DS3231_REG_CTRL   = 0x0E // Control register
	DS3231_REG_STATUS = 0x0F // Status register

	HOUR_12_BIT = 0x40 // 12-hour mode bit
	HOUR_PM_BIT = 0x20 // PM bit in 12-hour mode
	HOUR_MASK   = 0x1F // Mask for hours in 12-hour mode

	DS3231_OSF_BIT  = 0x80 // Oscillator Stop Flag, in status register
	DS3231_EOSC_BIT = 0x80 // Enable Oscillator (active low), in control register

	ds3231Retries = 3 // bounded retry count for transient I2C errors
)

// DS3231 represents a connection to a DS3231 RTC chip. All I2C access is
// serialized by mu so the tick loop and the NTP goroutine can share it safely.
type DS3231 struct {
	mu     sync.Mutex
	device *i2c.I2C
	bus    int
}

// NewDS3231 creates a new connection to the DS3231 RTC.
func NewDS3231(bus int) (*DS3231, error) {
	if err := Assert(bus >= 0, "i2c bus non-negative"); err != nil {
		return nil, err
	}
	logger.ChangePackageLogLevel("i2c", logger.ErrorLevel)
	device, err := i2c.NewI2C(DS3231_ADDRESS, bus)
	if err != nil {
		return nil, fmt.Errorf("error opening DS3231: %v", err)
	}
	if err := Assert(device != nil, "i2c device non-nil"); err != nil {
		return nil, err
	}
	return &DS3231{device: device, bus: bus}, nil
}

// Reopen closes and reopens the underlying I2C device. It recovers from a wedged
// bus or stale file descriptor (e.g. persistent EIO) without restarting the
// process. All other access is serialized by d.mu, so a concurrent caller (the
// NTP goroutine) is safe across the swap.
func (d *DS3231) Reopen() error {
	if err := Assert(d.bus >= 0, "i2c bus non-negative"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.device != nil {
		d.device.Close()
		d.device = nil
	}
	device, err := i2c.NewI2C(DS3231_ADDRESS, d.bus)
	if err != nil {
		return fmt.Errorf("reopen DS3231: %v", err)
	}
	if err := Assert(device != nil, "i2c device non-nil after reopen"); err != nil {
		return err
	}
	d.device = device
	return nil
}

// Close closes the connection to the DS3231.
func (d *DS3231) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.device != nil {
		d.device.Close()
	}
}

// bcdToDec converts Binary Coded Decimal to decimal.
func bcdToDec(bcd byte) int {
	return int((bcd/16)*10 + (bcd % 16))
}

// decToBcd converts decimal to Binary Coded Decimal.
func decToBcd(dec int) byte {
	return byte(((dec / 10) << 4) | (dec % 10))
}

// readRegLocked reads count bytes from reg, retrying transient errors. Caller
// holds d.mu. Each loop has a fixed bound (ds3231Retries), per Power-of-Ten r2.
func (d *DS3231) readRegLocked(reg byte, count int) ([]byte, error) {
	if err := Assert(count > 0 && count <= 8, "read count in 1..8"); err != nil {
		return nil, err
	}
	if d.device == nil {
		return nil, fmt.Errorf("DS3231 device not open")
	}
	var lastErr error
	for range ds3231Retries {
		data, _, err := d.device.ReadRegBytes(reg, count)
		if err == nil {
			if err := Assert(len(data) >= count, "short register read"); err != nil {
				return nil, err
			}
			return data, nil
		}
		lastErr = err
		time.Sleep(5 * time.Millisecond)
	}
	return nil, fmt.Errorf("DS3231 read reg 0x%02X failed: %v", reg, lastErr)
}

// IsAvailable checks if the DS3231 is responding on the I2C bus.
func (d *DS3231) IsAvailable() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.readRegLocked(DS3231_REG_SECOND, 1)
	return err == nil
}

// fullTimeLocked reads the full date+time. Caller holds d.mu.
func (d *DS3231) fullTimeLocked() (time.Time, error) {
	data, err := d.readRegLocked(DS3231_REG_SECOND, 7)
	if err != nil {
		return time.Time{}, err
	}
	second := bcdToDec(data[0] & 0x7F)
	minute := bcdToDec(data[1] & 0x7F)

	var hour int
	if (data[2] & HOUR_12_BIT) != 0 {
		hour = bcdToDec(data[2] & HOUR_MASK)
		if (data[2]&HOUR_PM_BIT) != 0 && hour != 12 {
			hour += 12
		}
		if hour == 12 && (data[2]&HOUR_PM_BIT) == 0 {
			hour = 0
		}
	} else {
		hour = bcdToDec(data[2] & 0x3F)
	}

	date := bcdToDec(data[4])
	month := bcdToDec(data[5] & 0x1F)
	year := 2000 + bcdToDec(data[6])

	if err := Assert(hour >= 0 && hour <= 23, "rtc hour in range"); err != nil {
		return time.Time{}, err
	}
	if err := Assert(minute >= 0 && minute <= 59 && second >= 0 && second <= 59, "rtc min/sec in range"); err != nil {
		return time.Time{}, err
	}
	return time.Date(year, time.Month(month), date, hour, minute, second, 0, time.Local), nil
}

// ReadClock returns the current 24-hour time (hour 0..23) from the DS3231.
func (d *DS3231) ReadClock() (hour, minute, second int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.fullTimeLocked()
	if err != nil {
		return 0, 0, 0, err
	}
	return t.Hour(), t.Minute(), t.Second(), nil
}

// setFullTimeLocked writes the complete time in 24-hour mode. Caller holds d.mu.
func (d *DS3231) setFullTimeLocked(t time.Time) error {
	if d.device == nil {
		return fmt.Errorf("DS3231 device not open")
	}
	writes := []struct {
		reg byte
		val byte
	}{
		{DS3231_REG_SECOND, decToBcd(t.Second())},
		{DS3231_REG_MINUTE, decToBcd(t.Minute())},
		{DS3231_REG_HOUR, decToBcd(t.Hour())}, // bit6 clear => 24-hour mode
		{DS3231_REG_DAY, decToBcd(int(t.Weekday()) + 1)},
		{DS3231_REG_DATE, decToBcd(t.Day())},
		{DS3231_REG_MONTH, decToBcd(int(t.Month()))},
		{DS3231_REG_YEAR, decToBcd(t.Year() % 100)},
	}
	for _, w := range writes {
		if err := d.device.WriteRegU8(w.reg, w.val); err != nil {
			return fmt.Errorf("error writing DS3231 reg 0x%02X: %v", w.reg, err)
		}
	}
	return nil
}

// WriteTime writes the time-of-day, preserving the stored date.
func (d *DS3231) WriteTime(hour, minute, second int) error {
	if err := Assert(hour >= 0 && hour <= 23, "write hour in range"); err != nil {
		return err
	}
	if err := Assert(minute >= 0 && minute <= 59 && second >= 0 && second <= 59, "write min/sec in range"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current, err := d.fullTimeLocked()
	if err != nil {
		current = time.Now()
	}
	newTime := time.Date(current.Year(), current.Month(), current.Day(),
		hour, minute, second, 0, current.Location())
	return d.setFullTimeLocked(newTime)
}

// OscillatorStopped reports whether the RTC lost time (OSF set). A read error is
// reported as stopped, so the caller treats an unreadable RTC as untrusted.
func (d *DS3231) OscillatorStopped() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := d.readRegLocked(DS3231_REG_STATUS, 1)
	if err != nil {
		return true, err
	}
	return data[0]&DS3231_OSF_BIT != 0, nil
}

// ClearOSF clears the Oscillator Stop Flag after the time has been re-trusted.
func (d *DS3231) ClearOSF() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := d.readRegLocked(DS3231_REG_STATUS, 1)
	if err != nil {
		return err
	}
	return d.device.WriteRegU8(DS3231_REG_STATUS, data[0]&^byte(DS3231_OSF_BIT))
}

// EnsureRunning makes sure the oscillator keeps running on battery (EOSC clear).
func (d *DS3231) EnsureRunning() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := d.readRegLocked(DS3231_REG_CTRL, 1)
	if err != nil {
		return err
	}
	if data[0]&DS3231_EOSC_BIT != 0 {
		return d.device.WriteRegU8(DS3231_REG_CTRL, data[0]&^byte(DS3231_EOSC_BIT))
	}
	return nil
}
