package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	d2r2log "github.com/d2r2/go-logger"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/aluedtke7/dew_point_fan/display"
	"github.com/aluedtke7/dew_point_fan/lcd"
	"github.com/aluedtke7/go-dht"
	"github.com/antigloss/go/logger"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/host/v3"
)

var (
	disp           display.Display
	lcdDelayPtr    *int
	scrollSpeedPtr *int
	ipAddress      string
	homePath       string
	isAlive        bool
	lg             = d2r2log.NewPackageLogger("main", d2r2log.InfoLevel)
	cycleUpdate    string
	remoteOverride int
)

const (
	DIFF_MIN         = 3.0    // minimal dew point difference
	HYSTERESIS       = 1.0    // difference between switching on/off
	HUM_INSIDE_MIN   = 50.0   // minimal inside humidity, to have an active venting
	TEMP_INSIDE_MIN  = 10.0   // minimal inside temperatur, to have an active venting
	TEMP_OUTSIDE_MIN = -10.0  // minimal outside temperatur, to have an active venting
	DEF_TEMP         = -200.0 // default temperatur
	DEF_HUM          = -1.0   // default humidity
	DATE_TIME_FORMAT = "2006-01-02 15:04:05"
)

type sensorData struct {
	Name        string  `json:"name"`
	Temperature float32 `json:"temperature"`
	Humidity    float32 `json:"humidity"`
	DewPoint    float32 `json:"dew_point"`
}

type info struct {
	Update         string       `json:"update"`
	Sensors        []sensorData `json:"sensors"`
	Venting        bool         `json:"venting"`
	Override       bool         `json:"override"`
	RemoteOverride int          `json:"remote_override"`
	DiffMin        float32      `json:"diff_min"`
	Hysteresis     float32      `json:"hysteresis"`
}

type remoteControl struct {
	Override int `json:"override"`
}

// correction values for temperature
// each sensor is different, find your own correction values!
func getTempCorrections() []float32 {
	return []float32{-4.0, 0.0}
}

// correction values for humidity
// each sensor is different, find your own correction values!
func getHumCorrections() []float32 {
	return []float32{10.0, -6.0}
}

// round float32 to N digits precision
func roundFloat32(val float32, precision uint) float32 {
	ratio := math.Pow(10, float64(precision))
	return float32(math.Round(float64(val)*ratio) / ratio)
}

// helper for error checking
func check(err error) {
	if err != nil {
		lg.Error(errors.Unwrap(fmt.Errorf("wrapped error: %w", err)).Error())
	}
}

// logs the ipv4 addresses found and stores the first non localhost addresses in variable 'ipAddress'
func logNetworkInterfaces() {
	interfaces, err := net.Interfaces()
	if err != nil {
		logger.Error(err.Error())
		return
	}
	reg := regexp.MustCompilePOSIX("^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])")
	for _, i := range interfaces {
		byName, err := net.InterfaceByName(i.Name)
		if err != nil {
			logger.Warn(err.Error())
		}
		err = nil
		addresses, _ := byName.Addrs()
		for _, v := range addresses {
			ipv4 := v.String()
			if reg.MatchString(ipv4) {
				logger.Info(ipv4)
				if strings.Index(ipv4, "127.0.") != 0 {
					idx := strings.Index(ipv4, "/")
					if idx > 0 {
						ipAddress = ipv4[0:idx]
					} else {
						ipAddress = ipv4
					}
				}
			}
		}
	}
}

func printLine(line int, text string, scroll bool) {
	t := strings.TrimSpace(text)
	disp.PrintLine(line, t, scroll)
}

func getHomeDir() string {
	usr, err := user.Current()
	if err != nil {
		return "~/"
	}
	return usr.HomeDir
}

func calcDewPoint(t, r float32) float32 {

	var a, b float64
	t64 := float64(t)
	r64 := float64(r)

	if t64 >= 0 {
		a = 7.5
		b = 237.3
	} else if t64 < 0 {
		a = 7.6
		b = 240.7
	}

	// saturation vapor pressure in hPa
	sdd := 6.1078 * math.Pow(10, (a*t64)/(b+t64))

	// vapor pressure in hPa
	dd := sdd * (r64 / 100)

	// v parameter
	v := math.Log10(dd / 6.1078)

	// dew point temperature (°C)
	tt := (b * v) / (a - v)
	return float32(tt)
}

