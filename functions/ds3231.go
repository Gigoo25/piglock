package functions

import (
	"fmt"
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
	DS3231_REG_A1SEC  = 0x07 // Alarm 1 seconds
	DS3231_REG_CTRL   = 0x0E // Control register
	DS3231_REG_STATUS = 0x0F // Status register
	DS3231_REG_TEMP_H = 0x11 // Temperature register (high byte)
	DS3231_REG_TEMP_L = 0x12 // Temperature register (low byte)

	HOUR_12_BIT = 0x40 // 12-hour mode bit
	HOUR_PM_BIT = 0x20 // PM bit in 12-hour mode
	HOUR_MASK   = 0x1F // Mask for hours in 12-hour mode
)

// DS3231 represents a connection to a DS3231 RTC chip
type DS3231 struct {
	device *i2c.I2C
}

// NewDS3231 creates a new connection to the DS3231 RTC
func NewDS3231(bus int) (*DS3231, error) {
	// Disable debug output for i2c
	logger.ChangePackageLogLevel("i2c", logger.ErrorLevel)

	device, err := i2c.NewI2C(DS3231_ADDRESS, bus)
	if err != nil {
		return nil, fmt.Errorf("error opening DS3231: %v", err)
	}

	return &DS3231{device: device}, nil
}

// Close closes the connection to the DS3231
func (d *DS3231) Close() {
	if d.device != nil {
		d.device.Close()
	}
}

// bcdToDec converts Binary Coded Decimal to decimal
func bcdToDec(bcd byte) int {
	return int((bcd/16)*10 + (bcd % 16))
}

// decToBcd converts decimal to Binary Coded Decimal
func decToBcd(dec int) byte {
	return byte(((dec / 10) << 4) | (dec % 10))
}

// IsAvailable checks if the DS3231 is available on the I2C bus
func (d *DS3231) IsAvailable() bool {
	// Try to read the seconds register
	data, _, err := d.device.ReadRegBytes(DS3231_REG_SECOND, 1)
	return err == nil && len(data) > 0
}

// GetFullTimeData reads the full time data from DS3231, includes year, month, date
func (d *DS3231) GetFullTimeData() (*time.Time, error) {
	// Read all time registers at once (7 bytes from 0x00 to 0x06)
	data, _, err := d.device.ReadRegBytes(DS3231_REG_SECOND, 7)
	if err != nil {
		return nil, fmt.Errorf("error reading full time data from DS3231: %v", err)
	}

	second := bcdToDec(data[0] & 0x7F)
	minute := bcdToDec(data[1] & 0x7F)

	// Handle 12/24 hour modes
	var hour int
	if (data[2] & HOUR_12_BIT) != 0 {
		// 12-hour mode
		hour = bcdToDec(data[2] & HOUR_MASK)
		if (data[2]&HOUR_PM_BIT) != 0 && hour != 12 {
			hour += 12
		}
		if hour == 12 && (data[2]&HOUR_PM_BIT) == 0 {
			hour = 0
		}
	} else {
		// 24-hour mode
		hour = bcdToDec(data[2] & 0x3F)
	}

	// Not using day variable - remove it to fix the unused variable error
	// day := bcdToDec(data[3])
	date := bcdToDec(data[4])
	month := bcdToDec(data[5] & 0x1F)
	year := 2000 + bcdToDec(data[6])

	t := time.Date(year, time.Month(month), date, hour, minute, second, 0, time.Local)
	return &t, nil
}

// ReadTime reads just the time (hour, minute, second) from the DS3231
func (d *DS3231) ReadTime() (hour, minute, second int, err error) {
	t, err := d.GetFullTimeData()
	if err != nil {
		return 0, 0, 0, err
	}

	// Convert to 12-hour format for the clock
	hour = t.Hour() % 12
	if hour == 0 {
		hour = 12
	}
	minute = t.Minute() // Add this line
	second = t.Second() // Add this line

	fmt.Printf("Time read from DS3231: %02d:%02d:%02d\n", hour, minute, second)
	return hour, t.Minute(), t.Second(), nil
}

// SetFullTimeData sets the complete time including date on the DS3231
func (d *DS3231) SetFullTimeData(t time.Time) error {
	// Using individual register writes instead of WriteRegBytes which doesn't exist
	if err := d.device.WriteRegU8(DS3231_REG_SECOND, decToBcd(t.Second())); err != nil {
		return fmt.Errorf("error writing seconds to DS3231: %v", err)
	}
	if err := d.device.WriteRegU8(DS3231_REG_MINUTE, decToBcd(t.Minute())); err != nil {
		return fmt.Errorf("error writing minutes to DS3231: %v", err)
	}
	if err := d.device.WriteRegU8(DS3231_REG_HOUR, decToBcd(t.Hour())); err != nil {
		return fmt.Errorf("error writing hours to DS3231: %v", err)
	}
	if err := d.device.WriteRegU8(DS3231_REG_DAY, decToBcd(int(t.Weekday())+1)); err != nil {
		return fmt.Errorf("error writing day to DS3231: %v", err)
	}
	if err := d.device.WriteRegU8(DS3231_REG_DATE, decToBcd(t.Day())); err != nil {
		return fmt.Errorf("error writing date to DS3231: %v", err)
	}
	if err := d.device.WriteRegU8(DS3231_REG_MONTH, decToBcd(int(t.Month()))); err != nil {
		return fmt.Errorf("error writing month to DS3231: %v", err)
	}
	if err := d.device.WriteRegU8(DS3231_REG_YEAR, decToBcd(t.Year()%100)); err != nil {
		return fmt.Errorf("error writing year to DS3231: %v", err)
	}

	fmt.Printf("Full time written to DS3231: %s\n", t.Format("2006-01-02 15:04:05"))
	return nil
}

// WriteTime writes just the time part to the DS3231
func (d *DS3231) WriteTime(hour, minute, second int) error {
	// Get current full time first
	currentTime, err := d.GetFullTimeData()
	if err != nil {
		// If we can't read the current time, just use today's date
		now := time.Now()
		currentTime = &now
	}

	// Create new time with updated hour, minute, second
	newTime := time.Date(
		currentTime.Year(),
		currentTime.Month(),
		currentTime.Day(),
		hour,
		minute,
		second,
		0,
		currentTime.Location(),
	)

	// Write the full time data
	return d.SetFullTimeData(newTime)
}
