package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	evdev "github.com/gvalkov/golang-evdev"
	"github.com/jessevdk/go-flags"
	log "github.com/sirupsen/logrus"
)

const version = "0.1.5"

const (
	mouseLoopInterval = 20 * time.Millisecond
	defaultConfigFile = ".config/mouseless/config.yaml"
)

var (
	configFile      string
	config          *Config
	keyboardDevices []*KeyboardDevice
	mouse           *VirtualMouse
	keyboard        *VirtualKeyboard
	tapHoldHandler  *TapHoldHandler

	currentLayer *Layer

	// remember all keys that toggled a layer, and from which layer they came from
	toggleLayerKeys     []uint16
	toggleLayerPrevious []*Layer
)

var opts struct {
	Version    bool   `short:"v" long:"version" description:"Show the version"`
	Debug      bool   `short:"d" long:"debug" description:"Show verbose debug information"`
	ConfigFile string `short:"c" long:"config" description:"The config file"`
}

func main() {
	var err error

	_, err = flags.Parse(&opts)
	if err != nil {
		os.Exit(1)
	}

	if opts.Version {
		fmt.Println(version)
		os.Exit(0)
	}

	// init logging
	log.SetOutput(os.Stdout)
	if opts.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	// if no config file is given, use the default one
	configFile = opts.ConfigFile
	if configFile == "" {
		u, err := user.Current()
		if err != nil {
			exitError(err, "Failed to get the current user")
		}
		configFile = filepath.Join(u.HomeDir, defaultConfigFile)
	}

	log.Debugf("Using config file: %s", configFile)
	loadConfig()

	detectedKeyboardDevices := findKeyboardDevices()

	// check if another instance of mouse is already running
	for _, device := range detectedKeyboardDevices {
		if device.Name == "mouseless" {
			exitError(nil, "Found a keyboard device with name mouseless, "+
				"which probably means that another instance of mouseless is already running")
		}
	}

	// if no devices are specified, use the detected ones
	if len(config.Devices) == 0 {
		for _, device := range detectedKeyboardDevices {
			config.Devices = append(config.Devices, device.Fn)
		}
		if len(config.Devices) == 0 {
			exitError(nil, "No keyboard devices found")
		}
	}

	// init virtual mouse and keyboard
	mouse, err = NewVirtualMouse()
	if err != nil {
		exitError(err, "Failed to init the virtual mouse")
	}
	defer mouse.Close()

	keyboard, err = NewVirtualKeyboard()
	if err != nil {
		exitError(err, "Failed to init the virtual keyboard")
	}
	defer keyboard.Close()

	tapHoldHandler = NewTapHoldHandler(int64(config.QuickTapTime))

	// init keyboard devices
	for _, dev := range config.Devices {
		kd := NewKeyboardDevice(dev, tapHoldHandler.InChannel())
		keyboardDevices = append(keyboardDevices, kd)
		go kd.ReadLoop()
	}

	if config.StartCommand != "" {
		log.Debugf("Executing start command: %s", config.StartCommand)
		cmd := exec.Command("sh", "-c", config.StartCommand)
		err := cmd.Run()
		if err != nil {
			exitError(err, "Execution of start command failed")
		}
	}

	mainLoop()
}

func loadConfig() {
	var err error
	config, err = readConfig(configFile)
	if err != nil {
		exitError(err, "Failed to read the config file")
	}

	// set initial layer
	currentLayer = config.Layers[0]
	log.Debugf("Switching to initial layer %s", currentLayer.Name)
}

func mainLoop() {
	tapHoldHandler.StartProcessing()
	mouseTimer := time.NewTimer(math.MaxInt64)

	for {
		// check if a key was pressed
		var event *KeyboardEvent = nil
		select {
		case e := <-tapHoldHandler.OutChannel():
			event = &e
		case <-mouseTimer.C:
		}
		if event != nil {
			handleKey(event)
		}

		// check if at least one device is opened
		oneDeviceOpen := false
		for _, device := range keyboardDevices {
			if device.IsOpen() {
				oneDeviceOpen = true
			}
		}
		if !oneDeviceOpen {
			log.Warnf("No keyboard device could be opened:")
			for i, device := range keyboardDevices {
				log.Warnf("Device %d: %s: %s", i+1, device.DeviceName(), device.LastOpenError())
			}
			time.Sleep(10 * time.Second)
		}

		// handle mouse movement and scrolling
		moveX := 0.0
		moveY := 0.0
		scrollX := 0.0
		scrollY := 0.0
		speedFactor := 1.0
		for code, binding := range currentLayer.Bindings {
			if tapHoldHandler.IsKeyPressed(code) {
				switch t := binding.(type) {
				case SpeedBinding:
					speedFactor *= t.Speed
				case ScrollBinding:
					scrollX += t.X
					scrollY += t.Y
				case MoveBinding:
					moveX += t.X
					moveY += t.Y
				}
			}
		}

		if moveX != 0 || moveY != 0 || scrollX != 0 || scrollY != 0 || mouse.IsMoving() {
			tickTime := mouseLoopInterval.Seconds()
			moveSpeed := config.BaseMouseSpeed * tickTime
			scrollSpeed := config.BaseScrollSpeed * tickTime
			accelerationStep := tickTime * 1000 / config.MouseAccelerationTime
			decelerationStep := tickTime * 1000 / config.MouseDecelerationTime
			mouse.Scroll(scrollX*scrollSpeed*speedFactor, scrollY*scrollSpeed*speedFactor)
			mouse.Move(
				moveX*moveSpeed, moveY*moveSpeed, config.StartMouseSpeed*tickTime,
				config.BaseMouseSpeed*tickTime,
				config.MouseAccelerationCurve,
				accelerationStep,
				config.MouseDecelerationCurve,
				decelerationStep,
				speedFactor,
			)
			mouseTimer = time.NewTimer(mouseLoopInterval)
		} else {
			mouseTimer = time.NewTimer(math.MaxInt64)
		}
	}
}

