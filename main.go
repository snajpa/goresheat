package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed index.html
var HtmlContent string

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type CPUUsage struct {
	User float64 `json:"user"`
	Sys  float64 `json:"sys"`
	Idle float64 `json:"idle"`
}

type CPUCore struct {
	Node  int      `json:"node"`
	HTID  int      `json:"htid"`
	Usage CPUUsage `json:"usage"`
}

type DiskStats struct {
	Device   string `json:"device"`
	ReadIOs  uint64 `json:"read_ios"`
	WriteIOs uint64 `json:"write_ios"`
	IoTime   uint64 `json:"io_time"`
}

var (
	usageHistory  = make([]string, 0)
	historyMutex  sync.Mutex
	clients       = make(map[*websocket.Conn]bool)
	clientsMutex  sync.Mutex
	previousUsage []CPUCore
	prevDiskStats map[string]DiskStats
	interval      time.Duration
	host          string
	port          string
	url           string
	historyLength int
	rectSize      = 20
	cpuCores      = make(map[int][]CPUCore) // Map of NUMA nodes to cores
	numCPUs       int
	numDisks      int
	diskDevices   []string
)

// readCPUUsage reads the CPU usage from the /proc/stat file.
func readCPUUsage() ([]CPUCore, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var cores []CPUCore
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu") && line[3] != ' ' {
			fields := strings.Fields(line)[1:]
			user, _ := strconv.ParseInt(fields[0], 10, 64)
			sys, _ := strconv.ParseInt(fields[2], 10, 64)
			idle, _ := strconv.ParseInt(fields[3], 10, 64)
			cores = append(cores, CPUCore{
				Node:  0, // Will be updated later
				HTID:  0, // Will be updated later
				Usage: CPUUsage{User: float64(user), Sys: float64(sys), Idle: float64(idle)},
			})
		}
	}
	return cores, nil
}

// calculatePercentage calculates the percentage difference in CPU usage between two reads
func calculatePercentage(current, previous []CPUCore) []CPUCore {
	if len(previous) == 0 {
		return current
	}

	var result []CPUCore
	for i := range current {
		total := (current[i].Usage.User + current[i].Usage.Sys + current[i].Usage.Idle) -
			(previous[i].Usage.User + previous[i].Usage.Sys + previous[i].Usage.Idle)

		if total > 0 {
			result = append(result, CPUCore{
				Node: current[i].Node,
				HTID: current[i].HTID,
				Usage: CPUUsage{
					User: (current[i].Usage.User - previous[i].Usage.User) / total * 100,
					Sys:  (current[i].Usage.Sys - previous[i].Usage.Sys) / total * 100,
					Idle: (current[i].Usage.Idle - previous[i].Usage.Idle) / total * 100,
				},
			})
		} else {
			result = append(result, CPUCore{
				Node: current[i].Node,
				HTID: current[i].HTID,
				Usage: CPUUsage{
					User: 0,
					Sys:  0,
					Idle: 0,
				},
			})
		}
	}
	return result
}

// GetPhysicalBlockDevices returns a list of physical block devices (SSD/HDD)
func GetPhysicalBlockDevices() ([]string, error) {
	var devices []string
	sysBlockPath := "/sys/class/block/"
	entries, err := os.ReadDir(sysBlockPath)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vd") || strings.HasPrefix(entry.Name(), "sd") ||
			strings.HasPrefix(entry.Name(), "nvm") || strings.HasPrefix(entry.Name(), "nvme") {
			// Check if not a partition
			if fileExists(sysBlockPath + entry.Name() + "/partition") {
				continue
			}
			devices = append(devices, entry.Name())
		}
	}
	return devices, nil
}

// fileExists checks if a file exists and is not a directory
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// GetDiskStats reads /proc/diskstats and returns the statistics for the specified devices
func GetDiskStats(devices []string) (map[string]DiskStats, error) {
	file, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	stats := make(map[string]DiskStats)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}

		device := fields[2]
		if contains(devices, device) {
			readIOs, _ := strconv.ParseUint(fields[3], 10, 64)
			writeIOs, _ := strconv.ParseUint(fields[7], 10, 64)
			ioTime, _ := strconv.ParseUint(fields[12], 10, 64)
			stats[device] = DiskStats{Device: device, ReadIOs: readIOs, WriteIOs: writeIOs, IoTime: ioTime}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return stats, nil
}

// contains checks if a slice contains a specific string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// CalculateUtilization calculates the %util for each device
func CalculateUtilization(prevStats, currStats map[string]DiskStats, interval float64) map[string]float64 {
	utilization := make(map[string]float64)

	for device, currStat := range currStats {
		if prevStat, ok := prevStats[device]; ok {
			ioTimeDelta := float64(currStat.IoTime - prevStat.IoTime)
			utilization[device] = (ioTimeDelta / (interval * 1000.0)) * 100.0
		}
	}
	return utilization
}

