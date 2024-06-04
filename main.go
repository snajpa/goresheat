package main

import (
	"bufio"
	"encoding/csv"
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

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type CPUUsage struct {
	User float64
	Sys  float64
	Idle float64
}

type CPUCore struct {
	Node  int
	HTID  int
	Usage CPUUsage
}

var (
	usageHistory  = make([]string, 0)
	historyMutex  sync.Mutex
	clients       = make(map[*websocket.Conn]bool)
	clientsMutex  sync.Mutex
	previousUsage []CPUCore
	interval      time.Duration
	host          string
	port          string
	historyLength int
	rectSize      = 20
	cpuCores      = make(map[int][]CPUCore) // Map of NUMA nodes to cores
	numCPUs       int
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

	w.Header().Set("Content-Type", "text/csv")
	writer := csv.NewWriter(w)
	defer writer.Flush()

	for _, record := range usageHistory {
		writer.Write(strings.Split(record, ","))
	}
}

// broadcastUsage sends the current CPU usage to all connected clients.
func broadcastUsage() {
	for {
		time.Sleep(interval)

		currentUsage, err := readCPUUsage()
		if err != nil {
			fmt.Println("Error reading CPU usage:", err)
			return
		}

		percentageUsage := calculatePercentage(currentUsage, previousUsage)
		previousUsage = currentUsage

		// Get current time with milliseconds
		currentTime := time.Now().Format("15:04:05.000")

		// Convert usage to CSV format using csv.Writer
		var csvData [][]string
		header := []string{currentTime, strconv.Itoa(numCPUs)}

		// Add user percentage values
		for _, core := range percentageUsage {
			header = append(header, strconv.FormatFloat(core.Usage.User, 'f', 2, 64))
		}
		header = append(header, "", "") // background-colored spaces

		// Add sys percentage values
		for _, core := range percentageUsage {
			header = append(header, strconv.FormatFloat(core.Usage.Sys, 'f', 2, 64))
		}
		header = append(header, "", "") // background-colored spaces

		// Add idle percentage values (inverted)
		for _, core := range percentageUsage {
			header = append(header, strconv.FormatFloat(core.Usage.Idle, 'f', 2, 64))
		}

		csvData = append(csvData, header)

		var csvString strings.Builder
		writer := csv.NewWriter(&csvString)
		writer.WriteAll(csvData)
		writer.Flush()

		csvOutput := csvString.String()

		// Store the usage in history
		historyMutex.Lock()
		usageHistory = append([]string{strings.TrimSpace(csvOutput)}, usageHistory...)
		if len(usageHistory) > historyLength { // Limit history to historyLength entries
			usageHistory = usageHistory[:historyLength]
		}
		historyMutex.Unlock()

		// Broadcast to all clients
		clientsMutex.Lock()
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, []byte(csvOutput))
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

	htmlFileContent, err := os.ReadFile("index.html")
	if err != nil {
		fmt.Println("Error reading index.html:", err)
		return
	}
	htmlContent := string(htmlFileContent)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		intervalMsec := interval.Milliseconds()
		fmt.Fprintf(w, htmlContent, rectSize, historyLength, intervalMsec)
	})
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/history", handleHistory)
	go broadcastUsage()
	address := fmt.Sprintf("%s:%s", host, port)
	fmt.Println("Listening on", address)
	http.ListenAndServe(address, nil)
}