func showIpAndOverride(msg string) {
	ofs := 17 - len(ipAddress)
	spacer := strings.Repeat(" ", ofs)
	if ofs > 0 {
		alive := " "
		if isAlive {
			alive = "*"
		}
		if ofs > 4 {
			spacer = fmt.Sprintf(" %s %d %s", alive, remoteOverride, strings.Repeat(" ", ofs-5))
		} else if ofs > 2 {
			spacer = fmt.Sprintf(" %s %s", alive, strings.Repeat(" ", ofs-3))
		} else {
			spacer = fmt.Sprintf("%s%s", alive, strings.Repeat(" ", ofs-1))
		}
	}
	printLine(3, ipAddress+spacer+msg, false)
}

func main() {
	defer func() {
		_ = d2r2log.FinalizeLogger()
	}()
	isAlive = false
	cycleUpdate = "---"
	remoteOverride = 0      // 0 = not set, 1 = set to ON, 2 = set to OFF
	lastRemoteOverride := 0 // to detect changes and log them

	homePath = filepath.Join(getHomeDir(), ".dew_point_fan")
	_ = os.MkdirAll(homePath, os.ModePerm)
	config := logger.Config{
		LogDir:            filepath.Join(homePath, "log"),
		LogFileMaxSize:    2,
		LogFileMaxNum:     30,
		LogFileNumToDel:   3,
		LogDest:           logger.LogDestBoth,
		LogFilenamePrefix: "dpf",
		LogSymlinkPrefix:  "dpf",
		Flag:              logger.ControlFlagLogDate | logger.ControlFlagLogFuncName,
	}
	_ = logger.Init(&config)
	defer func() {
		if err := recover(); err != nil {
			logger.Error("Panic occurred:", err)
		}
	}()
	logger.Info("Starting Dew Point Fan...")

	_ = d2r2log.ChangePackageLogLevel("dht", d2r2log.ErrorLevel)

	// Commandline parameters
	lcdDelayPtr = flag.Int("lcdDelay", 3, "initial delay for LCD in s (1s...10s)")
	scrollSpeedPtr = flag.Int("scrollSpeed", 500, "scroll speed in ms (100ms...10000ms)")
	flag.Parse()
	if *scrollSpeedPtr < 100 {
		*scrollSpeedPtr = 100
	}
	if *scrollSpeedPtr > 10000 {
		*scrollSpeedPtr = 10000
	}
	if *lcdDelayPtr < 1 {
		*lcdDelayPtr = 1
	}
	if *lcdDelayPtr > 10 {
		*lcdDelayPtr = 10
	}

	var err error
	disp, err = lcd.New(false, *scrollSpeedPtr, *lcdDelayPtr)
	if err != nil {
		logger.Errorf("Couldn't initialize display: %s", err)
	} else {
		ipAddress = ""
		logNetworkInterfaces()
		logger.Infof("IP address: %s", ipAddress)
		disp.Backlight(true)
		printLine(0, "Starting...", false)
		showIpAndOverride("")
	}

	// Load gpio drivers:
	if _, err = host.Init(); err != nil {
		check(err)
	}
	// pin GPIO22 is input for fanIsOn detection (via hardware 3 state switch)
	pin22 := gpioreg.ByName("GPIO22")
	if pin22 == nil {
		log.Fatal("Failed to to find GPIO22")
	}
	// set to floating input pin
	if err = pin22.In(gpio.Float, gpio.NoEdge); err != nil {
		log.Fatal(err)
	}
	// pin GPIO25 is output for fan fanShouldBeOn
	pin25 := gpioreg.ByName("GPIO25")
	if pin25 == nil {
		log.Fatal("Failed to to find GPIO25")
	}
	// initial off value for fan fanShouldBeOn (active low)
	fanShouldBeOn := false
	// last value of fanShouldBeOn state to detect changes for logging purpose
	lastfanShouldBeOn := false
	if err = pin25.Out(gpio.High); err != nil {
		log.Fatal(err)
	}

	// initial off value for manual fanIsOn (3 state switch)
	fanStatus := false
	lastFanStatus := false // to detect changes and log them

	var ctrlChan = make(chan os.Signal, 1)
	signal.Notify(ctrlChan, os.Interrupt, syscall.SIGTERM)
	// this goroutine is waiting for being stopped
	go func() {
		<-ctrlChan
		logger.Info("Ctrl+C received... Exiting")
		os.Exit(1)
	}()

	sensorType := dht.DHT22
	var pins = []int{24, 23}
	var temperatures = []float32{DEF_TEMP, DEF_TEMP}
	var humidities = []float32{DEF_HUM, DEF_HUM}
	var dewpoints = []float32{0.0, 0.0}
	var lastDewpoints = []float32{0.0, 0.0}
	var retried = []int{0, 0}
	var retries = 15
	var venting = "---"
	var fanIsOn = "---"

	// load token from environment
	token, _ := os.LookupEnv("INFLUX_DP_TOKEN")
	logger.Infof("InfluxDB token: %s", token)
	url, _ := os.LookupEnv("INFLUX_SRV_URL")
	logger.Infof("Influx srv url: %s", url)
	client := influxdb2.NewClient(url, token)
	org := "privat"
	bucket := "dew-point"
	writeAPI := client.WriteAPIBlocking(org, bucket)

	// a little http server to show current values
	go func() {
		// browser page plain text
		webHandler := func(w http.ResponseWriter, req *http.Request) {
			_, _ = fmt.Fprintf(w, "Dew Point Fan                     %s\n"+
				"-----------------------------------------------------\n"+
				"Inside:  DP: %6.1f, Temp: %5.1f°C, Humidity: %5.1f%%\n"+
				"Outside: DP: %6.1f, Temp: %5.1f°C, Humidity: %5.1f%%\n"+
				"Fan should be %s                         Fan is %s",
				cycleUpdate,
				dewpoints[0], temperatures[0], humidities[0],
				dewpoints[1], temperatures[1], humidities[1],
				venting, fanIsOn,
			)
		}
		http.HandleFunc("/", webHandler)

		// data in JSON format
		infoHandler := func(w http.ResponseWriter, req *http.Request) {
			if req.Method == "GET" {
				inf := new(info)
				inf.Update = cycleUpdate
				inf.Sensors = []sensorData{
					{"Inside", temperatures[0], humidities[0], dewpoints[0]},
					{"Outside", temperatures[1], humidities[1], dewpoints[1]},
				}
				inf.Venting = fanShouldBeOn
				inf.Override = fanShouldBeOn != fanStatus
				inf.RemoteOverride = remoteOverride
				inf.DiffMin = DIFF_MIN
				inf.Hysteresis = HYSTERESIS
				j, _ := json.MarshalIndent(inf, "", "  ")
				_, _ = w.Write(j)
			}
		}
		http.HandleFunc("/info", infoHandler)

		// POST handler for changing fanIsOn
		overrideHandler := func(w http.ResponseWriter, req *http.Request) {
			if req.Method == "POST" {
				lg.Info("POST API called")
				decoder := json.NewDecoder(req.Body)
				remote := &remoteControl{}
				err := decoder.Decode(remote)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
				remoteOverride = remote.Override
				j, _ := json.MarshalIndent(remote, "", "  ")
				_, _ = w.Write(j)
			}
		}
		http.HandleFunc("/override", overrideHandler)
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	for {
		readingsGood := true
		location := ""
		for i := 0; i < len(pins); i++ {
			if i == 0 {
				location = "I"
			} else {
				location = "O"
			}
			// Read DHT sensor data from specific pin, retrying several times in case of failure.
			temperatures[i], humidities[i], retried[i], err = dht.ReadDHTxxWithRetry(sensorType, pins[i], false, retries)
			if err != nil {
				printLine(i, fmt.Sprintf("%s: retried %d", location, retried[i]), false)
				readingsGood = false
			} else {
				temperatures[i] = roundFloat32(temperatures[i]+getTempCorrections()[i], 1)
				humidities[i] = roundFloat32(humidities[i]+getHumCorrections()[i], 1)
				// print temperature and humidity on LCD
				printLine(i, fmt.Sprintf("%s-T:%5.1fC H:%5.1f%%", location, temperatures[i], humidities[i]), false)
			}
			if temperatures[i] > DEF_TEMP && humidities[i] > DEF_HUM {
				if temperatures[i] < -20 || temperatures[i] > 40 {
					logger.Warnf("%s: temperature is out of range: %5.1f°C", location, temperatures[i])
					readingsGood = false
				} else {
					dewpoints[i] = roundFloat32(calcDewPoint(temperatures[i], humidities[i]), 1)
					lg.Infof("%s: Dewpoint =%5.1f, Temperature =%5.1f°C, Humidity =%5.1f%% (retried %d times)",
						location, dewpoints[i], temperatures[i], humidities[i], retried[i])
				}
			}
		}
		if readingsGood {
			// check for spike/false values and skip them
			if math.Abs(float64(dewpoints[0])-float64(lastDewpoints[0])) > 1 ||
				math.Abs(float64(dewpoints[1])-float64(lastDewpoints[1])) > 1 {
				logger.Warn("Deviation between dew points is too high!")
			} else {
				deltaTP := dewpoints[0] - dewpoints[1]
				if deltaTP > (DIFF_MIN + HYSTERESIS) {
					fanShouldBeOn = true
				}
				if deltaTP < DIFF_MIN {
					fanShouldBeOn = false
				}
				if temperatures[0] < TEMP_INSIDE_MIN {
					fanShouldBeOn = false
				}
				if temperatures[1] < TEMP_OUTSIDE_MIN {
					fanShouldBeOn = false
				}
				// no venting when inside humidity is below threshold
				if humidities[0] < HUM_INSIDE_MIN {
					fanShouldBeOn = false
				}
				if fanShouldBeOn {
					venting = "on"
				} else {
					venting = "off"
				}
				printLine(2, fmt.Sprintf("DP:%5.1fC %5.1fC %s", dewpoints[0], dewpoints[1], venting), false)

				// prepare data for InfuxDb and send it
				tags := map[string]string{
					// "manual_override": strconv.FormatBool(fanStatus),
					// "remote_override": strconv.Itoa(remoteOverride),
					// "venting":         strconv.FormatBool(fanShouldBeOn),
				}
				ventingValue := 0
				if fanShouldBeOn {
					ventingValue = 1
				}
				fields := map[string]interface{}{
					"temp_i":     temperatures[0],
					"temp_o":     temperatures[1],
					"dewpoint_i": dewpoints[0],
					"dewpoint_o": dewpoints[1],
					"hum_i":      humidities[0],
					"hum_o":      humidities[1],
					"retry_i":    retried[0],
					"retry_o":    retried[1],
					"vent_val":   ventingValue,
				}
				point := write.NewPoint("dp", tags, fields, time.Now())
				if err := writeAPI.WritePoint(context.Background(), point); err != nil {
					logger.Error(err)
				}
			}
			lastDewpoints[0] = dewpoints[0]
			lastDewpoints[1] = dewpoints[1]
		}

		if remoteOverride > 0 {
			if remoteOverride == 1 {
				fanShouldBeOn = true
			} else {
				fanShouldBeOn = false
			}
		}
		// here we set the value for the fan relais (active low)
		if fanShouldBeOn {
			err = pin25.Out(gpio.Low)
		} else {
			err = pin25.Out(gpio.High)
		}
		if err != nil {
			logger.Error(err)
		}

		isAlive = !isAlive
		// here we read the value of the fan relais, to detect a manual (switch) override
		if pin22.Read() {
			fanIsOn = "OFF"
			fanStatus = false
		} else {
			fanIsOn = "ON "
			fanStatus = true
		}
		//logger.Infof("Test: fanShouldBeOn is %t, fanIsOn is %s, fan status is %t", fanShouldBeOn, fanIsOn, fanStatus)
		showIpAndOverride(fanIsOn)
		if fanShouldBeOn != lastfanShouldBeOn || fanStatus != lastFanStatus || remoteOverride != lastRemoteOverride {
			logger.Infof("Venting change: new state is %t, fan status %t, remote fanIsOn %d", fanShouldBeOn, fanStatus, remoteOverride)
		}
		lastfanShouldBeOn = fanShouldBeOn
		lastFanStatus = fanStatus
		lastRemoteOverride = remoteOverride
		lg.Infof("Fan is %s - %s", venting, fanIsOn)
		cycleUpdate = time.Now().Format(DATE_TIME_FORMAT)
		time.Sleep(15000 * time.Millisecond)
	}
}