// handleConnections handles incoming WebSocket connections and sends current CPU usage data.
func handleConnections(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Error upgrading to WebSocket:", err)
		return
	}
	defer conn.Close()

	clientsMutex.Lock()
	clients[conn] = true
	clientsMutex.Unlock()

	for {
		time.Sleep(interval)
	}
}

// handleHistory serves the usage history to the clients.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	historyMutex.Lock()
	defer historyMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	writer := json.NewEncoder(w)

	writer.Encode(usageHistory)
}

// broadcastUsage sends the current CPU and disk usage to all connected clients.
func broadcastUsage() {
	skipFirst := true
	for {
		time.Sleep(interval)

		var cpuUsage []CPUCore
		var diskStats map[string]DiskStats
		var errCPU, errDisk error
		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()
			cpuUsage, errCPU = readCPUUsage()
		}()

		go func() {
			defer wg.Done()
			diskStats, errDisk = GetDiskStats(diskDevices)
		}()

		wg.Wait()

		if errCPU != nil {
			fmt.Println("Error reading CPU usage:", errCPU)
			return
		}

		if errDisk != nil {
			fmt.Println("Error reading disk stats:", errDisk)
			return
		}

		percentageUsage := calculatePercentage(cpuUsage, previousUsage)
		previousUsage = cpuUsage

		diskUtilization := CalculateUtilization(prevDiskStats, diskStats, interval.Seconds())
		prevDiskStats = diskStats

		usageData := map[string]interface{}{
			"num_cpu":   numCPUs,
			"num_disks": numDisks,
			"cpu_user":  []float64{},
			"cpu_sys":   []float64{},
			"cpu_idle":  []float64{},
			"disk":      []float64{},
			"time":      time.Now().Format("15:04:05.000"),
		}

		for _, core := range percentageUsage {
			usageData["cpu_user"] = append(usageData["cpu_user"].([]float64), core.Usage.User)
			usageData["cpu_sys"] = append(usageData["cpu_sys"].([]float64), core.Usage.Sys)
			usageData["cpu_idle"] = append(usageData["cpu_idle"].([]float64), core.Usage.Idle)
		}

		for _, device := range diskDevices {
			util := diskUtilization[device]
			usageData["disk"] = append(usageData["disk"].([]float64), util)
		}

		jsonOutput, err := json.Marshal(usageData)
		if err != nil {
			fmt.Println("Error marshaling JSON:", err)
			return
		}

		// Store the usage in history
		historyMutex.Lock()
		usageHistory = append([]string{string(jsonOutput)}, usageHistory...)
		if len(usageHistory) > historyLength { // Limit history to historyLength entries
			usageHistory = usageHistory[:historyLength]
		}
		historyMutex.Unlock()

		if skipFirst {
			skipFirst = false
			continue
		}

		// Broadcast to all clients
		clientsMutex.Lock()
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, jsonOutput)
			if err != nil {
				client.Close()
				delete(clients, client)
			}
		}
		clientsMutex.Unlock()
	}
}

// getCPUInfo reads the CPU topology from /proc/cpuinfo and populates the cpuCores map.
func getCPUInfo() error {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var currentCore CPUCore
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "processor") {
			fields := strings.Fields(line)
			processorID, _ := strconv.Atoi(fields[2])
			currentCore = CPUCore{HTID: processorID}
			numCPUs++
		} else if strings.HasPrefix(line, "physical id") {
			fields := strings.Fields(line)
			nodeID, _ := strconv.Atoi(fields[3])
			currentCore.Node = nodeID
		} else if line == "" {
			cpuCores[currentCore.Node] = append(cpuCores[currentCore.Node], currentCore)
		}
	}
	return nil
}

func main() {
	flag.DurationVar(&interval, "interval", 100*time.Millisecond, "Update interval")
	flag.StringVar(&url, "url", "", "By which URL is this service accessible")
	flag.StringVar(&host, "host", "0.0.0.0", "Host address")
	flag.StringVar(&port, "port", "8080", "Port number")
	flag.IntVar(&historyLength, "history", 150, "History length")
	flag.IntVar(&rectSize, "rectsize", 9, "Rectangle size")
	flag.Parse()

	err := getCPUInfo()
	if err != nil {
		fmt.Println("Error reading CPU info:", err)
		return
	}

	diskDevices, err = GetPhysicalBlockDevices()
	if err != nil {
		fmt.Println("Error getting block devices:", err)
		return
	}
	numDisks = len(diskDevices)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		intervalMsec := interval.Milliseconds()
		numDataRows := 3 + 3*numCPUs + numDisks + 3*2
		urlParam := "default"
		if url != "" {
			urlParam = url
		}
		fmt.Fprintf(w, HtmlContent, rectSize, historyLength, numDataRows, intervalMsec, urlParam)
	})
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/history", handleHistory)
	go broadcastUsage()
	address := fmt.Sprintf("%s:%s", host, port)
	fmt.Println("Listening on", address)
	http.ListenAndServe(address, nil)
}
