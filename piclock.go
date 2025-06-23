package main

import (
	"flag"
	"fmt"
	"math"
	"sync"
	"time"

	"piclock/functions"

	rpio "github.com/stianeikeland/go-rpio/v4"
)

// GPIO Constants
const TICK_PIN1 = 12
const TICK_PIN2 = 13

// Timing Constants
const FORWARD_TICK_DURATION_MS = 32
const FORWARD_TICK_DELAY_MS = 32
const FORWARD_TICK_DUTY_CYCLE = 60 // 60% duty cycle for forward ticking

const REVERSE_TICK_SHORT_DURATION_MS = 10
const REVERSE_TICK_SHORT_DELAY_MS = 7
const REVERSE_TICK_LONG_DURATION_MS = 28
const REVERSE_TICK_LONG_DELAY_MS = 0
const REVERSE_TICK_DUTY_CYCLE_A = 90  // 90% duty cycle for region A (35-55 seconds)
const REVERSE_TICK_DUTY_CYCLE_B = 82  // 82% duty cycle for region B (all other positions)
const REVERSE_TICK_REGION_A_LOW = 35  // Start of region A
const REVERSE_TICK_REGION_A_HIGH = 55 // End of region A

// PWM Configuration
const PWM_FREQUENCY = 10000 // 10 kHz PWM frequency
const PWM_RANGE = 100       // 0-100 range for duty cycle

// NTP Constants
const NTP_SYNC_INTERVAL_SECONDS = 3600

// Clock status variables
var (
	fast_forward = false
	paused       = false
	reverse      = false
	time_diff    = 0 // Store the time difference in seconds
	mutex        = &sync.Mutex{}
)

// Clock hand positions
var (
	clock_hour   int = 0
	clock_minute int = 0
	clock_second int = 0
)

// Simple pulse function without PWM
func send_pulse(pin rpio.Pin, duration_ms int, next_tick_delay_ms int) {
	pin.Output()
	pin.High()
	fmt.Printf("Simple pulse: pin=%d, duration=%dms\n", pin, duration_ms)
	time.Sleep(time.Duration(duration_ms) * time.Millisecond)
	pin.Low()
	time.Sleep(time.Duration(next_tick_delay_ms) * time.Millisecond)
}

func forward_tick(current_tick_pin *rpio.Pin, tick_pin_1 rpio.Pin, tick_pin_2 rpio.Pin) {
	// Try simple pulse first
	send_pulse(*current_tick_pin, FORWARD_TICK_DURATION_MS, FORWARD_TICK_DELAY_MS)

	// Alternate between pins
	if *current_tick_pin == tick_pin_1 {
		*current_tick_pin = tick_pin_2
	} else {
		*current_tick_pin = tick_pin_1
	}
}

func reverse_tick(current_tick_pin *rpio.Pin, tick_pin_1 rpio.Pin, tick_pin_2 rpio.Pin, position int) {
	// First pulse (10ms) on current pin
	send_pulse(*current_tick_pin, REVERSE_TICK_SHORT_DURATION_MS, REVERSE_TICK_SHORT_DELAY_MS)

	// Switch pins
	if *current_tick_pin == tick_pin_2 {
		*current_tick_pin = tick_pin_1
	} else {
		*current_tick_pin = tick_pin_2
	}

	// Second pulse (28ms) on new pin
	send_pulse(*current_tick_pin, REVERSE_TICK_LONG_DURATION_MS, REVERSE_TICK_LONG_DELAY_MS)
}

func update_clock_position(clock_hour int, clock_minute int, clock_second int, reverse bool) (int, int, int) {
	mutex.Lock()
	defer mutex.Unlock()

	if reverse == true {
		clock_second = (clock_second - 1)
		if clock_second < 0 {
			clock_second = 59
			clock_minute = (clock_minute - 1)
			if clock_minute < 0 {
				clock_minute = 59
				clock_hour = (clock_hour - 1)
				if clock_hour < 0 {
					clock_hour = 11
				}
			}
		}
	} else {
		clock_second = (clock_second + 1)
		if clock_second >= 60 {
			clock_second = 0
			clock_minute = (clock_minute + 1)
			if clock_minute >= 60 {
				clock_minute = 0
				clock_hour = (clock_hour + 1)
				if clock_hour >= 12 {
					clock_hour = 0
				}
			}
		}
	}

	return clock_hour, clock_minute, clock_second
}

