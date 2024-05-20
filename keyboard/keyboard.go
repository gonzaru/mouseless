package keyboard

import (
	"fmt"
	"github.com/jbensmann/mouseless/config"
	"time"

	evdev "github.com/gvalkov/golang-evdev"
	log "github.com/sirupsen/logrus"
)

type Event struct {
	Code    uint16
	IsPress bool
	Time    time.Time
}

type Device struct {
	deviceName    string
	device        *evdev.InputDevice
	state         DeviceState
	lastOpenError string
	eventChan     chan<- Event
}

type DeviceState int

const (
	StateNotOpen DeviceState = iota
	StateOpenFailed
	StateOpen
)

func NewKeyboardDevice(deviceName string, eventChan chan<- Event) *Device {
	k := Device{
		deviceName: deviceName,
		device:     nil,
		state:      StateNotOpen,
		eventChan:  eventChan,
	}
	return &k
}

// ReadLoop reads from the keyboard device in an infinite loop.
// When the device is not opened or disconnects in between, it tries to open again.
func (k *Device) ReadLoop() {
	ticker := time.NewTicker(5 * time.Second)
	for {
		if k.state != StateOpen {
			if err := k.openDevice(); err != nil {
				k.lastOpenError = fmt.Sprintf("%v", err)
				if k.state == StateOpenFailed {
					log.Debugf("Failed to open %v: %v", k.deviceName, err)
				} else {
					log.Warnf("Failed to open %v: %v", k.deviceName, err)
				}
			}
		}

		select {
		case <-ticker.C:
			continue
		}
	}
}

// openDevice tries to open and grab the keyboard device.
func (k *Device) openDevice() error {
	log.Debugf("opening the keyboard device %v", k.deviceName)

	device, err := evdev.Open(k.deviceName)
	if err != nil {
		k.state = StateOpenFailed
		return err
	}
	err = device.Grab()
	if err != nil {
		k.state = StateOpenFailed
		return err
	}

	log.Debug(device)
	log.Debugf("Device name: %s", device.Name)
	log.Debugf("Evdev protocol version: %d", device.EvdevVersion)
	info := fmt.Sprintf("bus 0x%04x, vendor 0x%04x, product 0x%04x, version 0x%04x",
		device.Bustype, device.Vendor, device.Product, device.Version)
	log.Debugf("Device info: %s", info)

	k.device = device
	k.state = StateOpen
	go k.readKeyboard()
	return nil
}

// readKeyboard reads from the device in an infinite loop.
// The device has to be opened, and if it disconnects in between this method returns and sets the state to not open.
func (k *Device) readKeyboard() {
	var events []evdev.InputEvent
	var err error
	for {
		if k.state != StateOpen {
			return
		}
		events, err = k.device.Read()
		if err != nil {
			log.Warnf("Failed to read keyboard: %v", err)
			k.state = StateNotOpen
			return
		}
		for _, event := range events {
			if event.Type == evdev.EV_KEY {
				if event.Value == 0 || event.Value == 1 {

					codeAlias, exists := config.GetKeyAlias(event.Code)
					if !exists {
						codeAlias = "?"
					}
					fmtString := "Pressed:  "
					if event.Value == 0 {
						fmtString = "Released: "
					}
					fmtString += "%s (%d)"
					log.Debugf(fmtString, codeAlias, event.Code)

					e := Event{
						Code:    event.Code,
						IsPress: event.Value == 1,
						Time:    time.Now(),
					}
					k.eventChan <- e
				}
			}
		}
	}
}

// DeviceName returns the name of the keyboard device.
func (k *Device) DeviceName() string {
	return k.deviceName
}

// IsOpen returns true if the device has been opened successfully.
func (k *Device) IsOpen() bool {
	return k.state == StateOpen
}

// LastOpenError returns the last error on opening the device.
func (k *Device) LastOpenError() string {
	return k.lastOpenError
}
