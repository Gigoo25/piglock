package functions

import (
	"fmt"

	i2c "github.com/d2r2/go-i2c"
	logger "github.com/d2r2/go-logger"
)

// FRAM Constants
const (
	FRAM_ADDRESS     = 0x50           // Default I2C address for MB85RC256V
	FRAM_TIME_ADDR   = 0x0000         // Memory address to store time data
	FRAM_MAGIC_ADDR  = 0x0010         // Address to store magic number (for validation)
	FRAM_MAGIC_VALUE = uint16(0xA55A) // Magic value to check if FRAM has valid data
)

// init function runs when the package is initialized
func init() {
	// Disable all I2C debug output
	logger.ChangePackageLogLevel("i2c", logger.ErrorLevel)
}

// FRAM represents a connection to an MB85RC256V FRAM chip
type FRAM struct {
	device *i2c.I2C
}

// NewFRAM creates a new connection to the FRAM chip
func NewFRAM(bus int) (*FRAM, error) {
	device, err := i2c.NewI2C(FRAM_ADDRESS, bus)
	if err != nil {
		return nil, fmt.Errorf("error opening FRAM: %v", err)
	}
	return &FRAM{device: device}, nil
}

// Close closes the connection to the FRAM chip
func (f *FRAM) Close() {
	if f.device != nil {
		f.device.Close()
	}
}

// WriteTime writes the current clock time and magic value to FRAM
func (f *FRAM) WriteTime(hour, minute, second int) error {
	// Store time data (3 bytes: hour, minute, second)
	timeData := []byte{byte(hour), byte(minute), byte(second)}

	// First set the address where to write the time data
	// For the MB85RC256V, we need to write the memory address before the data
	addrBytes := []byte{byte(FRAM_TIME_ADDR >> 8), byte(FRAM_TIME_ADDR & 0xFF)}

	// Write address + data in a single operation
	dataToWrite := append(addrBytes, timeData...)
	_, err := f.device.WriteBytes(dataToWrite)
	if err != nil {
		return fmt.Errorf("error writing time data to FRAM: %v", err)
	}

	// Only write the magic value if we haven't done so already
	// This optimization avoids unnecessary writes to the magic value address
	if !f.HasValidData() {
		// Now write the magic value for validation (2 bytes)
		magicAddrBytes := []byte{byte(FRAM_MAGIC_ADDR >> 8), byte(FRAM_MAGIC_ADDR & 0xFF)}
		magicData := []byte{byte(FRAM_MAGIC_VALUE >> 8), byte(FRAM_MAGIC_VALUE & 0xFF)}

		magicToWrite := append(magicAddrBytes, magicData...)
		_, err = f.device.WriteBytes(magicToWrite)
		if err != nil {
			return fmt.Errorf("error writing magic value to FRAM: %v", err)
		}
		fmt.Println("Magic value initialized in FRAM")
	}

	// Only print time updates periodically to reduce console spam
	if second == 0 || second == 30 {
		fmt.Printf("Time data written to FRAM: %02d:%02d:%02d\n", hour, minute, second)
	}

	return nil
}

// ReadTime reads the stored time from FRAM if valid
func (f *FRAM) ReadTime() (hour, minute, second int, err error) {
	// First check if valid time data exists by reading the magic value
	if !f.HasValidData() {
		return 0, 0, 0, fmt.Errorf("no valid time data in FRAM")
	}

	// Set the address to read from
	addrBytes := []byte{byte(FRAM_TIME_ADDR >> 8), byte(FRAM_TIME_ADDR & 0xFF)}
	_, err = f.device.WriteBytes(addrBytes)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("error setting read address: %v", err)
	}

	// Read 3 bytes (hour, minute, second)
	timeData := make([]byte, 3)
	_, err = f.device.ReadBytes(timeData)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("error reading time data from FRAM: %v", err)
	}

	hour = int(timeData[0])
	minute = int(timeData[1])
	second = int(timeData[2])

	// Validate the ranges
	if hour < 0 || hour > 11 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return 0, 0, 0, fmt.Errorf("invalid time values read from FRAM")
	}

	fmt.Printf("Time data read from FRAM: %02d:%02d:%02d\n", hour, minute, second)
	return hour, minute, second, nil
}

// HasValidData checks if valid data exists in FRAM by verifying the magic value
func (f *FRAM) HasValidData() bool {
	// Set the address to read the magic value
	addrBytes := []byte{byte(FRAM_MAGIC_ADDR >> 8), byte(FRAM_MAGIC_ADDR & 0xFF)}
	_, err := f.device.WriteBytes(addrBytes)
	if err != nil {
		fmt.Printf("Error setting magic value read address: %v\n", err)
		return false
	}

	// Read 2 bytes for the magic value
	magicData := make([]byte, 2)
	_, err = f.device.ReadBytes(magicData)
	if err != nil {
		fmt.Printf("Error reading magic value from FRAM: %v\n", err)
		return false
	}

	// Convert the bytes to uint16
	readMagic := uint16(magicData[0])<<8 | uint16(magicData[1])

	// Check if it matches the expected magic value
	return readMagic == FRAM_MAGIC_VALUE
}