func calculate_time_difference(rtc_hour, rtc_minute, rtc_second int) int {
	mutex.Lock()
	defer mutex.Unlock()

	// Convert clock hour to 24-hour format for comparison
	var clock_hour_24 int
	if clock_hour == 12 {
		if rtc_hour < 12 {
			clock_hour_24 = 0
		} else {
			clock_hour_24 = 12
		}
	} else {
		clock_hour_24 = clock_hour
		if rtc_hour >= 12 {
			clock_hour_24 += 12
		}
	}

	// Calculate total seconds for each time
	rtc_total_seconds := rtc_hour*3600 + rtc_minute*60 + rtc_second
	clock_total_seconds := clock_hour_24*3600 + clock_minute*60 + clock_second

	// Calculate difference
	total_seconds_diff := rtc_total_seconds - clock_total_seconds

	// Account for 12-hour wraparound
	if total_seconds_diff > 21600 {
		total_seconds_diff -= 43200
	} else if total_seconds_diff < -21600 {
		total_seconds_diff += 43200
	}

	return total_seconds_diff
}

func synchronize_clock(ds3231 *functions.DS3231) {
	// Read time from RTC
	rtc_hour, rtc_minute, rtc_second, err := ds3231.ReadTime()
	if err != nil {
		fmt.Println("Error reading from DS3231:", err)
		return
	}

	// Calculate time difference
	diff := calculate_time_difference(rtc_hour, rtc_minute, rtc_second)

	mutex.Lock()
	// Store the difference in the global variable
	time_diff = diff
	fmt.Printf("RTC time: %02d:%02d:%02d\n", rtc_hour, rtc_minute, rtc_second)
	fmt.Printf("Clock position: %02d:%02d:%02d\n", clock_hour, clock_minute, clock_second)
	fmt.Printf("Difference: %d seconds\n", diff)
	mutex.Unlock()

	tolerance := 1

	if math.Abs(float64(diff)) <= float64(tolerance) {
		fmt.Println("Clock is in sync with RTC time")
		fast_forward = false
		reverse = false
		return
	} else if diff > tolerance {
		fmt.Printf("Clock is behind RTC time by %d seconds\n", diff)
		fast_forward = true
		reverse = false
	} else {
		fmt.Printf("Clock is ahead of RTC time by %d seconds\n", -diff)
		fast_forward = false
		reverse = true // Set reverse flag when clock is ahead
		return
	}
}

func continuous_sync_rtc_with_ntp(ds3231 *functions.DS3231, ntpSyncer *functions.NTPSyncer) {
	for {
		// Try to sync RTC with NTP
		fmt.Println("Syncing RTC with NTP...")
		ntp_hour, ntp_minute, ntp_second, err := ntpSyncer.SyncTime()
		if err == nil {
			// Update RTC with NTP time
			err = ds3231.WriteTime(ntp_hour, ntp_minute, ntp_second)
			if err != nil {
				fmt.Printf("Error writing time to DS3231: %v\n", err)
			} else {
				fmt.Printf("RTC synchronized with NTP: %02d:%02d:%02d\n",
					ntp_hour, ntp_minute, ntp_second)
			}
		} else {
			fmt.Printf("Failed to get NTP time: %v\n", err)
		}

		// Sleep before next sync
		time.Sleep(NTP_SYNC_INTERVAL_SECONDS * time.Second)
	}
}