// handleKey handles a single key event (press or release).
func handleKey(event *KeyboardEvent) {
	binding, _ := currentLayer.Bindings[event.code]

	// switch to first layer on escape, if not mapped to something else
	if binding == nil && event.code == evdev.KEY_ESC && event.isPress && currentLayer != config.Layers[0] {
		binding = LayerBinding{BaseBinding{}, config.Layers[0].Name}
	}

	// use the wildcard binding if no binding is defined for the key
	if binding == nil && currentLayer.WildcardBinding != nil {
		binding = currentLayer.WildcardBinding
	}

	// if there is no wildcard either and pass through is enabled, insert a KeyBinding
	if binding == nil && currentLayer.PassThrough {
		binding = KeyBinding{KeyCombo: []uint16{event.code}}
	}

	// go back to the previous layer when toggleLayerKey is released
	if !event.isPress {
		for i, key := range toggleLayerKeys {
			if key == event.code {
				currentLayer = toggleLayerPrevious[i]
				log.Debugf("Switching to layer %v", currentLayer.Name)
				// all layers that have been toggled after the current one are removed as well
				toggleLayerKeys = toggleLayerKeys[:i]
				toggleLayerPrevious = toggleLayerPrevious[:i]
				break
			}
		}
	}

	// inform the keyboard and mouse about key releases
	if !event.isPress {
		keyboard.OriginalKeyUp(event.code)
		mouse.OriginalKeyUp(event.code)
	}

	executeBinding(event, binding)
}

// executeBinding does what needs to be done for the given binding.
// For some bindings there is nothing that needs to be done, e.g. for the speed
// and move bindings.
// For tap-hold bindings, either the tap or the hold binding is executed.
func executeBinding(event *KeyboardEvent, binding interface{}) {
	log.Debugf("Executing %T: %+v", binding, binding)

	switch t := binding.(type) {
	case MultiBinding:
		for _, b := range t.Bindings {
			executeBinding(event, b)
		}
	case TapHoldBinding:
		if event.holdKey {
			executeBinding(event, t.HoldBinding)
		} else {
			executeBinding(event, t.TapBinding)
		}
	case LayerBinding:
		if event.isPress {
			// deactivate any toggled layers
			if toggleLayerPrevious != nil {
				toggleLayerKeys = nil
				toggleLayerPrevious = nil
			}
			for _, layer := range config.Layers {
				if layer.Name == t.Layer {
					log.Debugf("Switching to layer %v", layer.Name)
					currentLayer = layer
					break
				}
			}
		}
	case ToggleLayerBinding:
		if event.isPress {
			for _, layer := range config.Layers {
				if layer.Name == t.Layer {
					log.Debugf("Switching to layer %v", layer.Name)
					toggleLayerKeys = append(toggleLayerKeys, event.code)
					toggleLayerPrevious = append(toggleLayerPrevious, currentLayer)
					currentLayer = layer
					break
				}
			}
		}
	case ReloadConfigBinding:
		if event.isPress {
			loadConfig()
		}
	case KeyBinding:
		if event.isPress {
			// replace any wildcard with the key that was pressed
			keys := make([]uint16, len(t.KeyCombo))
			copy(keys, t.KeyCombo)
			for i, key := range keys {
				if key == WildcardKey {
					keys[i] = event.code
				}
			}
			keyboard.PressKeys(event.code, keys)
		}
	case ButtonBinding:
		if event.isPress {
			mouse.ButtonPress(event.code, t.Button)
		}
	case ExecBinding:
		// exec
		if event.isPress {
			log.Debugf("Executing: %s", t.Command)
			cmd := exec.Command("sh", "-c", t.Command)
			// pass the pressed key as environment variable
			cmd.Env = append(
				os.Environ(),
				fmt.Sprintf("key=%d", keyAliasesReversed[event.code]),
				fmt.Sprintf("key_code=%d", event.code),
			)
			err := cmd.Run()
			if err != nil {
				log.Warnf("Execution of command failed: %v", err)
			}
		}
	}
}

// findKeyboardDevices finds all available keyboard input devices.
func findKeyboardDevices() []*evdev.InputDevice {
	var devices []*evdev.InputDevice
	devices, _ = evdev.ListInputDevices("/dev/input/event*")

	// filter out the keyboard devices that have at least an A key or a 1 key
	var keyboardDevices []*evdev.InputDevice
	for _, dev := range devices {
		for capType, codes := range dev.Capabilities {
			if capType.Type == evdev.EV_KEY {
				for _, code := range codes {
					if code.Code == evdev.KEY_A || code.Code == evdev.KEY_KP1 {
						keyboardDevices = append(keyboardDevices, dev)
						break
					}
				}
			}
		}
	}

	// print the keyboard devices
	log.Debugf("Auto detected keyboard devices:")
	for _, dev := range keyboardDevices {
		log.Debugf("- %s: %s\n", dev.Fn, dev.Name)
	}
	return keyboardDevices
}

func exitError(err error, msg string) {
	if err != nil {
		log.Errorf(msg+": %v", err)
	} else {
		log.Error(msg)
	}
	log.Error("Exiting")
	os.Exit(1)
}