func main() {
	// Define command line flags for setting time
	hourFlag := flag.Int("hour", -1, "Set clock hour (0-11)")
	minuteFlag := flag.Int("minute", -1, "Set clock minute (0-59)")
	secondFlag := flag.Int("second", -1, "Set clock second (0-59)")

	// Parse command line flags
	flag.Parse()

	fmt.Println("Starting PiClock")

	// Initialize GPIO
	err := rpio.Open()
	if err != nil {
		fmt.Println("Error opening GPIO:", err)
		return
	}
	defer rpio.Close()

	// Initialize DS3231 RTC
	ds3231, err := functions.NewDS3231(1) // Using I2C bus 1
	if err != nil {
		fmt.Println("Error opening DS3231:", err)
		return
	}
	defer ds3231.Close()

	// Check if DS3231 is available
	if !ds3231.IsAvailable() {
		fmt.Println("DS3231 not found or not responding")
		return
	}

	// Initialize FRAM
	fram, err := functions.NewFRAM(1) // Using I2C bus 1
	if err != nil {
		fmt.Println("Error opening FRAM:", err)
	}
	defer fram.Close()

	// Initialize NTP syncer
	ntpSyncer := functions.NewNTPSyncer("") // Empty string means use default server

	// Initialize pins outside the loop
	tick_pin_1 := rpio.Pin(TICK_PIN1)
	tick_pin_2 := rpio.Pin(TICK_PIN2)
	tick_pin_1.Output()
	tick_pin_2.Output()
	current_tick_pin := tick_pin_1

	// Check if time flags were provided to set clock time
	time_reset := false
	if *hourFlag >= 0 && *hourFlag <= 11 &&
		*minuteFlag >= 0 && *minuteFlag <= 59 &&
		*secondFlag >= 0 && *secondFlag <= 59 {
		// Set clock time from flags
		clock_hour = *hourFlag
		clock_minute = *minuteFlag
		clock_second = *secondFlag

		// Update FRAM with the new time
		err = fram.WriteTime(clock_hour, clock_minute, clock_second)
		if err != nil {
			fmt.Println("Error setting initial time in FRAM:", err)
		} else {
			fmt.Printf("Clock initialized from flags to %02d:%02d:%02d\n",
				clock_hour, clock_minute, clock_second)
			time_reset = true
		}
	}

	// If no flags were provided or setting time failed, read from FRAM
	if !time_reset {
		stored_hour, stored_minute, stored_second, err := fram.ReadTime()
		if err == nil {
			// Successfully read time from FRAM
			clock_hour = stored_hour
			clock_minute = stored_minute
			clock_second = stored_second
			fmt.Println("Loaded time from FRAM:", clock_hour, ":", clock_minute, ":", clock_second)
		} else {
			fmt.Println("No valid time data in FRAM, initializing to 00:00:00")
		}
	}

	// Perform initial NTP sync
	fmt.Println("Performing initial NTP sync...")
	ntp_hour, ntp_minute, ntp_second, err := ntpSyncer.SyncTime()
	if err == nil {
		// Update RTC with NTP time
		err = ds3231.WriteTime(ntp_hour, ntp_minute, ntp_second)
		if err != nil {
			fmt.Printf("Error writing time to DS3231: %v\n", err)
		} else {
			fmt.Printf("RTC initialized with NTP: %02d:%02d:%02d\n",
				ntp_hour, ntp_minute, ntp_second)
		}
	} else {
		fmt.Printf("Failed to get initial NTP time: %v\n", err)
	}

	// Start a goroutine to continuously sync RTC with NTP
	go continuous_sync_rtc_with_ntp(ds3231, ntpSyncer)

	for {
		mutex.Lock()
		is_paused := paused
		is_fast_forward := fast_forward
		mutex.Unlock()

		if !is_paused {
			// Synchronize with RTC first
			synchronize_clock(ds3231)

			mutex.Lock()
			is_fast_forward = fast_forward
			is_reverse := reverse
			mutex.Unlock()

			// Perform tick based on synchronization result
			if is_reverse {
				fmt.Println("Ticking in reverse")
				reverse_tick(&current_tick_pin, tick_pin_1, tick_pin_2, clock_second)
				// Fix: Pass all required arguments to update_clock_position
				clock_hour, clock_minute, clock_second = update_clock_position(clock_hour, clock_minute, clock_second, true)
			} else {
				fmt.Println("Ticking forward")
				forward_tick(&current_tick_pin, tick_pin_1, tick_pin_2)
				// Fix: Pass all required arguments to update_clock_position
				clock_hour, clock_minute, clock_second = update_clock_position(clock_hour, clock_minute, clock_second, false)
			}

			// Save time to FRAM every second
			err := fram.WriteTime(clock_hour, clock_minute, clock_second)
			if err != nil {
				fmt.Println("Error saving time to FRAM:", err)
			}

			fmt.Printf("Clock Time: %02d:%02d:%02d\n", clock_hour, clock_minute, clock_second)
		} else {
			fmt.Println("Clock is paused")
		}

		// Adjust sleep time based on fast forward flag and difference
		if is_fast_forward {
			mutex.Lock()
			diff_value := time_diff // Capture the diff value safely inside the mutex
			mutex.Unlock()

			if diff_value > 60 {
				time.Sleep(125 * time.Millisecond) // 8 ticks per second for large differences
			} else {
				time.Sleep(250 * time.Millisecond) // 4 ticks per second for small differences
			}
		} else {
			time.Sleep(1 * time.Second)
		}

	}
}
